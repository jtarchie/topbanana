package textedit

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
		want       string
		wantCount  int
		wantNote   bool // whether a non-empty note is expected
		wantErr    bool
	}{
		{
			name:      "single exact match",
			content:   "<h1>hi</h1>",
			oldText:   "hi",
			newText:   "hello",
			want:      "<h1>hello</h1>",
			wantCount: 1,
		},
		{
			name:    "not found",
			content: "<h1>hi</h1>",
			oldText: "nope",
			newText: "x",
			wantErr: true,
		},
		{
			name:    "ambiguous without replace_all",
			content: "a a a",
			oldText: "a",
			newText: "b",
			wantErr: true,
		},
		{
			name:       "replace_all",
			content:    "a a a",
			oldText:    "a",
			newText:    "b",
			replaceAll: true,
			want:       "b b b",
			wantCount:  3,
		},
		{
			name:      "whitespace-tolerant unique match",
			content:   "<div>\n    <p>hi</p>\n</div>",
			oldText:   "<div> <p>hi</p> </div>",
			newText:   "<section>ok</section>",
			want:      "<section>ok</section>",
			wantCount: 1,
			wantNote:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := ApplyEdit(tc.content, tc.oldText, tc.newText, tc.replaceAll)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (out=%q)", res.Content)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Content != tc.want {
				t.Errorf("content = %q, want %q", res.Content, tc.want)
			}
			if res.Count != tc.wantCount {
				t.Errorf("count = %d, want %d", res.Count, tc.wantCount)
			}
			if (res.Note != "") != tc.wantNote {
				t.Errorf("note presence = %v (note=%q), want %v", res.Note != "", res.Note, tc.wantNote)
			}
		})
	}
}

func TestApplyEdit_IdenticalTolerantAmbiguity(t *testing.T) {
	// Two whitespace-equivalent regions => tolerant match must bail, surfacing
	// a diagnostic rather than editing the wrong one.
	content := "<p>x</p>\n<p>x</p>"
	_, err := ApplyEdit(content, "<p>x</p>", "<p>y</p>", false)
	if err == nil {
		t.Fatal("expected ambiguity error for duplicate exact matches")
	}
}

func TestApplyEdit_TolerantAmbiguityFallsThrough(t *testing.T) {
	// old_text has zero exact matches but two whitespace-variant regions; the
	// tolerant search must refuse (ambiguous) and surface the not-found
	// diagnostic that points at grep_files.
	content := "<p> a </p>\n<p>  a  </p>"
	_, err := ApplyEdit(content, "<p>a</p>", "x", false)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	if !strings.Contains(err.Error(), "grep_files") {
		t.Errorf("not-found diagnostic should mention grep_files, got %q", err.Error())
	}
}

func TestDiagnoseNotFound(t *testing.T) {
	cases := []struct {
		name    string
		content string
		oldText string
		wantSub string
	}{
		{"trim", "<p>hello</p>", "  <p>hello</p>  ", "extra leading or trailing whitespace"},
		{"collapsed", "<p>a\tb</p>", "<p>a b</p>", "whitespace is normalized"},
		{"generic", "<p>abc</p>", "xyz", "not found in file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := DiagnoseNotFound(tc.content, tc.oldText)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("DiagnoseNotFound = %v, want substring %q", err, tc.wantSub)
			}
		})
	}
}

func TestCollapseWS(t *testing.T) {
	cases := map[string]string{
		"a  b":          "a b",
		"a\t\nb":        "a b",
		"  lead":        " lead",
		"trail  ":       "trail ",
		"no-ws":         "no-ws",
		"":              "",
		"\n\n\nmid\n\n": " mid ",
	}
	for in, want := range cases {
		if got := CollapseWS(in); got != want {
			t.Errorf("CollapseWS(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSpliceLines(t *testing.T) {
	cases := []struct {
		name    string
		content string
		start   int
		end     int
		newText string
		want    string
		wantErr bool
	}{
		{"replace middle", "a\nb\nc", 2, 2, "B", "a\nB\nc", false},
		{"replace range", "a\nb\nc\nd", 2, 3, "X\nY", "a\nX\nY\nd", false},
		{"delete range", "a\nb\nc", 2, 2, "", "a\nc", false},
		{"start below 1", "a\nb", 0, 1, "x", "", true},
		{"inverted", "a\nb", 2, 1, "x", "", true},
		{"end beyond file", "a\nb", 2, 5, "x", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := SpliceLines(tc.content, tc.start, tc.end, tc.newText)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got out=%q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out != tc.want {
				t.Errorf("SpliceLines = %q, want %q", out, tc.want)
			}
		})
	}
}

func TestInsertAfterLine(t *testing.T) {
	cases := []struct {
		name    string
		content string
		after   int
		insert  string
		want    string
		wantErr bool
	}{
		{"prepend", "a\nb", 0, "X", "X\na\nb", false},
		{"middle", "a\nb\nc", 1, "X", "a\nX\nb\nc", false},
		{"append", "a\nb", 2, "X", "a\nb\nX", false},
		{"negative", "a\nb", -1, "X", "", true},
		{"beyond", "a\nb", 9, "X", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := InsertAfterLine(tc.content, tc.after, tc.insert)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got out=%q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out != tc.want {
				t.Errorf("InsertAfterLine = %q, want %q", out, tc.want)
			}
		})
	}
}

func TestSliceLines(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		start     int
		end       int
		want      string
		wantTotal int
		wantErr   bool
	}{
		{"whole when zero", "a\nb\nc", 0, 0, "a\nb\nc", 3, false},
		{"slice middle", "a\nb\nc\nd", 2, 3, "b\nc", 4, false},
		{"end clamps", "a\nb\nc", 2, 99, "b\nc", 3, false},
		{"start past end empty", "a\nb", 9, 9, "", 2, false},
		{"inverted error", "a\nb\nc", 3, 1, "", 3, true},
		{"empty content", "", 0, 0, "", 0, false},
		// strings.Split("a\nb\n","\n") == ["a","b",""] — three "lines".
		{"trailing newline counts empty final line", "a\nb\n", 0, 0, "a\nb\n", 3, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, total, err := SliceLines(tc.content, tc.start, tc.end)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got out=%q", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if out != tc.want {
				t.Errorf("slice = %q, want %q", out, tc.want)
			}
			if total != tc.wantTotal {
				t.Errorf("total = %d, want %d", total, tc.wantTotal)
			}
		})
	}
}

func TestNumberLines(t *testing.T) {
	cases := []struct {
		name        string
		content     string
		startOffset int
		want        string
	}{
		{"empty", "", 1, ""},
		{"single", "hi", 1, "     1\thi"},
		{"multi", "a\nb", 1, "     1\ta\n     2\tb"},
		{"offset", "a\nb", 5, "     5\ta\n     6\tb"},
		{"zero offset clamps to 1", "x", 0, "     1\tx"},
		{"trailing newline numbers empty final entry", "a\nb\n", 1, "     1\ta\n     2\tb\n     3\t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NumberLines(tc.content, tc.startOffset); got != tc.want {
				t.Errorf("NumberLines(%q,%d) = %q, want %q", tc.content, tc.startOffset, got, tc.want)
			}
		})
	}
}

func TestValidateHTMLPath(t *testing.T) {
	good := []string{"index.html", "about.html", "blog/post.html", "a-b_c.html"}
	for _, p := range good {
		err := ValidateHTMLPath(p)
		if err != nil {
			t.Errorf("ValidateHTMLPath(%q) unexpected error: %v", p, err)
		}
	}
	bad := []string{
		"",                 // empty
		"/index.html",      // leading slash
		"..\\x.html",       // backslash
		"../escape.html",   // relative segment
		"dir/../x.html",    // non-canonical
		"styles.css",       // wrong extension
		"page.HTML",        // uppercase extension not allowed
		"functions/x.html", // reserved prefix
		"assets/a.html",    // reserved prefix
		".topbanana.json",  // reserved + wrong ext
		"Caps.html",        // uppercase charset
	}
	for _, p := range bad {
		err := ValidateHTMLPath(p)
		if err == nil {
			t.Errorf("ValidateHTMLPath(%q) = nil, want error", p)
		}
	}
}

func TestValidateFunctionName(t *testing.T) {
	good := []string{"submit", "save-entry", "a_b", "x1"}
	for _, n := range good {
		err := ValidateFunctionName(n)
		if err != nil {
			t.Errorf("ValidateFunctionName(%q) unexpected error: %v", n, err)
		}
	}
	bad := []string{"", "Submit", "with space", "dots.x", "slash/x", strings.Repeat("a", 41)}
	for _, n := range bad {
		err := ValidateFunctionName(n)
		if err == nil {
			t.Errorf("ValidateFunctionName(%q) = nil, want error", n)
		}
	}
}

func TestGrepEligible(t *testing.T) {
	cases := map[string]bool{
		"index.html":        true,
		"functions/x.js":    true,
		"assets/logo.svg":   false,
		".topbanana.json":   false,
		".bloomhollow.json": false,
		".buildabear.json":  false,
		"notes.txt":         false,
	}
	for p, want := range cases {
		if got := GrepEligible(p); got != want {
			t.Errorf("GrepEligible(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestMatchLines(t *testing.T) {
	content := "alpha\nbeta needle\ngamma\nneedle again"
	got := MatchLines("index.html", content, "needle", 200)
	if len(got) != 2 {
		t.Fatalf("got %d matches, want 2: %+v", len(got), got)
	}
	if got[0].LineNumber != 2 || got[1].LineNumber != 4 {
		t.Errorf("line numbers = %d,%d want 2,4", got[0].LineNumber, got[1].LineNumber)
	}
	if got[0].Path != "index.html" {
		t.Errorf("path = %q want index.html", got[0].Path)
	}
	if none := MatchLines("p", content, "absent", 200); none != nil {
		t.Errorf("expected nil for no match, got %+v", none)
	}
	if empty := MatchLines("p", content, "", 200); empty != nil {
		t.Errorf("expected nil for empty pattern, got %+v", empty)
	}
}

func TestTruncateSnippet(t *testing.T) {
	if got := TruncateSnippet("short", 200); got != "short" {
		t.Errorf("short snippet altered: %q", got)
	}
	long := strings.Repeat("x", 10)
	got := TruncateSnippet(long, 4)
	if got != "xxxx…" {
		t.Errorf("TruncateSnippet = %q, want %q", got, "xxxx…")
	}
}
