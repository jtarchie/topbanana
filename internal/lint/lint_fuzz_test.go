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
		`<a href="#missing">x</a>`,
		`<a href="index.html#frag">x</a>`,
		`<div id="x"><a href="#x">ok</a></div>`,
		`<a href="#%zz">bad escape</a>`,
		`<a href="?q=1#frag">query then fragment</a>`,
		`<head><title></title></head>`,
		`<html lang=""><head><meta charset></head></html>`,
		`<form method="post"><input></form>`,
		`<input type="file">`,
		`<script>fetch('/api/x')</script>`,
		`<div onclick="ghost()"></div>`,
		`<a href="mailto:">contact</a>`,
		`<a href="tel:abc">call</a>`,
		`<label for="nope">x</label>`,
		`<script>document.getElementById('missing')</script>`,
		`<script src="http://evil.example/x.js"></script>`,
		`<link rel="stylesheet" href="https://fonts.example.com/css">`,
		`<svg xmlns="http://www.w3.org/2000/svg"><use xlink:href="#i"/></svg>`,
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
		pi := collectPageInfo("index.html", doc)
		facts := collectJSFacts("index.html", pi.scripts)
		_ = checkForms(pi)
		_ = checkFetchTargets(pi, facts, linkCheckContext{fileSet: fileSet})
		_ = checkDeadInteractions(pi, facts)
		_ = checkExternalResources(pi)
		_ = checkUnreferencedPages([]pageInfo{pi}, map[string]jsFacts{"index.html": facts}, []string{raw}, nil, linkCheckContext{fileSet: fileSet})
		_ = checkHTMLLinks("index.html", doc, linkCheckContext{fileSet: fileSet, enablesFns: false})
		_ = checkHTMLLinks("index.html", doc, linkCheckContext{fileSet: fileSet, enablesFns: true})
		_ = checkInlineJS("index.html", pi.scripts)
		_ = suspiciousAttrValues("index.html", doc)
		_ = checkDesignSubstrate("index.html", doc)
		_ = checkMobileViewport("index.html", doc)
		_ = checkHeadHygiene(pi)
		_ = checkAnchors([]pageInfo{pi}, linkCheckContext{fileSet: fileSet})
		_ = checkDuplicateTitles([]pageInfo{pi})
		_, _ = AutoFixCharset(raw)
	})
}
