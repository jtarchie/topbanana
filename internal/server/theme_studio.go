package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"golang.org/x/net/html"

	"github.com/jtarchie/buildabear/internal/build"
	"github.com/jtarchie/buildabear/internal/lint"
	"github.com/jtarchie/buildabear/internal/snapshot"
)

// daisyTheme is one DaisyUI theme rendered as a card in the gallery. Swatch
// hexes are approximate previews — DaisyUI's real palette is loaded at
// runtime via its stylesheet, but the gallery picker only needs a glanceable
// signal, not pixel parity.
type daisyTheme struct {
	Name      string `json:"name"`
	Category  string `json:"category"`
	Primary   string `json:"primary"`
	Secondary string `json:"secondary"`
	Accent    string `json:"accent"`
	Base      string `json:"base"`
}

// daisyThemes is the source-of-truth allowlist. Order is the gallery order.
// Categories mirror agent_prompt.md so the studio's grouping matches the
// vocabulary the agent already reaches for.
var daisyThemes = []daisyTheme{
	{"light", "Professional", "#491eff", "#ff41c7", "#00cfbd", "#ffffff"},
	{"dark", "Professional", "#6419e6", "#d926a9", "#1fb2a6", "#1d232a"},
	{"corporate", "Professional", "#4b6bfb", "#7b92b2", "#67cba0", "#ffffff"},
	{"business", "Professional", "#1c4e80", "#7c909a", "#ea6947", "#202020"},
	{"winter", "Professional", "#047aff", "#463aa2", "#c148ac", "#ffffff"},
	{"cupcake", "Warm", "#65c3c8", "#ef9fbc", "#eeaf3a", "#faf7f5"},
	{"bumblebee", "Warm", "#f9d72f", "#e0a82e", "#181830", "#ffffff"},
	{"valentine", "Warm", "#e96d7b", "#a991f7", "#88dbdf", "#f0d6e8"},
	{"lemonade", "Warm", "#519903", "#e9e92f", "#af4670", "#f9f7e8"},
	{"pastel", "Warm", "#d1c1d7", "#f6cbd1", "#b4e9d6", "#ffffff"},
	{"autumn", "Warm", "#8c0327", "#d85251", "#d59b6a", "#f1f1f1"},
	{"synthwave", "Bold", "#e779c1", "#58c7f3", "#f3cc30", "#2d1b69"},
	{"cyberpunk", "Bold", "#ff7598", "#75d1f0", "#c07eec", "#ffee00"},
	{"dracula", "Bold", "#ff79c6", "#bd93f9", "#ffb86c", "#282a36"},
	{"night", "Bold", "#38bdf8", "#818cf8", "#f471b5", "#0f172a"},
	{"forest", "Bold", "#1eb854", "#1fd65f", "#d99330", "#171212"},
	{"coffee", "Bold", "#db924b", "#263e3f", "#10576d", "#20161f"},
	{"retro", "Bold", "#ef9995", "#a4cbb4", "#ebcb8b", "#ece3ca"},
	{"garden", "Earthy", "#5c7f67", "#ecf4e7", "#fae5e5", "#e9e7e7"},
	{"aqua", "Earthy", "#09ecf3", "#966fb3", "#ffe999", "#345da7"},
	{"wireframe", "Earthy", "#b8b8b8", "#b8b8b8", "#b8b8b8", "#ffffff"},
	{"nord", "Earthy", "#5e81ac", "#81a1c1", "#88c0d0", "#eceff4"},
	{"sunset", "Earthy", "#ff865b", "#fd6f9c", "#b387fa", "#1e293b"},
}

var daisyThemeSet = func() map[string]bool {
	m := make(map[string]bool, len(daisyThemes))
	for _, t := range daisyThemes {
		m[t.Name] = true
	}
	return m
}()

type themeApplyRequest struct {
	Theme string `json:"theme"`
}

type themeApplyResponse struct {
	OK       bool     `json:"ok"`
	Warnings []string `json:"warnings,omitempty"`
	Changed  int      `json:"changed"`
}

func (s *Server) themeStudioApplyHandler(c *echo.Context) error {
	slug := c.Param("slug")

	reader := http.MaxBytesReader(c.Response(), c.Request().Body, 4096)
	defer func() { _ = reader.Close() }()

	var req themeApplyRequest
	err := json.NewDecoder(reader).Decode(&req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	if !daisyThemeSet[req.Theme] {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("unknown theme %q", req.Theme))
	}

	ctx := c.Request().Context()
	all, err := s.store.List(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list files", err)
	}
	pages, _ := build.SplitFilesByKind(all)
	if len(pages) == 0 {
		return echo.NewHTTPError(http.StatusNotFound, "site has no pages")
	}

	// Two-pass: parse and rewrite every page in memory first. If any page
	// fails to parse, abort before writing — the site stays consistent.
	rewritten := make(map[string]string, len(pages))
	for _, page := range pages {
		obj, err := s.store.Read(ctx, slug, page)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "read "+page, err)
		}
		if obj == nil || obj.Content == "" {
			continue
		}
		out, err := setThemeAttribute(obj.Content, req.Theme)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "rewrite "+page, err)
		}
		rewritten[page] = out
	}

	s.snapshotBefore(ctx, slug, snapshot.ReasonThemeApply)

	for page, content := range rewritten {
		err := s.store.Write(ctx, slug, page, content, "text/html; charset=utf-8", nil)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "write "+page, err)
		}
	}

	meta := s.build.ReadMeta(ctx, slug)
	tmpl := build.EffectiveTemplate(meta)
	lintErrs := lint.App(ctx, s.store, slug, tmpl)
	warnings := make([]string, 0, len(lintErrs))
	for i := range lintErrs {
		warnings = append(warnings, lintErrs[i].Error())
	}

	slog.Info("theme_studio.apply", "slug", slug, "theme", req.Theme, "pages", len(rewritten), "warnings", len(warnings))
	return c.JSON(http.StatusOK, themeApplyResponse{ //nolint:wrapcheck
		OK:       true,
		Warnings: warnings,
		Changed:  len(rewritten),
	})
}

// daisyThemesHref is the CDN URL for DaisyUI's full theme palette. The base
// daisyui@5 stylesheet only ships light/dark; without this companion every
// other data-theme value (synthwave, cupcake, …) renders with default
// colors because its CSS variables are never defined.
const daisyThemesHref = "https://cdn.jsdelivr.net/npm/daisyui@5/themes.css"

// setThemeAttribute returns content with the <html> element's data-theme
// attribute set to theme. If the attribute is absent it's added. If the
// page already loads the base daisyui@5 stylesheet but is missing the
// themes companion, the themes <link> is inserted immediately after the
// base — older sites built before that link was required get fixed up the
// first time Theme Studio touches them. The rest of the document (doctype,
// head, body, scripts, attribute order on other tags) is preserved.
func setThemeAttribute(content, theme string) (string, error) {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return "", fmt.Errorf("parse html: %w", err)
	}

	htmlNode := findHTMLNode(doc)
	if htmlNode == nil {
		return "", errors.New("no <html> element")
	}

	found := false
	for i := range htmlNode.Attr {
		if htmlNode.Attr[i].Key == "data-theme" {
			htmlNode.Attr[i].Val = theme
			found = true
			break
		}
	}
	if !found {
		htmlNode.Attr = append(htmlNode.Attr, html.Attribute{Key: "data-theme", Val: theme})
	}

	ensureDaisyThemesLink(doc)

	var out bytes.Buffer
	err = html.Render(&out, doc)
	if err != nil {
		return "", fmt.Errorf("render document: %w", err)
	}
	return out.String(), nil
}

// ensureDaisyThemesLink inserts a themes.css <link> immediately after the
// base daisyui@5 <link> when the page is missing it. No-op when the themes
// link is already present, or when the base daisyui link is absent (that's
// a separate problem the lint flags — Theme Studio shouldn't bootstrap the
// design substrate from scratch).
func ensureDaisyThemesLink(doc *html.Node) {
	var baseLink *html.Node
	var hasThemes bool

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if hasThemes {
			return
		}
		if n.Type == html.ElementNode && n.Data == "link" {
			for _, a := range n.Attr {
				if a.Key != "href" {
					continue
				}
				switch {
				case strings.Contains(a.Val, "daisyui@5/themes.css"):
					hasThemes = true
					return
				case strings.Contains(a.Val, "cdn.jsdelivr.net/npm/daisyui") && baseLink == nil:
					baseLink = n
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	if hasThemes || baseLink == nil {
		return
	}

	themesLink := &html.Node{
		Type: html.ElementNode,
		Data: "link",
		Attr: []html.Attribute{
			{Key: "href", Val: daisyThemesHref},
			{Key: "rel", Val: "stylesheet"},
			{Key: "type", Val: "text/css"},
		},
	}
	parent := baseLink.Parent
	if parent == nil {
		return
	}
	parent.InsertBefore(themesLink, baseLink.NextSibling)
}

// readThemeAttribute returns the data-theme value on the <html> element, or
// empty string if absent or unparseable.
func readThemeAttribute(content string) (string, error) {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return "", fmt.Errorf("parse html: %w", err)
	}
	htmlNode := findHTMLNode(doc)
	if htmlNode == nil {
		return "", nil
	}
	for _, a := range htmlNode.Attr {
		if a.Key == "data-theme" {
			return a.Val, nil
		}
	}
	return "", nil
}

func findHTMLNode(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "html" {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if found := findHTMLNode(c); found != nil {
			return found
		}
	}
	return nil
}
