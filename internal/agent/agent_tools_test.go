package agent

import (
	"strings"
	"testing"
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

func TestGrepEligible(t *testing.T) {
	cases := map[string]bool{
		"index.html":             true,
		"functions/submit.js":    true,
		"about/team.html":        true,
		"assets/hero.jpg":        false,
		"assets/foo.html":        false, // anything under assets/ is excluded
		".buildabear.json":       false,
		"functions/handler.json": false, // wrong extension
		"styles.css":             false,
	}
	for path, want := range cases {
		if got := grepEligible(path); got != want {
			t.Errorf("grepEligible(%q) = %v, want %v", path, got, want)
		}
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
