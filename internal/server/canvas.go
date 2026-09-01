package server

// The workspace ("Canvas"): the site itself is the interface. The page renders
// the site full-bleed through the /s/:slug path in edit mode — same origin as
// the admin UI, so the injected selection script can report element addresses
// (see internal/domaddr) back to the floating command bar. It replaced the
// classic form-and-preview workspace at /workspace/:slug; the non-visual tools
// that lived in that page's rail (validate, export, server functions) moved to
// the Settings tab, and the two that drive the live preview (Themes, Version
// history) are slide-overs here.

import (
	"context"
	"errors"
	"fmt"
	gohtml "html"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/domaddr"
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

type canvasData struct {
	Chrome
	Page  string
	Pages []string
	// SiteHost is the address the site answers on, shown in the top bar's
	// centre slot whenever no run is narrating there.
	SiteHost string
	// Stylesheets are the sheets the site authored for itself. They are not
	// navigable — there is nothing to render in the frame — so the page menu
	// offers them as a composer *scope* instead: picking one sends `page` on
	// the run, the same file-scoping field the classic workspace used.
	Stylesheets []string
	// PreviewBaseJSON is the mount the canvas loads site content from:
	// /s/{slug} normally, or the tokenized /sp mount for private sites so the
	// sandboxed (cookie-less) document's subresources pass the private gate.
	// Only the JS form is rendered — the page reaches the mount through the
	// script's reloadFrame(), never as a markup attribute.
	PreviewBaseJSON template.JS
	EditURL         template.URL
	SlugJSON        template.JS
	PageJSON        template.JS
	// Building is true when a run is already in flight for this slug (started
	// from MCP or another tab). The canvas reattaches to it on load — without
	// this the page renders an idle composer that 409s on submit, and the
	// in-flight run's completion never updates the page.
	Building bool

	// Flash is a SENTINEL from a redirect (restore, snapshot delete, relint),
	// mapped to copy in the template — never echoed text.
	Flash string

	// Themes panel. The gallery stages a theme by postMessage into the edit
	// frame (canvas.js applies it to the live document) and only writes on
	// Apply, so the picker needs the site's current theme to mark "applied".
	CurrentTheme     string
	CurrentThemeJSON template.JS
	ThemesJSON       template.JS

	// Version history panel.
	Snapshots []historyRow
}

// resolveScopeBlock turns the canvas's element selection (page + address)
// into the agent-facing context block, resolved entirely from the stored
// source via domaddr — the client contributes only two small numbers/names,
// both validated here. Empty scope_el means an unscoped edit. A stale
// address (the page changed since it was served) is a user-facing error that
// tells them to reselect, not a silent mis-scope.
func (s *Server) resolveScopeBlock(ctx context.Context, slug, scopePage, scopeEl string) (string, error) {
	scopeEl = strings.TrimSpace(scopeEl)
	if scopeEl == "" {
		return "", nil
	}
	n, err := strconv.Atoi(scopeEl)
	if err != nil || n < 0 {
		return "", errors.New("the selection is invalid — click the element again")
	}
	err = validatePage(scopePage)
	if err != nil || scopePage == "" {
		return "", errors.New("the selection is missing its page — click the element again")
	}
	obj, err := s.store.Read(ctx, slug, scopePage)
	if err != nil || obj == nil || obj.Content == "" {
		return "", errors.New("the selected page no longer exists — reload and try again")
	}
	outer, err := domaddr.OuterHTML([]byte(obj.Content), n)
	if err != nil {
		return "", errors.New("the page changed since you selected — click the element again")
	}
	markup := string(outer)
	if len(markup) > maxScopeBytes {
		markup = strings.ToValidUTF8(markup[:maxScopeBytes], "") + "…"
	}
	return fmt.Sprintf(
		"The user selected one specific element on %s and the change applies to THAT element only — not to other elements with similar content. It is element #%d of the page in document order. Its current source markup:\n\n```html\n%s\n```",
		scopePage, n, markup), nil
}

// maxDirectTextBytes caps a single in-place text edit. Text nodes are prose,
// not documents; anything larger belongs to the agent path.
const maxDirectTextBytes = 8 * 1024

// textEditHandler is the canvas's direct text edit: replace one text node,
// addressed as (page, element, direct-text-index), with new text — no LLM,
// no lint loop, instant. The write is deterministic through domaddr.TextSpan
// and guarded by an expect-text compare, so a page that changed since the
// user started typing yields a clear conflict instead of a silent clobber —
// and because the address names an element, duplicated content elsewhere on
// the page is never touched.
// textEditInput is the validated form of a direct text edit request.
type textEditInput struct {
	page      string
	el        int
	textIndex int
	text      string
	expect    string
}

// parseTextEditInput validates the request form; every failure is a
// user-facing echo error with recovery guidance.
func parseTextEditInput(c *echo.Context) (textEditInput, error) {
	var in textEditInput
	in.page = c.FormValue("page")
	err := validatePage(in.page)
	if err != nil || in.page == "" {
		return in, echo.NewHTTPError(http.StatusBadRequest, "the edited page is missing — reload and try again")
	}
	in.el, err = strconv.Atoi(c.FormValue("el"))
	if err != nil || in.el < 0 {
		return in, echo.NewHTTPError(http.StatusBadRequest, "the selection is invalid — reload and try again")
	}
	in.textIndex, err = strconv.Atoi(c.FormValue("text_index"))
	if err != nil || in.textIndex < 0 {
		return in, echo.NewHTTPError(http.StatusBadRequest, "the selection is invalid — reload and try again")
	}
	in.text = c.FormValue("text")
	if len(in.text) > maxDirectTextBytes {
		return in, echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("that's too much text for an in-place edit (max %d bytes) — use the Describe box instead", maxDirectTextBytes))
	}
	in.expect = c.FormValue("expect")
	return in, nil
}

func (s *sitesController) textEditHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	in, err := parseTextEditInput(c)
	if err != nil {
		return err
	}
	if s.buildInFlight(slug) {
		return echo.NewHTTPError(http.StatusConflict, "the builder is working on this site — wait for it to finish")
	}

	ctx := c.Request().Context()
	obj, err := s.store.Read(ctx, slug, in.page)
	if err != nil || obj == nil || obj.Content == "" {
		return echo.NewHTTPError(http.StatusConflict, "the page no longer exists — reload and try again")
	}
	doc := []byte(obj.Content)
	span, err := domaddr.TextSpan(doc, in.el, in.textIndex)
	if err != nil {
		return echo.NewHTTPError(http.StatusConflict, "the page changed since you started editing — reload and try again")
	}
	// Compare decoded text: the browser edits &rsquo; as ’, so the stored
	// bytes and the browser's original only match after entity decoding.
	if gohtml.UnescapeString(string(doc[span.Start:span.End])) != in.expect {
		return echo.NewHTTPError(http.StatusConflict, "the page changed since you started editing — reload and try again")
	}

	before := obj.Content
	after := string(doc[:span.Start]) + gohtml.EscapeString(in.text) + string(doc[span.End:])

	s.snapshotBefore(ctx, slug, snapshot.ReasonText)
	err = s.store.Write(ctx, slug, in.page, after, obj.ContentType, nil)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "save text", err)
	}
	// Recorded like the MCP direct edits: visible in /debug, absent from the
	// run feed (it's an instant local change, not an agent request).
	editrec.RecordEdit(ctx, s.store, slug, "text", "text_edit", in.page, before, after)
	slog.Info("text_edit.save", "slug", slug, "page", in.page, "el", in.el, "text_index", in.textIndex, "user", callerEmail(c))
	return c.JSON(http.StatusOK, map[string]any{"ok": true}) //nolint:wrapcheck
}

func (s *sitesController) canvasHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	page := c.QueryParam("page")
	if page == "" {
		page = "index.html"
	}
	err = validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	all, err := s.store.List(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list pages", err)
	}
	pages, _ := build.SplitFilesByKind(all)
	meta := s.build.ReadMeta(ctx, slug)
	siteName := meta.Title
	if siteName == "" {
		siteName = slug
	}
	currentTheme := s.readCurrentTheme(ctx, slug)

	previewBase := "/s/" + url.PathEscape(slug)
	if meta.Private {
		previewBase = "/sp/" + url.PathEscape(slug) + "/" + mintPreviewToken(slug)
	}
	editURL := previewBase + "/" + page + "?tb_edit=1"

	return s.render(c, "canvas", canvasData{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: siteName,
			SiteURL:  s.siteURL(c, slug, "/"),
			Active:   "workspace",
		},
		Page:            page,
		Pages:           pages,
		SiteHost:        s.siteHost(c, slug),
		Stylesheets:     authorStylesheets(all),
		PreviewBaseJSON: toJSONLiteral(previewBase),
		EditURL:         template.URL(editURL), //nolint:gosec // built from the base and a validated page.
		SlugJSON:        toJSONLiteral(slug),
		PageJSON:        toJSONLiteral(page),
		// ?building=1 comes from the POST /build redirect and is trusted on
		// its own: the events tracker may not have registered the run yet, and
		// rendering an idle composer for a build that just started is the one
		// case buildInFlight can miss.
		Building: c.QueryParam("building") == "1" || s.buildInFlight(slug),

		Flash:            c.QueryParam("flash"),
		CurrentTheme:     currentTheme,
		CurrentThemeJSON: toJSONLiteral(currentTheme),
		ThemesJSON:       toJSONLiteral(daisyThemes),
		Snapshots:        s.listSnapshotRows(ctx, slug),
	})
}
