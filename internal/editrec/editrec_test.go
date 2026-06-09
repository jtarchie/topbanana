package editrec_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/topbanana/internal/compressutil"
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/store"
)

func minioStore(t *testing.T) *store.Store {
	t.Helper()
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	bucket := os.Getenv("S3_BUCKET")
	if endpoint == "" || bucket == "" {
		return nil
	}
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	s, err := store.New(client, bucket, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	err = s.EnsureBucket(context.Background())
	if err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return s
}

func freshSlug(t *testing.T) string {
	t.Helper()
	return "edittest-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func cleanup(t *testing.T, ctx context.Context, s *store.Store, slug string) {
	t.Helper()
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
		rows, _ := editrec.List(ctx, s, slug)
		for _, r := range rows {
			_ = editrec.Delete(ctx, s, r.Key)
		}
	})
}

// TestRecorderCapturesWriteFile exercises the full happy path: an emit
// callback receives start/done events for write_file, the recorder reads
// before/after content from the store, and Finish persists a transcript.
func TestRecorderCapturesWriteFile(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	err := s.Write(ctx, slug, "index.html", "<p>old times: 8am-4pm</p>", "text/html", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	rec := editrec.New(slug, "edit", "fix the times to 9-5", "index.html", 25)
	emit := rec.Wrap(ctx, s, slug, nil)

	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseStart, Path: "index.html"})
	err = s.Write(ctx, slug, "index.html", "<p>new times: 9am-5pm</p>", "text/html", nil)
	if err != nil {
		t.Fatalf("agent write: %v", err)
	}
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseDone, Path: "index.html"})

	rec.Finish(ctx, s, events.StatusCompleted, nil)

	tr := mustReadOnlyTranscript(t, ctx, s, slug)
	assertTranscriptMeta(t, tr, "edit", "index.html", 25, events.StatusCompleted)
	assertFileChange(t, tr, "old times", "new times")
}

func mustReadOnlyTranscript(t *testing.T, ctx context.Context, s *store.Store, slug string) editrec.Transcript {
	t.Helper()
	rows, err := editrec.List(ctx, s, slug)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 transcript, got %d", len(rows))
	}
	tr, err := editrec.Read(ctx, s, rows[0].Key)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return tr
}

func assertTranscriptMeta(t *testing.T, tr editrec.Transcript, logKey, page string, selLen int, status string) {
	t.Helper()
	if tr.LogKey != logKey {
		t.Errorf("LogKey = %q, want %q", tr.LogKey, logKey)
	}
	if tr.Page != page {
		t.Errorf("Page = %q, want %q", tr.Page, page)
	}
	if tr.SelectionLen != selLen {
		t.Errorf("SelectionLen = %d, want %d", tr.SelectionLen, selLen)
	}
	if tr.FinalStatus != status {
		t.Errorf("FinalStatus = %q, want %q", tr.FinalStatus, status)
	}
}

func assertFileChange(t *testing.T, tr editrec.Transcript, wantBefore, wantAfter string) {
	t.Helper()
	if len(tr.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2 (start+done)", len(tr.ToolCalls))
	}
	if len(tr.FileChanges) != 1 {
		t.Fatalf("FileChanges = %d, want 1", len(tr.FileChanges))
	}
	fc := tr.FileChanges[0]
	if !strings.Contains(fc.BeforeContent, wantBefore) {
		t.Errorf("BeforeContent missing %q: %q", wantBefore, fc.BeforeContent)
	}
	if !strings.Contains(fc.AfterContent, wantAfter) {
		t.Errorf("AfterContent missing %q: %q", wantAfter, fc.AfterContent)
	}
	if fc.BeforeSHA256 == fc.AfterSHA256 {
		t.Errorf("expected sha256 to differ between before and after")
	}
}

// TestRecorderSkipsNonMutators verifies that read-only tools appear in the
// tool-call timeline but don't produce file-change entries.
func TestRecorderSkipsNonMutators(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	rec := editrec.New(slug, "edit", "list the files", "", 0)
	emit := rec.Wrap(ctx, s, slug, nil)

	emit(events.Event{Type: events.TypeTool, Tool: "list_files", Phase: events.PhaseStart})
	emit(events.Event{Type: events.TypeTool, Tool: "list_files", Phase: events.PhaseDone})

	rec.Finish(ctx, s, events.StatusCompleted, nil)

	rows, _ := editrec.List(ctx, s, slug)
	if len(rows) != 1 {
		t.Fatalf("want 1 transcript, got %d", len(rows))
	}
	tr, _ := editrec.Read(ctx, s, rows[0].Key)
	if len(tr.ToolCalls) != 2 {
		t.Errorf("ToolCalls = %d, want 2", len(tr.ToolCalls))
	}
	if len(tr.FileChanges) != 0 {
		t.Errorf("FileChanges = %d, want 0 for read-only tools", len(tr.FileChanges))
	}
}

// TestRecorderRecordsAgentNoWrite is the smoking-gun scenario: the agent
// reports success but never calls a mutator. Transcript shows zero file
// changes — distinguishing "never wrote" from "wrote but served stale."
func TestRecorderRecordsAgentNoWrite(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	rec := editrec.New(slug, "edit", "fix the times", "index.html", 0)
	emit := rec.Wrap(ctx, s, slug, nil)
	// Agent reads but does NOT write.
	emit(events.Event{Type: events.TypeTool, Tool: "read_file", Phase: events.PhaseStart, Path: "index.html"})
	emit(events.Event{Type: events.TypeTool, Tool: "read_file", Phase: events.PhaseDone, Path: "index.html"})

	rec.Finish(ctx, s, events.StatusCompleted, nil)

	rows, _ := editrec.List(ctx, s, slug)
	tr, _ := editrec.Read(ctx, s, rows[0].Key)
	if len(tr.FileChanges) != 0 {
		t.Errorf("expected 0 file changes for read-only run, got %d", len(tr.FileChanges))
	}
}

// TestRecorderHandlesMutatorError verifies an error phase doesn't leave a
// dangling before-snapshot and doesn't emit a phantom FileChange.
func TestRecorderHandlesMutatorError(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	rec := editrec.New(slug, "edit", "bad path", "", 0)
	emit := rec.Wrap(ctx, s, slug, nil)
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseStart, Path: "bogus.html"})
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseError, Path: "bogus.html", Message: "validation failed"})

	rec.Finish(ctx, s, events.StatusFailed, nil)

	rows, _ := editrec.List(ctx, s, slug)
	tr, _ := editrec.Read(ctx, s, rows[0].Key)
	if len(tr.FileChanges) != 0 {
		t.Errorf("expected 0 file changes after error, got %d", len(tr.FileChanges))
	}
	if len(tr.ToolCalls) != 2 {
		t.Errorf("expected 2 tool calls (start+error), got %d", len(tr.ToolCalls))
	}
}

// TestRecordEdit covers the one-shot helper the MCP edit tools use: a direct
// store write with no agent loop still produces a transcript with a single
// tool call + a single before/after FileChange, tagged with the caller's log
// key and marked completed. This is what makes MCP edits show up under the
// /system dashboard's Recent builds / Last edited.
func TestRecordEdit(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	editrec.RecordEdit(ctx, s, slug, "mcp", "edit_file", "index.html", "<p>old</p>", "<p>new</p>")

	tr := mustReadOnlyTranscript(t, ctx, s, slug)
	if tr.LogKey != "mcp" {
		t.Errorf("LogKey = %q, want %q", tr.LogKey, "mcp")
	}
	if tr.FinalStatus != events.StatusCompleted {
		t.Errorf("FinalStatus = %q, want %q", tr.FinalStatus, events.StatusCompleted)
	}
	if tr.FinishedAt.IsZero() {
		t.Error("FinishedAt unset — would render as in-progress")
	}
	if len(tr.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(tr.ToolCalls))
	}
	if tr.ToolCalls[0].Tool != "edit_file" {
		t.Errorf("ToolCalls[0].Tool = %q, want edit_file", tr.ToolCalls[0].Tool)
	}
	if len(tr.FileChanges) != 1 {
		t.Fatalf("FileChanges = %d, want 1", len(tr.FileChanges))
	}
	fc := tr.FileChanges[0]
	if !strings.Contains(fc.BeforeContent, "old") || !strings.Contains(fc.AfterContent, "new") {
		t.Errorf("FileChange before/after = %q / %q", fc.BeforeContent, fc.AfterContent)
	}
	if fc.BeforeSHA256 == fc.AfterSHA256 {
		t.Error("expected before/after sha256 to differ")
	}
}

// TestRecordEditDelete checks the delete shape: a non-empty before and an
// empty after, so the transcript records what was removed.
func TestRecordEditDelete(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	editrec.RecordEdit(ctx, s, slug, "mcp", "delete_file", "about.html", "<p>bye</p>", "")

	tr := mustReadOnlyTranscript(t, ctx, s, slug)
	if len(tr.FileChanges) != 1 {
		t.Fatalf("FileChanges = %d, want 1", len(tr.FileChanges))
	}
	fc := tr.FileChanges[0]
	if !strings.Contains(fc.BeforeContent, "bye") {
		t.Errorf("BeforeContent = %q, want it to contain %q", fc.BeforeContent, "bye")
	}
	if fc.AfterSize != 0 || fc.AfterContent != "" {
		t.Errorf("after a delete, AfterSize/AfterContent = %d / %q, want 0 / empty", fc.AfterSize, fc.AfterContent)
	}
}

// TestFinishWritesZstd confirms the on-disk transcript is zstd-compressed.
// This is what shrinks the "Build transcripts" row on /system's Storage
// breakdown — the byte count S3 reports is the compressed size.
func TestFinishWritesZstd(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	rec := editrec.New(slug, "edit", strings.Repeat("user prompt that compresses well. ", 100), "", 0)
	rec.Finish(ctx, s, events.StatusCompleted, nil)

	rows, err := editrec.List(ctx, s, slug)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List err=%v rows=%d, want 1", err, len(rows))
	}
	raw, err := s.ReadRaw(ctx, rows[0].Key)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	body := []byte(raw.Content)
	if !compressutil.HasMagic(body) {
		t.Fatalf("stored transcript not zstd-compressed (first four bytes %x)", body[:min(4, len(body))])
	}
	// Sanity-check round-trip: Read should decode the gzipped body back to
	// the original prompt.
	tr, err := editrec.Read(ctx, s, rows[0].Key)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.HasPrefix(tr.UserPrompt, "user prompt that compresses well.") {
		t.Errorf("round-trip UserPrompt = %q", tr.UserPrompt)
	}
}

// TestReadDecodesLegacyUncompressed verifies that transcripts written before
// gzip-at-rest landed (raw JSON in S3) still decode through Read. Magic-byte
// sniffing in maybeGunzip is what makes this work with no migration step.
func TestReadDecodesLegacyUncompressed(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	key := editrec.Key(slug, time.Now().UTC(), "legacy")
	plain := `{"slug":"` + slug + `","log_key":"legacy","user_prompt":"old format","tool_calls":[],"file_changes":[]}`
	err := s.WriteRaw(ctx, key, plain, "application/json", nil)
	if err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	tr, err := editrec.Read(ctx, s, key)
	if err != nil {
		t.Fatalf("Read legacy: %v", err)
	}
	if tr.UserPrompt != "old format" {
		t.Errorf("legacy UserPrompt = %q, want %q", tr.UserPrompt, "old format")
	}
}

// TestRecorderTrim verifies retention drops oldest transcripts beyond the
// keep cap.
func TestRecorderTrim(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run editrec integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	cleanup(t, ctx, s, slug)

	for range 5 {
		rec := editrec.New(slug, "edit", "", "", 0)
		// Stagger started_at so the key timestamps differ.
		time.Sleep(1100 * time.Millisecond)
		rec.Finish(ctx, s, events.StatusCompleted, nil)
	}
	rows, _ := editrec.List(ctx, s, slug)
	if len(rows) != 5 {
		t.Fatalf("want 5, got %d", len(rows))
	}
	editrec.Trim(ctx, s, slug, 2)
	rows, _ = editrec.List(ctx, s, slug)
	if len(rows) != 2 {
		t.Errorf("after trim want 2, got %d", len(rows))
	}
}
