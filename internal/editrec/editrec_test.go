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

	"github.com/jtarchie/buildabear/internal/editrec"
	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/store"
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

	for i := 0; i < 5; i++ {
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
