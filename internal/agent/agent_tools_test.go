package agent

import (
	"fmt"
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
		{name: "reserved meta", path: ".buildabear.json", wantErr: "must end with .html"},
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
		for i := 0; i < toolGuardRingLen; i++ {
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
