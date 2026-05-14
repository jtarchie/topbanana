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
			name:          "diagnoses internal whitespace mismatch",
			content:       "<div>\n    <p>Hello</p>\n</div>",
			oldText:       "<div>\n  <p>Hello</p>\n</div>",
			newText:       "x",
			errSubstrings: []string{"whitespace is normalized"},
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
			out, count, err := applyEdit(tc.content, tc.oldText, tc.newText, tc.replaceAll)
			if len(tc.errSubstrings) > 0 {
				if err == nil {
					t.Fatalf("expected error, got out=%q count=%d", out, count)
				}
				for _, sub := range tc.errSubstrings {
					if !strings.Contains(err.Error(), sub) {
						t.Errorf("error %q missing substring %q", err.Error(), sub)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if out != tc.wantOut {
				t.Errorf("out: got %q, want %q", out, tc.wantOut)
			}
			if count != tc.wantCount {
				t.Errorf("count: got %d, want %d", count, tc.wantCount)
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
