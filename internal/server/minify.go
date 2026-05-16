package server

import (
	"fmt"

	"github.com/tdewolff/minify/v2"
	minifyhtml "github.com/tdewolff/minify/v2/html"
)

// newHTMLMinifier returns a *minify.M configured for HTML responses served
// from S3-backed sites. Defaults strip whitespace and comments without
// touching document structure or quoting. Inline <style>/<script> blocks
// are left alone — we don't register CSS or JS minifiers because the
// agent's inline JS is already validated by goja during lint, and
// re-minifying CSS the agent wrote risks subtle behaviour changes we
// don't want introduced at serve time.
func newHTMLMinifier() *minify.M {
	m := minify.New()
	m.Add("text/html", &minifyhtml.Minifier{
		// Keep <html>/<head>/<body> — browsers can infer them, but tooling
		// (view-source, crawlers, our own injectEditToolbar) is much more
		// predictable when the structural tags are present.
		KeepDocumentTags: true,
		// Preserve closing tags. The agent emits semantic HTML throughout,
		// and our HTML lint walks the parsed DOM expecting closing tags to
		// land where the source put them.
		KeepEndTags: true,
		// Always quote attribute values. Saves a few bytes to drop quotes
		// where the spec allows it, but the savings aren't worth the risk
		// of attribute-parsing ambiguities on edge-case values (URLs with
		// query strings, JSON in data-* attributes, etc.).
		KeepQuotes: true,
	})
	return m
}

// minifyHTMLBody runs htmlContent through the configured HTML minifier.
// On error, returns the original content plus the error — callers serve
// the original (a minifier hiccup must never fail a page response) and
// log the error as a signal that something about the HTML is unusual
// enough to confuse tdewolff/parse, which is a finding worth surfacing.
func minifyHTMLBody(m *minify.M, htmlContent string) (string, error) {
	out, err := m.String("text/html", htmlContent)
	if err != nil {
		return htmlContent, fmt.Errorf("minify html: %w", err)
	}
	return out, nil
}
