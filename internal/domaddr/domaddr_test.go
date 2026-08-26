package domaddr

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const sample = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>T</title>
</head>
<body>
  <div class="wrap">
    <h1>Hello <em>there</em></h1>
    <p>Same text</p>
    <p>Same text</p>
    <img src="/a.png" alt="">
    <script>if (1 < 2) { document.write("<p>not an element</p>"); }</script>
  </div>
</body>
</html>`

// Element order in sample: 0 html, 1 head, 2 meta, 3 title, 4 body, 5 div,
// 6 h1, 7 em, 8 p, 9 p, 10 img, 11 script.

func TestAnnotate_StampsEveryElementInOrder(t *testing.T) {
	t.Parallel()
	out := string(Annotate([]byte(sample)))

	for i := range Count([]byte(sample)) {
		if !strings.Contains(out, `data-tb-el="`+strconv.Itoa(i)+`"`) {
			t.Fatalf("annotated doc missing address %d:\n%s", i, out)
		}
	}
	if !strings.Contains(out, `<html data-tb-el="0" lang="en">`) {
		t.Fatalf("html tag not annotated in place:\n%s", out)
	}
	if !strings.Contains(out, `<em data-tb-el="7">`) {
		t.Fatalf("em should be address 7:\n%s", out)
	}
	// Script content is raw text — the markup inside it must not be stamped.
	if !strings.Contains(out, `document.write("<p>not an element</p>")`) {
		t.Fatalf("script body was altered:\n%s", out)
	}
}

func TestAnnotate_PreservesEverythingElse(t *testing.T) {
	t.Parallel()
	out := Annotate([]byte(sample))
	stripped := regexp.MustCompile(` data-tb-el="\d+"`).ReplaceAll(out, nil)
	if !bytes.Equal(stripped, []byte(sample)) {
		t.Fatalf("annotation is not byte-reversible:\n--- got ---\n%s\n--- want ---\n%s", stripped, sample)
	}
}

func TestResolve_DuplicatedContentGetsDistinctSpans(t *testing.T) {
	t.Parallel()
	doc := []byte(sample)

	first, err := OuterHTML(doc, 8)
	if err != nil {
		t.Fatalf("resolve 8: %v", err)
	}
	second, err := OuterHTML(doc, 9)
	if err != nil {
		t.Fatalf("resolve 9: %v", err)
	}
	if string(first) != "<p>Same text</p>" || string(second) != "<p>Same text</p>" {
		t.Fatalf("outer html mismatch: %q / %q", first, second)
	}
	s8, _ := Resolve(doc, 8)
	s9, _ := Resolve(doc, 9)
	if s8 == s9 || s8.End > s9.Start {
		t.Fatalf("identical content must still get distinct, ordered spans: %+v vs %+v", s8, s9)
	}
}

func TestResolve_SpansNestVoidAndRawText(t *testing.T) {
	t.Parallel()
	doc := []byte(sample)

	h1, err := OuterHTML(doc, 6)
	if err != nil {
		t.Fatalf("resolve h1: %v", err)
	}
	if string(h1) != "<h1>Hello <em>there</em></h1>" {
		t.Fatalf("h1 span wrong: %q", h1)
	}

	img, err := OuterHTML(doc, 10)
	if err != nil {
		t.Fatalf("resolve img: %v", err)
	}
	if string(img) != `<img src="/a.png" alt="">` {
		t.Fatalf("void element span must be the tag alone: %q", img)
	}

	script, err := OuterHTML(doc, 11)
	if err != nil {
		t.Fatalf("resolve script: %v", err)
	}
	if !strings.HasPrefix(string(script), "<script>") || !strings.HasSuffix(string(script), "</script>") {
		t.Fatalf("script span must include its raw-text body: %q", script)
	}
}

func TestResolve_OutOfRangeAndUnclosed(t *testing.T) {
	t.Parallel()
	_, err := Resolve([]byte(sample), 99)
	if err == nil {
		t.Fatal("out-of-range address must error")
	}
	_, err = Resolve([]byte(sample), -1)
	if err == nil {
		t.Fatal("negative address must error")
	}

	unclosed := []byte(`<div><p>never closed`)
	var span Span
	span, err = Resolve(unclosed, 1)
	if err != nil {
		t.Fatalf("unclosed element: %v", err)
	}
	if span.End != len(unclosed) {
		t.Fatalf("unclosed element must span to EOF, got %+v", span)
	}
}

func FuzzResolve(f *testing.F) {
	f.Add(sample, 3)
	f.Add("<div><div><div></div></div>", 1)
	f.Add("plain text, no tags", 0)
	f.Add("<p>a</p><p>b</p>", 1)
	f.Fuzz(func(t *testing.T, doc string, n int) {
		span, err := Resolve([]byte(doc), n%64)
		if err != nil {
			return
		}
		if span.Start < 0 || span.End > len(doc) || span.Start >= span.End {
			t.Fatalf("span out of bounds: %+v for doc len %d", span, len(doc))
		}
		// Annotate must never lose bytes either.
		out := Annotate([]byte(doc))
		stripped := regexp.MustCompile(` data-tb-el="\d+"`).ReplaceAll(out, nil)
		if !bytes.Equal(stripped, []byte(doc)) {
			t.Fatalf("annotate not reversible for %q", doc)
		}
	})
}
