package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/templates"
)

func TestApplyEdit(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		oldText    string
		newText    string
		replaceAll bool
		wantOut    string
		wantCount  int
		wantNote   string // substring that must appear in the returned note (empty = note must be empty)
		// errSubstrings: empty means no error; otherwise each must appear in err.Error()
		errSubstrings []string
	}{
		{
			name:      "unique match",
			content:   "hello world",
			oldText:   "world",
			newText:   "there",
			wantOut:   "hello there",
			wantCount: 1,
		},
		{
			name:          "not found generic",
			content:       "hello world",
			oldText:       "totally absent",
			newText:       "x",
			errSubstrings: []string{"not found", "grep_files"},
		},
		{
			name:          "diagnoses leading/trailing whitespace",
			content:       "hello world",
			oldText:       "  hello  ",
			newText:       "x",
			errSubstrings: []string{"trim"},
		},
		{
			name:      "unique whitespace-tolerant fallback applies",
			content:   "<div>\n    <p>Hello</p>\n</div>",
			oldText:   "<div>\n  <p>Hello</p>\n</div>",
			newText:   "<div><p>Hi</p></div>",
			wantOut:   "<div><p>Hi</p></div>",
			wantCount: 1,
			wantNote:  "whitespace-tolerant",
		},
		{
			name:          "ambiguous tolerant matches still error",
			content:       "<p> a </p>\n<p>  a  </p>",
			oldText:       "<p>a</p>",
			newText:       "x",
			errSubstrings: []string{"not found"},
		},
		{
			name:          "ambiguous without replace_all",
			content:       "a x b x c",
			oldText:       "x",
			newText:       "y",
			errSubstrings: []string{"2 locations", "replace_all"},
		},
		{
			name:       "ambiguous with replace_all",
			content:    "a x b x c",
			oldText:    "x",
			newText:    "y",
			replaceAll: true,
			wantOut:    "a y b y c",
			wantCount:  2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, count, note, err := applyEdit(tc.content, tc.oldText, tc.newText, tc.replaceAll)
			checkApplyEdit(t, out, count, note, err, tc.wantOut, tc.wantCount, tc.wantNote, tc.errSubstrings)
		})
	}
}

func checkApplyEdit(t *testing.T, out string, count int, note string, err error,
	wantOut string, wantCount int, wantNote string, errSubstrings []string,
) {
	t.Helper()
	if len(errSubstrings) > 0 {
		if err == nil {
			t.Fatalf("expected error, got out=%q count=%d", out, count)
		}
		for _, sub := range errSubstrings {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("error %q missing substring %q", err.Error(), sub)
			}
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if out != wantOut {
		t.Errorf("out: got %q, want %q", out, wantOut)
	}
	if count != wantCount {
		t.Errorf("count: got %d, want %d", count, wantCount)
	}
	if wantNote == "" && note != "" {
		t.Errorf("note: got %q, want empty", note)
	}
	if wantNote != "" && !strings.Contains(note, wantNote) {
		t.Errorf("note: got %q, want substring %q", note, wantNote)
	}
}

func TestSpliceLines(t *testing.T) {
	t.Parallel()
	const five = "a\nb\nc\nd\ne"

	cases := []struct {
		name       string
		content    string
		start, end int
		newText    string
		wantOut    string
		wantErr    string // substring; empty means no error expected
	}{
		{"replace single line", five, 3, 3, "C", "a\nb\nC\nd\ne", ""},
		{"replace range", five, 2, 4, "X\nY", "a\nX\nY\ne", ""},
		{"delete range (empty new_text)", five, 2, 4, "", "a\ne", ""},
		{"replace first line", five, 1, 1, "A", "A\nb\nc\nd\ne", ""},
		{"replace last line", five, 5, 5, "E", "a\nb\nc\nd\nE", ""},
		{"start < 1 errors", five, 0, 1, "x", "", "start_line must be"},
		{"end < start errors", five, 3, 2, "x", "", "must be >= start_line"},
		{"end > total errors", five, 4, 99, "x", "", "exceeds file length"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := spliceLines(tc.content, tc.start, tc.end, tc.newText)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got out=%q", tc.wantErr, out)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q missing substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if out != tc.wantOut {
				t.Errorf("out: got %q, want %q", out, tc.wantOut)
			}
		})
	}
}

func TestInsertAfterLine(t *testing.T) {
	t.Parallel()
	const three = "a\nb\nc"

	cases := []struct {
		name    string
		content string
		after   int
		insert  string
		wantOut string
		wantErr string
	}{
		{"prepend with after_line=0", three, 0, "Z", "Z\na\nb\nc", ""},
		{"insert in middle", three, 1, "X", "a\nX\nb\nc", ""},
		{"append at total_lines", three, 3, "Z", "a\nb\nc\nZ", ""},
		{"multi-line insert", three, 1, "X\nY", "a\nX\nY\nb\nc", ""},
		{"negative after_line errors", three, -1, "x", "", "must be >= 0"},
		{"after_line past end errors", three, 99, "x", "", "exceeds file length"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := insertAfterLine(tc.content, tc.after, tc.insert)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got out=%q", tc.wantErr, out)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q missing substring %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if out != tc.wantOut {
				t.Errorf("out: got %q, want %q", out, tc.wantOut)
			}
		})
	}
}

func TestSliceLines(t *testing.T) {
	const five = "a\nb\nc\nd\ne"

	cases := []struct {
		name       string
		content    string
		start, end int
		wantOut    string
		wantTotal  int
		wantErr    bool
	}{
		{"no bounds returns whole content", five, 0, 0, five, 5, false},
		{"middle slice", five, 2, 3, "b\nc", 5, false},
		{"end past total is clamped", five, 4, 99, "d\ne", 5, false},
		{"start past total returns empty, no error", five, 99, 0, "", 5, false},
		{"start greater than end errors", five, 3, 2, "", 5, true},
		// strings.Split("a\nb\n", "\n") == ["a", "b", ""] — three "lines".
		{"trailing newline counts empty final line", "a\nb\n", 0, 0, "a\nb\n", 3, false},
		{"empty content has zero total", "", 0, 0, "", 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, total, err := sliceLines(tc.content, tc.start, tc.end)
			gotErr := err != nil
			if gotErr != tc.wantErr {
				t.Fatalf("err: got %v, want error=%v", err, tc.wantErr)
			}
			if !tc.wantErr && out != tc.wantOut {
				t.Errorf("content: got %q, want %q", out, tc.wantOut)
			}
			if total != tc.wantTotal {
				t.Errorf("total: got %d, want %d", total, tc.wantTotal)
			}
		})
	}
}

func TestNumberLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		content     string
		startOffset int
		want        string
	}{
		{
			name:        "empty stays empty",
			content:     "",
			startOffset: 1,
			want:        "",
		},
		{
			name:        "single line",
			content:     "hello",
			startOffset: 1,
			want:        "     1\thello",
		},
		{
			name:        "three lines starting at 1",
			content:     "a\nb\nc",
			startOffset: 1,
			want:        "     1\ta\n     2\tb\n     3\tc",
		},
		{
			name:        "offset shifts numbers but preserves separators",
			content:     "x\ny",
			startOffset: 42,
			want:        "    42\tx\n    43\ty",
		},
		{
			name:        "zero/negative offset clamps to 1",
			content:     "x\ny",
			startOffset: 0,
			want:        "     1\tx\n     2\ty",
		},
		{
			name:        "trailing newline numbers empty final entry",
			content:     "a\nb\n",
			startOffset: 1,
			want:        "     1\ta\n     2\tb\n     3\t",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NumberLines(tc.content, tc.startOffset)
			if got != tc.want {
				t.Errorf("NumberLines(%q, %d):\n got  %q\n want %q",
					tc.content, tc.startOffset, got, tc.want)
			}
		})
	}
}

func TestGrepEligible(t *testing.T) {
	cases := map[string]bool{
		"index.html":             true,
		"functions/submit.js":    true,
		"about/team.html":        true,
		"assets/hero.jpg":        false,
		"assets/foo.html":        false, // anything under assets/ is excluded
		".bloomhollow.json":      false,
		".buildabear.json":       false, // legacy sidecar — still excluded for old sites
		"functions/handler.json": false, // wrong extension
		"styles.css":             false,
	}
	for path, want := range cases {
		if got := grepEligible(path); got != want {
			t.Errorf("grepEligible(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestValidateHTMLPath(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr string // substring; empty = expect no error
	}{
		{name: "simple ok", path: "index.html"},
		{name: "nested ok", path: "blog/post.html"},
		{name: "dotted ok", path: "v2.about.html"},
		{name: "underscore ok", path: "about_us.html"},
		{name: "empty", path: "", wantErr: "required"},
		{name: "wrong ext", path: "index.htm", wantErr: "must end with .html"},
		{name: "css ext", path: "style.css", wantErr: "must end with .html"},
		{name: "uppercase", path: "INDEX.HTML", wantErr: "[a-z0-9_/.-]"},
		{name: "trailing slash", path: "index.html/", wantErr: "empty or relative"},
		{name: "traversal leading", path: "../escape.html", wantErr: "relative segment"},
		{name: "traversal middle", path: "foo/../bar.html", wantErr: "relative segment"},
		{name: "leading slash", path: "/abs.html", wantErr: "leading /"},
		{name: "double slash", path: "foo//bar.html", wantErr: "empty or relative"},
		{name: "backslash", path: `foo\bar.html`, wantErr: "forward slashes"},
		{name: "space", path: "bad name.html", wantErr: "[a-z0-9_/.-]"},
		{name: "control char", path: "x\nhtml.html", wantErr: "[a-z0-9_/.-]"},
		{name: "reserved functions", path: "functions/x.html", wantErr: "reserved prefix"},
		{name: "reserved assets", path: "assets/x.html", wantErr: "reserved prefix"},
		{name: "reserved meta", path: ".bloomhollow.json", wantErr: "must end with .html"},
		{name: "reserved legacy meta", path: ".buildabear.json", wantErr: "must end with .html"},
		{name: "too long", path: strings.Repeat("a", maxHTMLPathLen-4) + "x.html", wantErr: "too long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHTMLPath(tc.path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestToolGuard(t *testing.T) {
	t.Run("first call allowed", func(t *testing.T) {
		g := &toolGuard{}
		err := g.Allow(toolSignature("write_file", "index.html", "hello"))
		if err != nil {
			t.Fatalf("first call should be allowed, got %v", err)
		}
	})

	t.Run("immediate repeat rejected", func(t *testing.T) {
		g := &toolGuard{}
		sig := toolSignature("write_file", "index.html", "hello")
		err := g.Allow(sig)
		if err != nil {
			t.Fatalf("seed call: %v", err)
		}
		err = g.Allow(sig)
		if err == nil {
			t.Fatal("expected repeat to be rejected")
		}
		if !strings.Contains(err.Error(), "write_file") {
			t.Fatalf("error should name the tool, got %q", err.Error())
		}
	})

	t.Run("rolls over after ring length", func(t *testing.T) {
		g := &toolGuard{}
		sig := toolSignature("write_file", "index.html", "hello")
		err := g.Allow(sig)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Fill the ring with distinct sigs so the seeded one rolls out.
		for i := range toolGuardRingLen {
			distinct := toolSignature("write_file", "index.html", fmt.Sprintf("filler-%d", i))
			err = g.Allow(distinct)
			if err != nil {
				t.Fatalf("filler %d: %v", i, err)
			}
		}
		err = g.Allow(sig)
		if err != nil {
			t.Fatalf("seeded sig should be allowed after %d distinct intervening calls, got %v",
				toolGuardRingLen, err)
		}
	})

	t.Run("different paths are independent", func(t *testing.T) {
		g := &toolGuard{}
		a := toolSignature("write_file", "a.html", "hello")
		b := toolSignature("write_file", "b.html", "hello")
		err := g.Allow(a)
		if err != nil {
			t.Fatalf("a: %v", err)
		}
		err = g.Allow(b)
		if err != nil {
			t.Fatalf("b: %v", err)
		}
	})
}

func TestSkeletonSeeds_NilOrEmpty(t *testing.T) {
	if got := SkeletonSeeds(nil); got != nil {
		t.Errorf("nil template: expected nil, got %d seeds", len(got))
	}
	if got := SkeletonSeeds(&templates.SiteTemplate{Skeleton: nil}); got != nil {
		t.Errorf("empty skeleton: expected nil, got %d seeds", len(got))
	}
}

func TestSkeletonSeeds_HTML(t *testing.T) {
	tmpl := &templates.SiteTemplate{Skeleton: map[string]string{
		"index.html": "<html><body>a</body></html>",
		"about.html": "<html><body>b\nc</body></html>",
	}}
	seeds := SkeletonSeeds(tmpl)
	if len(seeds) != 3 {
		t.Fatalf("expected 3 seeds (list + 2 reads), got %d", len(seeds))
	}
	if seeds[0].Name != "list_files" {
		t.Errorf("first seed must be list_files, got %s", seeds[0].Name)
	}
	// Names are sorted, so about.html comes first.
	if seeds[1].Name != "read_file" || seeds[1].Args["path"] != "about.html" {
		t.Errorf("second seed must be read_file(about.html), got %+v", seeds[1])
	}
	if seeds[2].Args["path"] != "index.html" {
		t.Errorf("third seed must be read_file(index.html), got %+v", seeds[2].Args)
	}
	resp := seeds[2].Response
	if resp["content"] == "" || resp["total_lines"] == nil {
		t.Errorf("read_file response missing content/total_lines: %+v", resp)
	}
}

func TestSkeletonSeeds_Functions(t *testing.T) {
	tmpl := &templates.SiteTemplate{Skeleton: map[string]string{
		"index.html":            "<html></html>",
		"functions/submit.js":   "module.exports = function() {}",
		"functions/redirect.js": "module.exports = function() {}",
	}}
	seeds := SkeletonSeeds(tmpl)
	// 1 list_files + 1 read_file + 1 list_functions + 2 read_function = 5
	if len(seeds) != 5 {
		t.Fatalf("expected 5 seeds, got %d: %+v", len(seeds), seeds)
	}

	listFns := findSeed(seeds, "list_functions", nil)
	if listFns == nil {
		t.Fatal("expected a list_functions seed")
	}
	fns, _ := listFns.Response["functions"].([]string)
	if len(fns) != 2 || fns[0] != "redirect" || fns[1] != "submit" {
		t.Errorf("list_functions returned wrong names: %+v", fns)
	}

	readSubmit := findSeed(seeds, "read_function", map[string]any{"name": "submit"})
	if readSubmit == nil {
		t.Fatal("expected a read_function(submit) seed")
	}
	if readSubmit.Response["source"] == "" {
		t.Errorf("read_function(submit) missing source")
	}
}

// findSeed returns the first seed whose Name matches and whose Args contain
// every key/value in match. Args may be nil to match on Name alone.
func findSeed(seeds []SeedToolCall, name string, match map[string]any) *SeedToolCall {
	for i := range seeds {
		if seeds[i].Name != name {
			continue
		}
		ok := true
		for k, v := range match {
			if seeds[i].Args[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return &seeds[i]
		}
	}
	return nil
}

func TestLineCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"\n", 1},
	}
	for _, c := range cases {
		if got := lineCount(c.in); got != c.want {
			t.Errorf("lineCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFormatBuildContext_Empty(t *testing.T) {
	if got := formatBuildContext(BuildContext{}); got != "" {
		t.Errorf("zero-value BuildContext should render empty, got %q", got)
	}
}

func TestFormatBuildContext_InitialBuild(t *testing.T) {
	// Saturday, 2026-05-30 — picked so the date format assertion is stable.
	when := time.Date(2026, 5, 30, 15, 42, 0, 0, time.UTC)
	got := formatBuildContext(BuildContext{
		Now:     when,
		Slug:    "myapp",
		SiteURL: "http://myapp.localhost:8080",
		IsEdit:  false,
	})
	want := "Build context:\n" +
		"- Today: Saturday, 2026-05-30\n" +
		"- Site: myapp at http://myapp.localhost:8080\n" +
		"- Mode: initial build (skeleton seeded — extend it)"
	if got != want {
		t.Errorf("initial build render mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatBuildContext_FollowUpEdit(t *testing.T) {
	when := time.Date(2026, 5, 30, 15, 42, 0, 0, time.UTC)
	got := formatBuildContext(BuildContext{
		Now:     when,
		Slug:    "myapp",
		SiteURL: "https://myapp.topbanana.io",
		IsEdit:  true,
	})
	if !strings.Contains(got, "- Mode: follow-up edit") {
		t.Errorf("edit mode line missing: %s", got)
	}
	if !strings.Contains(got, "edit_file / replace_lines") {
		t.Errorf("edit-mode rendering must nudge toward edit_file / replace_lines: %s", got)
	}
	if !strings.Contains(got, "- Site: myapp at https://myapp.topbanana.io") {
		t.Errorf("production-style site URL missing: %s", got)
	}
}

func TestFormatBuildContext_SlugOnlyNoURL(t *testing.T) {
	// The legacy build.New constructor leaves Domain empty so the runner
	// emits SiteURL="". The block should still render with date + slug.
	when := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	got := formatBuildContext(BuildContext{
		Now:     when,
		Slug:    "myapp",
		SiteURL: "",
		IsEdit:  false,
	})
	if !strings.Contains(got, "- Site: myapp\n") {
		t.Errorf("site line should render slug alone when SiteURL is empty: %s", got)
	}
	if strings.Contains(got, " at ") {
		t.Errorf("no \" at <url>\" suffix expected when SiteURL is empty: %s", got)
	}
}

func TestFormatBuildContext_Stable(t *testing.T) {
	// Cache stability: identical inputs must produce byte-identical output.
	// Catches accidental non-determinism (map iteration, time formatting).
	bctx := BuildContext{
		Now:     time.Date(2026, 5, 30, 15, 42, 0, 0, time.UTC),
		Slug:    "myapp",
		SiteURL: "http://myapp.localhost:8080",
		IsEdit:  false,
	}
	a := formatBuildContext(bctx)
	b := formatBuildContext(bctx)
	if a != b {
		t.Errorf("formatBuildContext not deterministic:\na: %q\nb: %q", a, b)
	}
}

func TestFormatTemplateChecks(t *testing.T) {
	cases := []struct {
		name   string
		checks []templates.Check
		want   string
	}{
		{
			name:   "empty",
			checks: nil,
			want:   "",
		},
		{
			name: "skips entries missing file or needles",
			checks: []templates.Check{
				{File: "", MustContain: []string{"<h1"}},
				{File: "index.html", MustContain: nil},
			},
			want: "",
		},
		{
			name: "single check with message",
			checks: []templates.Check{
				{File: "index.html", MustContain: []string{"<h1"}, Message: "landing pages need a clear headline"},
			},
			want: "Your output will be validated against these requirements (the lint loop asserts them after every build):\n- index.html must contain `<h1` — landing pages need a clear headline",
		},
		{
			name: "multi-needle joined with and",
			checks: []templates.Check{
				{File: "index.html", MustContain: []string{"<form", `method="post"`}},
			},
			want: "Your output will be validated against these requirements (the lint loop asserts them after every build):\n- index.html must contain `<form` and `method=\"post\"`",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTemplateChecks(tc.checks)
			if got != tc.want {
				t.Errorf("formatTemplateChecks = %q\nwant: %q", got, tc.want)
			}
		})
	}
}

func TestTruncateSnippet(t *testing.T) {
	short := "hello"
	if truncateSnippet(short) != short {
		t.Fatalf("short line should pass through")
	}
	long := strings.Repeat("x", grepSnippetMax+50)
	out := truncateSnippet(long)
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("long line should end with ellipsis")
	}
	// Visible portion equals the cap; the ellipsis is one extra rune.
	if len([]rune(out))-1 != grepSnippetMax {
		t.Fatalf("expected %d visible chars before ellipsis, got %d", grepSnippetMax, len([]rune(out))-1)
	}
}

// --- ask_user tests ----------------------------------------------------------

func newAskState() *buildState { return newBuildState() }

func TestAskUser_ValidationErrors(t *testing.T) {
	t.Parallel()

	tr := events.NewTracker()
	t.Cleanup(tr.Close)
	tr.Start("s")
	noEmit := func(events.Event) {}

	cases := []struct {
		name string
		args askUserArgs
		want string
	}{
		{"missing question", askUserArgs{Recommendation: "r", Why: "w"}, "question is required"},
		{"missing recommendation", askUserArgs{Question: "q?", Why: "w"}, "recommendation is required"},
		{"missing why", askUserArgs{Question: "q?", Recommendation: "r"}, "why is required"},
		{"too many options", askUserArgs{Question: "q?", Recommendation: "r", Why: "w", Options: []string{"a", "b", "c", "d", "e"}}, "options must have at most 4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh state per case so questionsAsked never exceeds the cap.
			state := newAskState()
			res, err := invokeAskUser(context.Background(), tc.args, "s", tr, noEmit, state, time.Millisecond)
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if res.Error == "" {
				t.Fatal("expected Error field to be set, got empty")
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("Error = %q, want substring %q", res.Error, tc.want)
			}
		})
	}
}

func TestAskUser_CapReached(t *testing.T) {
	t.Parallel()

	tr := events.NewTracker()
	t.Cleanup(tr.Close)
	tr.Start("s")
	state := newAskState()
	// Exhaust the cap.
	for range maxQuestionsPerBuild {
		state.questionsAsked.Add(1)
	}

	args := askUserArgs{Question: "q?", Recommendation: "rec", Why: "because"}
	res, err := invokeAskUser(context.Background(), args, "s", tr, func(events.Event) {}, state, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.Source != "limit_reached" {
		t.Errorf("Source = %q, want limit_reached", res.Source)
	}
	if res.Answer != "rec" {
		t.Errorf("Answer = %q, want recommendation %q", res.Answer, "rec")
	}
}

func TestAskUser_Timeout(t *testing.T) {
	t.Parallel()

	tr := events.NewTracker()
	t.Cleanup(tr.Close)
	tr.Start("s")
	state := newAskState()

	args := askUserArgs{Question: "q?", Recommendation: "default", Why: "because"}
	// Pass a very short timeout so the test runs in milliseconds.
	res, err := invokeAskUser(context.Background(), args, "s", tr, func(events.Event) {}, state, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.Source != "recommendation_timeout" {
		t.Errorf("Source = %q, want recommendation_timeout", res.Source)
	}
	if res.Answer != "default" {
		t.Errorf("Answer = %q, want %q", res.Answer, "default")
	}
}

func TestAskUser_UserAnswer(t *testing.T) {
	t.Parallel()

	tr := events.NewTracker()
	t.Cleanup(tr.Close)
	tr.Start("s")
	state := newAskState()

	// Subscribe BEFORE invoking so we receive the PhaseAsk event.
	_, subCh, _ := tr.Subscribe("s")

	args := askUserArgs{Question: "Tone?", Recommendation: "warm", Why: "prompt said cozy"}

	resultCh := make(chan askUserResult, 1)
	go func() {
		res, _ := invokeAskUser(context.Background(), args, "s", tr, func(events.Event) {}, state, 5*time.Second)
		resultCh <- res
	}()

	// Wait for the PhaseAsk event on the subscriber channel to get the qid.
	var qid string
	deadline := time.After(500 * time.Millisecond)
	for qid == "" {
		select {
		case ev := <-subCh:
			if ev.Type == events.TypeQuestion && ev.Phase == events.PhaseAsk {
				qid = ev.QuestionID
			}
		case <-deadline:
			t.Fatal("timed out waiting for PhaseAsk event on tracker subscriber")
		}
	}

	tr.Resolve("s", qid, "playful")

	select {
	case res := <-resultCh:
		if res.Source != "user" {
			t.Errorf("Source = %q, want user", res.Source)
		}
		if res.Answer != "playful" {
			t.Errorf("Answer = %q, want playful", res.Answer)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("invokeAskUser did not return after Resolve")
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	if truncate("hello", 10) != "hello" {
		t.Error("short string should pass through unchanged")
	}
	long := strings.Repeat("x", 300)
	out := truncate(long, 200)
	if len([]rune(out)) != 200 {
		t.Errorf("truncated length = %d, want 200", len([]rune(out)))
	}
}
