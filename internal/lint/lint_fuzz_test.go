package lint

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// FuzzHTMLLint feeds adversarial HTML through every pure check function in
// the lint package. The build retry loop calls these on every agent-emitted
// file, so a panic here would crash the build goroutine for one tenant and
// (because goroutines panic-bubble) eventually take down the server. The
// fuzz target only proves the absence of panics; semantic correctness is
// covered by the dedicated _test.go files.
func FuzzHTMLLint(f *testing.F) {
	seeds := []string{
		`<!doctype html><html><body><p>hi</p></body></html>`,
		`<html><head><link rel="stylesheet" href="/app.css"></head><body></body></html>`,
		`<a href="missing.html">x</a>`,
		`<script>alert(1)</script>`,
		`<a href="javascript:1">x</a>`,
		`<img src=" unclosed`,
		`<<<<<<>>>>>`,
		`<a href="/abs">x</a>`,
		`<a href="../escape.html">x</a>`,
		`<a href="日本.html">x</a>`,
		`<html><body><div onclick="x"></div></body></html>`,
		"",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	fileSet := map[string]bool{"index.html": true, "app.css": true}
	f.Fuzz(func(_ *testing.T, raw string) {
		doc, err := html.Parse(strings.NewReader(raw))
		if err != nil || doc == nil {
			return
		}
		_ = checkHTMLLinks("index.html", doc, fileSet, false)
		_ = checkHTMLLinks("index.html", doc, fileSet, true)
		_ = checkInlineJS("index.html", doc)
		_ = suspiciousAttrValues("index.html", doc)
		_ = checkDesignSubstrate("index.html", doc)
	})
}
