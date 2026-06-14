package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/editrec"
)

// renderDebugEdit parses layout + the debug_edit body the same way New does and
// executes it, so the inline "filediff" partial and nested ranges are exercised
// without a Minio or HTTP dependency.
func renderDebugEdit(t *testing.T, data debugDetailData) string {
	t.Helper()
	tpl := template.New("")
	template.Must(tpl.Parse(layoutTemplate))
	template.Must(tpl.New("debug_edit").Parse(debugEditTemplate))
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "debug_edit", data); err != nil {
		t.Fatalf("execute debug_edit template: %v", err)
	}
	return buf.String()
}

// TestDebugEdit_DiffInlineWithToolCall: a mutation attached to a tool call
// renders its +/- diff inside that tool call's list item, and there is no
// separate "File changes" heading anymore.
func TestDebugEdit_DiffInlineWithToolCall(t *testing.T) {
	t.Parallel()

	row := buildFileRow("pine-thorn-94", editrec.FileChange{
		Path:          "index.html",
		Tool:          "edit_file",
		BeforeSize:    5,
		AfterSize:     5,
		BeforeSHA256:  "aaa",
		AfterSHA256:   "bbb",
		BeforeContent: "hello",
		AfterContent:  "world",
	})
	html := renderDebugEdit(t, debugDetailData{
		FinalStatus:     "completed",
		FileChangeCount: 1,
		ToolCalls: []debugToolRow{
			{IndexLabel: "0", Tool: "edit_file", Phase: "done", Path: "index.html", Files: []debugFileRow{row}},
		},
	})

	if !strings.Contains(html, ">- </span>hello") {
		t.Errorf("expected a deletion line for 'hello' in the inline diff")
	}
	if !strings.Contains(html, ">+ </span>world") {
		t.Errorf("expected an addition line for 'world' in the inline diff")
	}
	if strings.Contains(html, "File changes (") {
		t.Errorf("the standalone File changes section should be gone")
	}
	if !strings.Contains(html, `checkCache('pine-thorn-94', 'index.html'`) {
		t.Errorf("cache-check button should carry the slug and path")
	}
}

// TestDebugEdit_NoMutations: tool calls ran but wrote nothing → the smoking-gun
// note shows.
func TestDebugEdit_NoMutations(t *testing.T) {
	t.Parallel()

	html := renderDebugEdit(t, debugDetailData{
		FinalStatus:     "completed",
		FileChangeCount: 0,
		ToolCalls:       []debugToolRow{{IndexLabel: "0", Tool: "read_file", Phase: "done"}},
	})
	if !strings.Contains(html, "No file mutations were recorded") {
		t.Errorf("expected the no-mutations diagnostic note")
	}
}

// kinds returns the Kind of every diff line, in order, so tests can assert the
// shape of a diff without pinning exact line text.
func kinds(lines []diffLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Kind
	}
	return out
}

// find returns the first diff line of the given kind with matching text, or
// nil if none — used to assert a specific +/- line is present.
func find(lines []diffLine, kind, text string) *diffLine {
	for i := range lines {
		if lines[i].Kind == kind && lines[i].Text == text {
			return &lines[i]
		}
	}
	return nil
}

func TestBuildDiff_IgnoresWhitespace(t *testing.T) {
	// Same words, different indentation and internal spacing → no diff.
	got := buildDiff("  <p>hi</p>\n", "<p>hi</p>\n")
	if len(got) != 0 {
		t.Fatalf("whitespace-only change should produce no diff, got %d lines: %+v", len(got), got)
	}

	got = buildDiff("hello   world", "hello world")
	if len(got) != 0 {
		t.Fatalf("collapsed internal whitespace should produce no diff, got %+v", got)
	}
}

func TestBuildDiff_Identical(t *testing.T) {
	if got := buildDiff("a\nb\nc", "a\nb\nc"); got != nil {
		t.Fatalf("identical content should diff to nil, got %+v", got)
	}
}

func TestBuildDiff_Replace(t *testing.T) {
	before := "line1\nline2\nline3"
	after := "line1\nCHANGED\nline3"
	got := buildDiff(before, after)

	if got[0].Kind != "hunk" {
		t.Fatalf("expected first line to be a hunk header, got %q", got[0].Kind)
	}
	del := find(got, "del", "line2")
	if del == nil {
		t.Fatalf("expected a deletion of line2, got kinds %v", kinds(got))
	}
	if del.Old != 2 {
		t.Errorf("deleted line2 should report old line number 2, got %d", del.Old)
	}
	add := find(got, "add", "CHANGED")
	if add == nil {
		t.Fatalf("expected an addition of CHANGED, got kinds %v", kinds(got))
	}
	if add.New != 2 {
		t.Errorf("added line should report new line number 2, got %d", add.New)
	}
	// line1 and line3 survive as context around the change.
	if find(got, "context", "line1") == nil || find(got, "context", "line3") == nil {
		t.Errorf("expected line1/line3 as context, got kinds %v", kinds(got))
	}
}

func TestBuildDiff_Created(t *testing.T) {
	got := buildDiff("", "alpha\nbeta")
	if find(got, "add", "alpha") == nil || find(got, "add", "beta") == nil {
		t.Fatalf("creating a file should diff to all additions, got kinds %v", kinds(got))
	}
	for _, l := range got {
		if l.Kind == "del" {
			t.Errorf("a created file should have no deletions, got %+v", l)
		}
	}
}

func TestBuildDiff_Deleted(t *testing.T) {
	got := buildDiff("alpha\nbeta", "")
	if find(got, "del", "alpha") == nil || find(got, "del", "beta") == nil {
		t.Fatalf("deleting a file should diff to all deletions, got kinds %v", kinds(got))
	}
	for _, l := range got {
		if l.Kind == "add" {
			t.Errorf("a deleted file should have no additions, got %+v", l)
		}
	}
}

func TestBuildFileRow_WhitespaceOnly(t *testing.T) {
	// Different bytes (so different sha), but only whitespace differs.
	row := buildFileRow("my-slug", editrec.FileChange{
		Path:          "index.html",
		Tool:          "edit_file",
		BeforeSize:    10,
		AfterSize:     8,
		BeforeSHA256:  "aaa",
		AfterSHA256:   "bbb",
		BeforeContent: "<p>  hi  </p>",
		AfterContent:  "<p>hi</p>",
	})
	if !row.Changed {
		t.Fatal("differing shas should mark the row as changed")
	}
	if !row.WhitespaceOnly {
		t.Fatalf("a whitespace-only change should be flagged, diff=%+v", row.Diff)
	}
	if row.Slug != "my-slug" {
		t.Errorf("slug should be carried onto the row, got %q", row.Slug)
	}
}

func TestBuildFileRow_RealChangeNotWhitespaceOnly(t *testing.T) {
	row := buildFileRow("my-slug", editrec.FileChange{
		Path:          "index.html",
		BeforeSHA256:  "aaa",
		AfterSHA256:   "bbb",
		BeforeSize:    5,
		AfterSize:     5,
		BeforeContent: "hello",
		AfterContent:  "world",
	})
	if row.WhitespaceOnly {
		t.Fatal("a real content change must not be flagged whitespace-only")
	}
	if len(row.Diff) == 0 {
		t.Fatal("a real content change should produce diff lines")
	}
}
