// Package editrec captures per-edit transcripts: every agent tool call plus
// before/after snapshots of any file the agent mutated. Transcripts are
// flushed to S3 at `_edits/{slug}/{timestamp}-{logkey}.json` for forensic
// inspection from the Debug viewer.
//
// The live SSE event stream is ephemeral; once the browser tab closes there's
// no record of what the agent actually did. This package is that durable
// record.
package editrec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/store"
)

// Prefix is the bucket-level prefix transcripts live under. Sits outside any
// user-slug namespace because slugs cannot start with `_` (validateSlug), so
// transcripts can never collide with a real site or be served by the
// subdomain proxy.
const Prefix = "_edits/"

const contentType = "application/json"

const (
	// MaxContentBytes caps each before/after content blob captured per file
	// mutation. A 256 KiB site cap × many ops would otherwise blow the
	// transcript JSON well past the per-edit budget; for anything bigger,
	// store head + tail plus sha256 + size only.
	MaxContentBytes = 64 * 1024
	// MaxTranscriptBytes is the hard upper bound on the marshaled transcript.
	// Anything larger is dropped with a warning rather than written, so a
	// runaway agent loop can't bloat the bucket.
	MaxTranscriptBytes = 1 << 20
)

// mutators is the set of tools the recorder snapshots before/after content
// for. All other tools (read_file, list_files, grep_files, list_assets,
// read_attachment, fetch_reference, list_functions, read_function) are
// recorded in the tool-call timeline but don't produce file-change entries.
var mutators = map[string]bool{
	"write_file":      true,
	"edit_file":       true,
	"replace_lines":   true,
	"insert_at_line":  true,
	"write_function":  true,
	"edit_function":   true,
	"delete_function": true,
}

// Transcript is the on-disk JSON shape. Versioned implicitly by additive,
// optional fields — older transcripts deserialize fine.
//
// Model + ReasoningEffort capture which LLM config produced this run.
// Useful when comparing two transcripts for "why did this build come out
// different from the last one?" — was it the model, the reasoning level,
// or something else entirely?
type Transcript struct {
	Slug            string       `json:"slug"`
	LogKey          string       `json:"log_key"`
	StartedAt       time.Time    `json:"started_at"`
	FinishedAt      time.Time    `json:"finished_at,omitempty"`
	Model           string       `json:"model,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
	Template        string       `json:"template,omitempty"`
	UserPrompt      string       `json:"user_prompt,omitempty"`
	Page            string       `json:"page,omitempty"`
	SelectionLen    int          `json:"selection_len,omitempty"`
	FinalStatus     string       `json:"final_status,omitempty"`
	Error           string       `json:"error,omitempty"`
	Usage           Usage        `json:"usage,omitempty"`
	ToolCalls       []ToolCall   `json:"tool_calls"`
	FileChanges     []FileChange `json:"file_changes"`
}

// Usage is the per-run token tally summed across every agent turn that fed
// this transcript — the initial author run plus any lint-fix retries. Mirrors
// agent.Usage as a stable on-disk shape (editrec is a persistence package and
// deliberately does not import the agent runtime). Cached vs Prompt is the
// signal that tells whether the cache-stable instruction prefix is being
// reused across builds.
type Usage struct {
	Prompt     int64 `json:"prompt_tokens,omitempty"`
	Cached     int64 `json:"cached_tokens,omitempty"`
	Candidates int64 `json:"candidates_tokens,omitempty"`
	Thoughts   int64 `json:"thoughts_tokens,omitempty"`
	ToolUse    int64 `json:"tool_use_tokens,omitempty"`
	Total      int64 `json:"total_tokens,omitempty"`
	Responses  int   `json:"responses,omitempty"`
}

// ToolCall is one start/done/error event in the agent run.
type ToolCall struct {
	Timestamp time.Time `json:"ts"`
	Tool      string    `json:"tool"`
	Phase     string    `json:"phase"`
	Path      string    `json:"path,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// FileChange is one before/after pair for a single mutator invocation. Two
// calls to write_file on the same path produce two FileChange entries so the
// timeline preserves intermediate states (helpful when an agent loops).
type FileChange struct {
	Index           int    `json:"index"`
	ToolCallIndex   int    `json:"tool_call_index"`
	Tool            string `json:"tool"`
	Path            string `json:"path"`
	BeforeSize      int    `json:"before_size"`
	BeforeSHA256    string `json:"before_sha256,omitempty"`
	BeforeContent   string `json:"before_content,omitempty"`
	BeforeTruncated bool   `json:"before_truncated,omitempty"`
	AfterSize       int    `json:"after_size"`
	AfterSHA256     string `json:"after_sha256,omitempty"`
	AfterContent    string `json:"after_content,omitempty"`
	AfterTruncated  bool   `json:"after_truncated,omitempty"`
}

// Recorder accumulates a Transcript across one agent run. Wrap exposes a
// single emit callback that's safe to call from the runner's tool callbacks;
// internal state is mutex-guarded so a future move to parallel tool calls
// wouldn't corrupt the transcript.
type Recorder struct {
	mu             sync.Mutex
	transcript     Transcript
	pendingBefores []pendingBefore
	pendingIdx     map[string]int // path -> index into pendingBefores
}

type pendingBefore struct {
	tool      string
	path      string
	content   string
	toolStart int
}

// New starts a recorder for one agent run. SelectionLen is the byte length of
// the user's visual-editor selection HTML (0 for non-visual edits).
func New(slug, logKey, userPrompt, page string, selectionLen int) *Recorder {
	return &Recorder{
		transcript: Transcript{
			Slug:         slug,
			LogKey:       logKey,
			StartedAt:    time.Now().UTC(),
			UserPrompt:   userPrompt,
			Page:         page,
			SelectionLen: selectionLen,
			ToolCalls:    []ToolCall{},
			FileChanges:  []FileChange{},
		},
		pendingIdx: map[string]int{},
	}
}

// SetModel stamps the recorder with the LLM model string and reasoning
// effort the run used. Separate from New so existing call sites (and the
// tests that don't care about the model) keep their current signature,
// and so the caller can attach the model info even when it isn't known
// at construction time.
func (r *Recorder) SetModel(model, reasoningEffort string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transcript.Model = model
	r.transcript.ReasoningEffort = reasoningEffort
}

// SetTemplate stamps the recorder with the site-template id this run used
// (e.g. "landing-page"). Separate from New so non-template callers and tests
// keep their signature, matching the SetModel pattern.
func (r *Recorder) SetTemplate(template string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transcript.Template = template
}

// AddUsage folds one agent run's token tally into the transcript total. Called
// once per run (author plus each lint-fix retry), so the recorded figure is
// the full cost of producing the site, not just the last turn.
func (r *Recorder) AddUsage(u Usage) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.transcript.Usage.Prompt += u.Prompt
	r.transcript.Usage.Cached += u.Cached
	r.transcript.Usage.Candidates += u.Candidates
	r.transcript.Usage.Thoughts += u.Thoughts
	r.transcript.Usage.ToolUse += u.ToolUse
	r.transcript.Usage.Total += u.Total
	r.transcript.Usage.Responses += u.Responses
}

// Wrap returns an emit callback that records each event into the transcript
// (reading before/after content for mutator tools) and forwards every event
// to downstream. ctx and st are captured for the duration of the run; cancel
// the parent context to stop store reads.
func (r *Recorder) Wrap(ctx context.Context, st *store.Store, slug string, downstream func(events.Event)) func(events.Event) {
	return func(ev events.Event) {
		r.observe(ctx, st, slug, ev)
		if downstream != nil {
			downstream(ev)
		}
	}
}

func (r *Recorder) observe(ctx context.Context, st *store.Store, slug string, ev events.Event) {
	if ev.Type != events.TypeTool {
		return
	}

	r.mu.Lock()
	tc := ToolCall{
		Timestamp: time.Now().UTC(),
		Tool:      ev.Tool,
		Phase:     ev.Phase,
		Path:      ev.Path,
		Message:   ev.Message,
	}
	r.transcript.ToolCalls = append(r.transcript.ToolCalls, tc)
	toolIdx := len(r.transcript.ToolCalls) - 1
	r.mu.Unlock()

	if !mutators[ev.Tool] || ev.Path == "" {
		return
	}

	switch ev.Phase {
	case events.PhaseStart:
		before := readContent(ctx, st, slug, ev.Path)
		r.mu.Lock()
		idx := len(r.pendingBefores)
		r.pendingBefores = append(r.pendingBefores, pendingBefore{
			tool:      ev.Tool,
			path:      ev.Path,
			content:   before,
			toolStart: toolIdx,
		})
		r.pendingIdx[ev.Path] = idx
		r.mu.Unlock()
	case events.PhaseDone:
		r.mu.Lock()
		idx, ok := r.pendingIdx[ev.Path]
		if !ok {
			r.mu.Unlock()
			return
		}
		delete(r.pendingIdx, ev.Path)
		pre := r.pendingBefores[idx]
		r.mu.Unlock()

		after := readContent(ctx, st, slug, ev.Path)

		r.mu.Lock()
		r.appendChange(pre, after, toolIdx)
		r.mu.Unlock()
	case events.PhaseError:
		r.mu.Lock()
		delete(r.pendingIdx, ev.Path)
		r.mu.Unlock()
	}
}

func readContent(ctx context.Context, st *store.Store, slug, p string) string {
	obj, err := st.Read(ctx, slug, p)
	if err != nil {
		slog.Warn("editrec.read_failed", "slug", slug, "path", p, "err", err)
		return ""
	}
	if obj == nil {
		return ""
	}
	return obj.Content
}

// appendChange must be called with r.mu held.
func (r *Recorder) appendChange(pre pendingBefore, afterContent string, doneIdx int) {
	fc := FileChange{
		Index:         len(r.transcript.FileChanges),
		ToolCallIndex: doneIdx,
		Tool:          pre.tool,
		Path:          pre.path,
	}
	fc.BeforeSize = len(pre.content)
	if fc.BeforeSize > 0 {
		h := sha256.Sum256([]byte(pre.content))
		fc.BeforeSHA256 = hex.EncodeToString(h[:])
		fc.BeforeContent, fc.BeforeTruncated = truncate(pre.content)
	}
	fc.AfterSize = len(afterContent)
	if fc.AfterSize > 0 {
		h := sha256.Sum256([]byte(afterContent))
		fc.AfterSHA256 = hex.EncodeToString(h[:])
		fc.AfterContent, fc.AfterTruncated = truncate(afterContent)
	}
	r.transcript.FileChanges = append(r.transcript.FileChanges, fc)
}

// truncate returns at most MaxContentBytes by keeping a head and tail slice
// joined by a marker. Used when capturing very large files so the transcript
// stays bounded.
func truncate(s string) (string, bool) {
	if len(s) <= MaxContentBytes {
		return s, false
	}
	head := MaxContentBytes / 2
	tail := MaxContentBytes - head
	dropped := len(s) - MaxContentBytes
	var b strings.Builder
	b.Grow(MaxContentBytes + 64)
	b.WriteString(s[:head])
	fmt.Fprintf(&b, "\n\n... [truncated %d bytes] ...\n\n", dropped)
	b.WriteString(s[len(s)-tail:])
	return b.String(), true
}

// Finish marshals the transcript and writes it to S3. Best-effort: marshal
// or write failures are logged and swallowed — losing a transcript must not
// fail the build that produced it.
func (r *Recorder) Finish(ctx context.Context, st *store.Store, finalStatus string, finalErr error) {
	if r == nil || st == nil {
		return
	}
	r.mu.Lock()
	r.transcript.FinishedAt = time.Now().UTC()
	r.transcript.FinalStatus = finalStatus
	if finalErr != nil {
		r.transcript.Error = finalErr.Error()
	}
	slug := r.transcript.Slug
	logKey := r.transcript.LogKey
	startedAt := r.transcript.StartedAt
	body, err := json.Marshal(r.transcript)
	toolCount := len(r.transcript.ToolCalls)
	changeCount := len(r.transcript.FileChanges)
	r.mu.Unlock()

	if err != nil {
		slog.Warn("editrec.marshal_failed", "slug", slug, "err", err)
		return
	}
	if len(body) > MaxTranscriptBytes {
		slog.Warn("editrec.too_large", "slug", slug, "bytes", len(body), "cap", MaxTranscriptBytes)
		return
	}
	key := Key(slug, startedAt, logKey)
	err = st.WriteRaw(ctx, key, string(body), contentType, nil)
	if err != nil {
		slog.Warn("editrec.write_failed", "slug", slug, "key", key, "err", err)
		return
	}
	slog.Info("editrec.write", "slug", slug, "key", key, "tool_calls", toolCount, "file_changes", changeCount, "bytes", len(body))
}

// Key returns the bucket key for a transcript with the given timestamp +
// log_key. The compact RFC3339 basic form sorts lexicographically.
func Key(slug string, startedAt time.Time, logKey string) string {
	return fmt.Sprintf("%s%s/%s-%s.json", Prefix, slug, startedAt.UTC().Format("20060102T150405Z"), logKey)
}

// Listing is the row data returned by List — just enough to render the
// transcripts table without paying for a full transcript Read per row.
type Listing struct {
	Key       string
	Timestamp time.Time
	LogKey    string
}

// List returns every transcript key for a slug, newest first.
func List(ctx context.Context, st *store.Store, slug string) ([]Listing, error) {
	if slug == "" {
		return nil, errors.New("editrec: slug is empty")
	}
	keys, err := st.ListPrefix(ctx, Prefix+slug+"/")
	if err != nil {
		return nil, fmt.Errorf("list transcripts %s: %w", slug, err)
	}
	out := make([]Listing, 0, len(keys))
	for _, k := range keys {
		ts, logKey := parseKey(k)
		out = append(out, Listing{Key: k, Timestamp: ts, LogKey: logKey})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	return out, nil
}

// Read fetches and decodes a single transcript by its full bucket key.
// Returns an empty Transcript (no error) for a missing key so the viewer
// can render a clean "not found" state.
func Read(ctx context.Context, st *store.Store, key string) (Transcript, error) {
	obj, err := st.ReadRaw(ctx, key)
	if err != nil {
		return Transcript{}, fmt.Errorf("read transcript %s: %w", key, err)
	}
	if obj == nil || obj.Content == "" {
		return Transcript{}, nil
	}
	var t Transcript
	err = json.Unmarshal([]byte(obj.Content), &t)
	if err != nil {
		return Transcript{}, fmt.Errorf("decode transcript %s: %w", key, err)
	}
	return t, nil
}

// Delete removes a transcript by full bucket key. Caller is responsible for
// ensuring the key belongs to the slug they're acting on.
func Delete(ctx context.Context, st *store.Store, key string) error {
	if !strings.HasPrefix(key, Prefix) {
		return fmt.Errorf("editrec: refusing to delete non-transcript key %q", key)
	}
	err := st.DeleteRaw(ctx, key)
	if err != nil {
		return fmt.Errorf("delete transcript %s: %w", key, err)
	}
	return nil
}

// Trim deletes the oldest transcripts beyond `keep`. Best-effort logging on
// individual failures; mirrors snapshot.trim semantics.
func Trim(ctx context.Context, st *store.Store, slug string, keep int) {
	if keep <= 0 {
		return
	}
	rows, err := List(ctx, st, slug)
	if err != nil {
		slog.Warn("editrec.trim_list_failed", "slug", slug, "err", err)
		return
	}
	if len(rows) <= keep {
		return
	}
	for _, victim := range rows[keep:] {
		err := st.DeleteRaw(ctx, victim.Key)
		if err != nil {
			slog.Warn("editrec.trim_delete_failed", "slug", slug, "key", victim.Key, "err", err)
			continue
		}
		slog.Info("editrec.trim", "slug", slug, "key", victim.Key)
	}
}

// parseKey extracts the started_at timestamp and log_key from a transcript
// bucket key. Best-effort: an unparseable key returns zero time so it still
// shows up in listings but sinks to the bottom.
func parseKey(key string) (time.Time, string) {
	base := path.Base(key)
	name := strings.TrimSuffix(base, ".json")
	// Format: {ts}-{logkey}
	idx := strings.IndexByte(name, '-')
	if idx <= 0 {
		return time.Time{}, name
	}
	ts, err := time.Parse("20060102T150405Z", name[:idx])
	if err != nil {
		return time.Time{}, name[idx+1:]
	}
	return ts, name[idx+1:]
}
