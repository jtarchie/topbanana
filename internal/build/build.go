// Package build orchestrates the per-slug build lifecycle: seed the
// template skeleton, run the agent, lint and retry on failures, and persist
// the per-site metadata sidecar.
package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/buildabear/internal/agent"
	"github.com/jtarchie/buildabear/internal/editrec"
	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/lint"
	"github.com/jtarchie/buildabear/internal/snapshot"
	"github.com/jtarchie/buildabear/internal/store"
	"github.com/jtarchie/buildabear/internal/templates"
)

const (
	maxLintRetries = 3

	// DefaultBuildTimeout caps total wall-clock for the initial agent.Run
	// plus any lint-retry passes plus the lint passes themselves. The ADK
	// runner has no built-in deadline; without this a stuck LLM call or a
	// stalled network ties up a goroutine and token budget indefinitely.
	//
	// 15 minutes by default because the design-substrate prompt + DaisyUI
	// example pages produce richer HTML files (5–15 KB each), and a local
	// 26B model generating 5 KB of HTML at ~30 tok/s already takes 2-3
	// minutes per turn. Cloud-only deployments can lower this via the CLI.
	DefaultBuildTimeout = 15 * time.Minute

	// MetaFile holds the per-site sidecar (template id, creation time, custom
	// domains). Stored alongside the HTML files in the same S3 prefix so it
	// travels with the site.
	MetaFile = ".buildabear.json"
)

// SiteMeta is the per-site sidecar persisted at MetaFile.
//
// EnablesFunctions is a per-site override layered on top of the template's
// default — set when a user opts in to dynamic features from the settings page
// for a site whose template didn't ship with them. Always read through
// EffectiveTemplate so the override is honoured everywhere the template's
// bit is consulted.
//
// Domains are external hostnames (e.g. `example.com`, `www.example.com`)
// that resolve to this site. Lowercased + port-stripped on write; the server
// builds a reverse Host → slug index from them.
type SiteMeta struct {
	Template         string    `json:"template"`
	Created          time.Time `json:"created"`
	Domains          []string  `json:"domains,omitempty"`
	EnablesFunctions bool      `json:"enables_functions,omitempty"`
	// EnablesPublicAPI opts the site's /api/* routes out of the same-origin
	// check applied to state-changing requests. Off by default so a freshly
	// built site can't be drive-by-spammed from another origin. Turn on for
	// genuine public APIs (webhooks, public JSON endpoints).
	EnablesPublicAPI bool   `json:"enables_public_api,omitempty"`
	Title            string `json:"title,omitempty"`
	Description      string `json:"description,omitempty"`
}

// EffectiveTemplate returns the template a build/edit/route lookup should use,
// OR-ing the per-site EnablesFunctions override on top of the template's
// default. Returns a shallow copy when the override flips the bit on so
// callers can never mutate the package-level template registry.
func EffectiveTemplate(meta SiteMeta) *templates.SiteTemplate {
	base := templates.Get(meta.Template)
	if base == nil || base.EnablesFunctions || !meta.EnablesFunctions {
		return base
	}
	out := *base
	out.EnablesFunctions = true
	return &out
}

// Service runs builds against a Store using a configured LLM, reporting
// progress through an events Tracker. Snapshots (when configured) capture
// the site state right before the agent runs so each build/edit is
// reversible from the History UI. editsKeep caps how many transcripts to
// retain per slug (0 disables trimming, but transcripts are still written).
// buildTimeout caps wall-clock per build; zero falls back to DefaultBuildTimeout.
type Service struct {
	store        *store.Store
	llm          adkmodel.LLM
	events       *events.Tracker
	snapshot     *snapshot.Service
	editsKeep    int
	recordEdit   bool
	buildTimeout time.Duration
}

// Config bundles dependencies for the build service. RecordEdit toggles the
// per-edit transcript capture (enabled by default in production; tests can
// opt out to avoid extra S3 writes). BuildTimeout, when zero, falls back to
// DefaultBuildTimeout.
type Config struct {
	Store        *store.Store
	LLM          adkmodel.LLM
	Events       *events.Tracker
	Snapshot     *snapshot.Service
	EditsKeep    int
	RecordEdit   bool
	BuildTimeout time.Duration
}

func New(s *store.Store, llm adkmodel.LLM, t *events.Tracker, snap *snapshot.Service) *Service {
	return &Service{store: s, llm: llm, events: t, snapshot: snap, recordEdit: true, buildTimeout: DefaultBuildTimeout}
}

// NewWithConfig is the configurable constructor used by cmd/buildabear; New
// stays around for tests and callers that don't care about retention.
func NewWithConfig(cfg Config) *Service {
	timeout := cfg.BuildTimeout
	if timeout <= 0 {
		timeout = DefaultBuildTimeout
	}
	return &Service{
		store:        cfg.Store,
		llm:          cfg.LLM,
		events:       cfg.Events,
		snapshot:     cfg.Snapshot,
		editsKeep:    cfg.EditsKeep,
		recordEdit:   cfg.RecordEdit,
		buildTimeout: timeout,
	}
}

// Params describes one invocation of Start. LogKey distinguishes build vs.
// edit in slog output. SeedSkeleton (initial builds only) writes the
// template's skeleton files and metadata sidecar before the agent runs.
// Attachments are user-uploaded reference files (markdown or HTML) surfaced
// to the agent as pre-seeded read_attachment calls; one-shot per invocation.
//
// UserPrompt, Page, SelectionLen are forensic context for the edit
// transcript: UserPrompt is the raw user input (Prompt may have been wrapped
// with EditPrompt); Page is the file the visual editor was on; SelectionLen
// is the byte length of the selected HTML fragment. All three are optional.
type Params struct {
	Slug         string
	Prompt       string
	LogKey       string
	Template     *templates.SiteTemplate
	SeedSkeleton bool
	Seeds        []agent.SeedToolCall
	Attachments  []agent.Attachment
	UserPrompt   string
	Page         string
	SelectionLen int
}

// Start records the build as in-flight and runs it asynchronously. The
// goroutine emits status events through the tracker; callers render the
// progress page and subscribe via the events handler.
func (svc *Service) Start(p Params) {
	svc.events.Start(p.Slug)

	go func() {
		ctx := context.Background()
		if p.SeedSkeleton {
			err := svc.seedTemplate(ctx, p.Slug, p.Template)
			if err != nil {
				slog.Error(p.LogKey+".seed_failed", "slug", p.Slug, "template", p.Template.ID, "err", err)
				svc.events.Fail(p.Slug, err)
				return
			}
		}
		// Snapshot post-seed and pre-agent. For initial builds this captures
		// the bare template (restorable to a known-good starting point); for
		// edits it captures the prior agent-built site. Failures are logged
		// but don't block the build — losing undo is better than losing the
		// edit.
		if svc.snapshot != nil {
			_, err := svc.snapshot.Create(ctx, p.Slug, p.LogKey)
			if err != nil {
				slog.Warn(p.LogKey+".snapshot_failed", "slug", p.Slug, "err", err)
			}
		}
		var rec *editrec.Recorder
		if svc.recordEdit {
			userPrompt := p.UserPrompt
			if userPrompt == "" {
				userPrompt = p.Prompt
			}
			rec = editrec.New(p.Slug, p.LogKey, userPrompt, p.Page, p.SelectionLen)
		}
		err := svc.buildAndLint(ctx, p.Slug, p.Prompt, p.Template, p.Attachments, p.Seeds, rec)
		if err != nil {
			slog.Error(p.LogKey+".failed", "slug", p.Slug, "err", err)
			svc.events.Fail(p.Slug, err)
			if rec != nil {
				rec.Finish(ctx, svc.store, events.StatusFailed, err)
				if svc.editsKeep > 0 {
					editrec.Trim(ctx, svc.store, p.Slug, svc.editsKeep)
				}
			}
			return
		}
		svc.refreshDescription(ctx, p.Slug, p.Prompt)
		slog.Info(p.LogKey+".done", "slug", p.Slug)
		svc.events.Complete(p.Slug)
		if rec != nil {
			rec.Finish(ctx, svc.store, events.StatusCompleted, nil)
			if svc.editsKeep > 0 {
				editrec.Trim(ctx, svc.store, p.Slug, svc.editsKeep)
			}
		}
	}()
}

// Lint runs the standard lint pass against a site. Exposed so callers (like
// a "force re-lint" endpoint) can ask for the same checks the build retry
// loop uses, without invoking the agent.
func (svc *Service) Lint(ctx context.Context, slug string, tmpl *templates.SiteTemplate) []lint.Error {
	return lint.App(ctx, svc.store, slug, tmpl)
}

// LintFixPrompt formats lint errors as a prompt the agent can act on. Shared
// between the retry loop and any caller (e.g. a force-relint button) that
// wants to kick off a build to fix observed issues.
func LintFixPrompt(errs []lint.Error) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return "Fix these issues in the site:\n" + strings.Join(msgs, "\n")
}

// buildAndLint runs the agent then lints with up to maxLintRetries fix-up
// passes when issues are found.
func (svc *Service) buildAndLint(ctx context.Context, slug, prompt string, tmpl *templates.SiteTemplate, attachments []agent.Attachment, seeds []agent.SeedToolCall, rec *editrec.Recorder) error {
	ctx, cancel := context.WithTimeout(ctx, svc.buildTimeout)
	defer cancel()

	emit := func(e events.Event) { svc.events.Emit(slug, e) }
	if rec != nil {
		emit = rec.Wrap(ctx, svc.store, slug, emit)
	}

	err := agent.Run(ctx, svc.llm, svc.store, slug, prompt, tmpl, attachments, seeds, emit)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("build timed out after %s", svc.buildTimeout)
		}
		return fmt.Errorf("agent run: %w", err)
	}

	for attempt := 0; attempt <= maxLintRetries; attempt++ {
		emit(events.Event{Type: events.TypeStatus, Status: events.StatusLinting})
		lintErrs := svc.Lint(ctx, slug, tmpl)
		if len(lintErrs) == 0 {
			return nil
		}

		if attempt == maxLintRetries {
			msgs := make([]string, 0, len(lintErrs))
			for _, e := range lintErrs {
				msgs = append(msgs, e.Error())
			}
			return fmt.Errorf("lint errors after %d retries: %s", maxLintRetries, strings.Join(msgs, "; "))
		}

		slog.Info("build.lint_retry", "slug", slug, "attempt", attempt+1, "issues", len(lintErrs))
		emit(events.Event{Type: events.TypeStatus, Status: events.StatusRetry, Message: fmt.Sprintf("fixing %d issue(s)", len(lintErrs))})
		err := agent.Run(ctx, svc.llm, svc.store, slug, LintFixPrompt(lintErrs), tmpl, attachments, nil, emit)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("build timed out after %s", svc.buildTimeout)
			}
			return fmt.Errorf("agent retry: %w", err)
		}
	}

	return nil
}

// seedTemplate writes the template's skeleton files (if any) and the
// .buildabear.json sidecar recording the template id. The sidecar lets later
// edits re-apply the same template addendum.
func (svc *Service) seedTemplate(ctx context.Context, slug string, tmpl *templates.SiteTemplate) error {
	if tmpl == nil {
		return nil
	}
	for path, content := range tmpl.Skeleton {
		ct := "text/html; charset=utf-8"
		if strings.HasSuffix(path, ".js") {
			ct = "application/javascript; charset=utf-8"
		}
		err := svc.store.Write(ctx, slug, path, content, ct, nil)
		if err != nil {
			return fmt.Errorf("seed %s: %w", path, err)
		}
		slog.Info("template.seed", "slug", slug, "template", tmpl.ID, "path", path)
	}

	return svc.WriteMeta(ctx, slug, SiteMeta{
		Template: tmpl.ID,
		Created:  time.Now().UTC(),
	})
}

// refreshDescription asks the LLM for a fresh title + description for the
// site and merges them into the existing sidecar. Best-effort: any failure
// is logged and swallowed so the build still completes — the Available Apps
// page just falls back to showing the slug.
func (svc *Service) refreshDescription(ctx context.Context, slug, userPrompt string) {
	desc, err := agent.DescribeSite(ctx, svc.llm, svc.store, slug, userPrompt)
	if err != nil {
		slog.Warn("describe.failed", "slug", slug, "err", err)
		return
	}
	meta := svc.ReadMeta(ctx, slug)
	meta.Title = desc.Title
	meta.Description = desc.Description
	err = svc.WriteMeta(ctx, slug, meta)
	if err != nil {
		slog.Warn("describe.write_failed", "slug", slug, "err", err)
	}
}

// WriteMeta persists the per-site sidecar.
func (svc *Service) WriteMeta(ctx context.Context, slug string, meta SiteMeta) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode site meta: %w", err)
	}
	err = svc.store.Write(ctx, slug, MetaFile, string(body), "application/json", nil)
	if err != nil {
		return fmt.Errorf("write site meta: %w", err)
	}
	return nil
}

// NormalizeDomain lowercases and strips an optional port from a user-entered
// host. Returns an error for empty input or anything that isn't a plausible
// hostname (catches obvious typos like trailing slashes or schemes).
func NormalizeDomain(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.LastIndex(h, ":"); i != -1 {
		h = h[:i]
	}
	if h == "" {
		return "", errors.New("empty domain")
	}
	if strings.ContainsAny(h, "/ \t\r\n") || strings.Contains(h, "://") {
		return "", fmt.Errorf("invalid domain %q", raw)
	}
	if !strings.Contains(h, ".") {
		return "", fmt.Errorf("domain %q must contain a dot", raw)
	}
	return h, nil
}

// ReadMeta returns the recorded sidecar for an existing site, or a zero value
// if the sidecar is missing (older sites pre-date templates).
func (svc *Service) ReadMeta(ctx context.Context, slug string) SiteMeta {
	obj, err := svc.store.Read(ctx, slug, MetaFile)
	if err != nil {
		slog.Warn("site_meta.read_failed", "slug", slug, "err", err)
		return SiteMeta{}
	}
	if obj.Content == "" {
		return SiteMeta{}
	}
	var m SiteMeta
	err = json.Unmarshal([]byte(obj.Content), &m)
	if err != nil {
		slog.Warn("site_meta.decode_failed", "slug", slug, "err", err)
		return SiteMeta{}
	}
	return m
}
