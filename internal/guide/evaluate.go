package guide

import (
	"context"
	"log/slog"
	"strings"

	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// Evaluate runs the template's declared guide items against the site's stored
// HTML and returns a Report for the manage page. It degrades gracefully and
// never errors: a nil template or one with no guide yields an empty Report (the
// card is hidden); a list/read failure yields an empty Report (the manage page
// must never 500 over the guide); an unparseable page is skipped.
//
// tmpl should be the template intrinsic to the site type (templates.Get on the
// stored template id), not a per-site functions override — guide items describe
// the type, not the runtime capability.
func Evaluate(ctx context.Context, s *store.Store, slug string, tmpl *templates.SiteTemplate) Report {
	if tmpl == nil || len(tmpl.Guide) == 0 {
		return Report{}
	}

	files, err := s.List(ctx, slug)
	if err != nil {
		slog.Warn("guide.list_failed", "slug", slug, "err", err)
		return Report{}
	}

	var pages []parsedPage
	byName := make(map[string]parsedPage)
	for _, f := range files {
		if !strings.HasSuffix(f, ".html") {
			continue
		}
		obj, err := s.Read(ctx, slug, f)
		if err != nil || obj == nil || obj.Content == "" {
			continue
		}
		doc, parseErr := html.Parse(strings.NewReader(obj.Content))
		if parseErr != nil {
			continue
		}
		pp := parsedPage{Path: f, Doc: doc}
		pages = append(pages, pp)
		byName[f] = pp
	}

	results := make([]Result, 0, len(tmpl.Guide))
	present := 0
	for _, item := range tmpl.Guide {
		page := item.Page
		if page == "" {
			page = "index.html"
		}

		isPresent := false
		if det, ok := detectors[item.Detector]; ok {
			isPresent = runDetector(det, item, page, pages, byName)
		} else {
			// Shipped templates can't reach here — the templates test asserts
			// every guide item references a known detector — so this only
			// guards a typo during development.
			slog.Warn("guide.unknown_detector", "slug", slug, "item", item.ID, "detector", item.Detector)
		}
		if isPresent {
			present++
		}
		results = append(results, Result{
			Item:         item,
			Present:      isPresent,
			WorkspaceURL: "/workspace/" + slug + "?page=" + page,
		})
	}

	return Report{Results: results, Present: present, Total: len(results)}
}

// runDetector applies a guide item's Scope: every-page runs the detector per
// page and requires all pass; specific-file inspects only the item's page;
// the default (any-page) hands the detector the whole page set.
func runDetector(det detector, item templates.GuideItem, page string, pages []parsedPage, byName map[string]parsedPage) bool {
	switch item.Scope {
	case ScopeEvery:
		if len(pages) == 0 {
			return false
		}
		for _, pg := range pages {
			if !det(item.Params, []parsedPage{pg}) {
				return false
			}
		}
		return true
	case ScopeFile:
		pg, ok := byName[page]
		if !ok {
			return false
		}
		return det(item.Params, []parsedPage{pg})
	default:
		return det(item.Params, pages)
	}
}
