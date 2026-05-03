package main

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"golang.org/x/net/html"
)

type LintError struct {
	File    string
	Message string
}

func (e *LintError) Error() string {
	return fmt.Sprintf("%s: %s", e.File, e.Message)
}

// lintApp validates all HTML files in a slug: checks HTML parse errors and
// ensures all relative href/src links resolve to existing files in the store.
func lintApp(ctx context.Context, store *Store, slug string) []LintError {
	files, err := store.List(ctx, slug)
	if err != nil {
		return []LintError{{File: slug, Message: fmt.Sprintf("failed to list files: %s", err)}}
	}

	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}

	var errs []LintError
	for _, file := range files {
		if !strings.HasSuffix(file, ".html") {
			continue
		}

		obj, err := store.Read(ctx, slug, file)
		if err != nil || obj.Content == "" {
			errs = append(errs, LintError{File: file, Message: "could not read file"})
			continue
		}

		parseErrs := checkHTMLLinks(file, obj.Content, fileSet)
		errs = append(errs, parseErrs...)
	}

	if len(errs) > 0 {
		slog.Warn("lint.app.errors", "slug", slug, "count", len(errs))
		for _, e := range errs {
			slog.Warn("lint.error", "slug", slug, "file", e.File, "message", e.Message)
		}
	} else {
		slog.Info("lint.app.ok", "slug", slug)
	}

	return errs
}

// checkHTMLLinks parses the HTML and checks all relative href/src attributes
// against the known file set. External URLs (http/https/mailto/etc.) are skipped.
func checkHTMLLinks(filename, content string, fileSet map[string]bool) []LintError {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return []LintError{{File: filename, Message: fmt.Sprintf("HTML parse error: %s", err)}}
	}

	dir := path.Dir(filename)
	var errs []LintError

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			errs = append(errs, checkNodeLinks(filename, dir, n, fileSet)...)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return errs
}

func checkNodeLinks(filename, dir string, n *html.Node, fileSet map[string]bool) []LintError {
	var errs []LintError
	for _, attr := range n.Attr {
		if attr.Key != "href" && attr.Key != "src" && attr.Key != "action" {
			continue
		}
		err := checkLink(filename, dir, attr.Val, fileSet)
		if err != nil {
			errs = append(errs, *err)
		}
	}
	return errs
}

func checkLink(filename, dir, rawVal string, fileSet map[string]bool) *LintError {
	link := strings.TrimSpace(rawVal)
	if link == "" || link == "#" || isExternalLink(link) {
		return nil
	}
	if i := strings.IndexByte(link, '#'); i != -1 {
		link = link[:i]
	}
	if i := strings.IndexByte(link, '?'); i != -1 {
		link = link[:i]
	}
	if link == "" {
		return nil
	}
	resolved := path.Join(dir, link)
	if fileSet[resolved] || fileSet[resolved+".html"] || fileSet[path.Join(resolved, "index.html")] {
		return nil
	}
	return &LintError{
		File:    filename,
		Message: fmt.Sprintf("broken link %q (resolved to %q)", rawVal, resolved),
	}
}

func isExternalLink(link string) bool {
	lower := strings.ToLower(link)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "mailto:") ||
		strings.HasPrefix(lower, "tel:") ||
		strings.HasPrefix(lower, "//") ||
		strings.HasPrefix(lower, "data:")
}
