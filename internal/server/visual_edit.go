package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/lint"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

const maxVisualSaveBytes = 2 << 20 // 2 MiB

type visualEditData struct {
	Slug          string
	Page          string
	SlugJSON      template.JS
	PageJSON      template.JS
	HTMLJSON      template.JS
	CSSJSON       template.JS
	AssetsJSON    template.JS
	CanvasCSSJSON template.JS
	ThemeJSON     template.JS
}

type visualAsset struct {
	Src  string `json:"src"`
	Type string `json:"type"`
	Name string `json:"name"`
}

type visualSaveRequest struct {
	HTML string `json:"html"`
	CSS  string `json:"css"`
	Page string `json:"page"`
}

type visualSaveResponse struct {
	OK       bool     `json:"ok"`
	Warnings []string `json:"warnings,omitempty"`
}

func (s *Server) visualEditHandler(c *echo.Context) error {
	slug := c.Param("slug")
	page := c.QueryParam("page")
	if page == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "page is required")
	}
	err := validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	obj, err := s.store.Read(ctx, slug, page)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read page", err)
	}
	if obj.Content == "" {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("page %q not found", page))
	}

	bodyHTML, css, err := splitPage(obj.Content)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "parse page", err)
	}

	// The GrapesJS canvas is its own iframe, so it needs the site's compiled
	// stylesheet loaded explicitly (the editor page's own /app.css only styles
	// the chrome). Load it from the site's own host and mirror the page's
	// data-theme so the canvas renders exactly like the published page.
	theme, _ := readThemeAttribute(obj.Content)
	canvasCSS := s.siteURL(c, slug, "/app.css")

	files, err := s.store.List(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list files", err)
	}
	_, assetPaths := build.SplitFilesByKind(files)
	assets := make([]visualAsset, 0, len(assetPaths))
	for _, p := range assetPaths {
		assets = append(assets, visualAsset{
			Src:  fmt.Sprintf("http://%s.%s:%s/%s", slug, s.domain, s.port, p),
			Type: "image",
			Name: p,
		})
	}

	return s.render(c, "visual_edit", visualEditData{
		Slug:          slug,
		Page:          page,
		SlugJSON:      toJSONLiteral(slug),
		PageJSON:      toJSONLiteral(page),
		HTMLJSON:      toJSONLiteral(bodyHTML),
		CSSJSON:       toJSONLiteral(css),
		AssetsJSON:    toJSONLiteral(assets),
		CanvasCSSJSON: toJSONLiteral(canvasCSS),
		ThemeJSON:     toJSONLiteral(theme),
	})
}

func (s *Server) visualEditSaveHandler(c *echo.Context) error {
	slug := c.Param("slug")

	reader := http.MaxBytesReader(c.Response(), c.Request().Body, maxVisualSaveBytes)
	defer func() { _ = reader.Close() }()

	var req visualSaveRequest
	err := json.NewDecoder(reader).Decode(&req)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	if req.Page == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "page is required")
	}
	err = validatePage(req.Page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	obj, err := s.store.Read(ctx, slug, req.Page)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read original", err)
	}
	if obj.Content == "" {
		return echo.NewHTTPError(http.StatusNotFound, fmt.Sprintf("page %q not found", req.Page))
	}

	assembled, err := assemblePage(obj.Content, req.HTML, req.CSS)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "assemble page", err)
	}

	s.snapshotBefore(ctx, slug, snapshot.ReasonVisualSave)

	err = s.store.Write(ctx, slug, req.Page, assembled, "text/html; charset=utf-8", nil)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "write page", err)
	}

	meta := s.build.ReadMeta(ctx, slug)
	tmpl := build.EffectiveTemplate(meta)
	lintErrs := lint.App(ctx, s.store, slug, tmpl)
	warnings := make([]string, 0, len(lintErrs))
	for i := range lintErrs {
		warnings = append(warnings, lintErrs[i].Error())
	}

	slog.Info("visual_edit.save", "slug", slug, "page", req.Page, "bytes", len(assembled), "warnings", len(warnings))
	return c.JSON(http.StatusOK, visualSaveResponse{OK: true, Warnings: warnings}) //nolint:wrapcheck
}

// toJSONLiteral marshals v to JSON and returns it as template.JS so the
// html/template engine emits it verbatim inside a <script> block. This lets
// the editor template assign server-supplied values directly to JS variables
// without an intermediate JSON.parse step.
func toJSONLiteral(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return template.JS("null")
	}
	return template.JS(b) //nolint:gosec // values are JSON-marshaled, not user-controlled JS.
}

// splitPage extracts the editable parts of a stored HTML file: the inner
// HTML of <body> and the concatenated contents of every <style> tag in
// <head>. The rest of the document (doctype, meta, title, scripts) is left
// untouched by the editor and re-applied by assemblePage on save.
func splitPage(content string) (bodyHTML, css string, err error) {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return "", "", fmt.Errorf("parse html: %w", err)
	}

	headNode, bodyNode := findHeadBody(doc)

	var cssBuf strings.Builder
	if headNode != nil {
		for c := headNode.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode || c.Data != "style" {
				continue
			}
			for t := c.FirstChild; t != nil; t = t.NextSibling {
				if t.Type == html.TextNode {
					cssBuf.WriteString(t.Data)
				}
			}
		}
	}

	var bodyBuf bytes.Buffer
	if bodyNode != nil {
		for c := bodyNode.FirstChild; c != nil; c = c.NextSibling {
			err := html.Render(&bodyBuf, c)
			if err != nil {
				return "", "", fmt.Errorf("render body child: %w", err)
			}
		}
	}

	return bodyBuf.String(), cssBuf.String(), nil
}

// assemblePage re-emits the original document with the body contents and the
// <style> tag(s) in <head> swapped for the editor's new values. Everything
// else (doctype, title, meta, scripts, other head children) is preserved
// byte-for-byte from the original.
func assemblePage(original, newHTML, newCSS string) (string, error) {
	doc, err := html.Parse(strings.NewReader(original))
	if err != nil {
		return "", fmt.Errorf("parse html: %w", err)
	}

	headNode, bodyNode := findHeadBody(doc)

	if headNode != nil {
		var styles []*html.Node
		for c := headNode.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode && c.Data == "style" {
				styles = append(styles, c)
			}
		}
		for _, s := range styles {
			headNode.RemoveChild(s)
		}
		if newCSS != "" {
			styleNode := &html.Node{Type: html.ElementNode, Data: "style", DataAtom: 0}
			styleNode.AppendChild(&html.Node{Type: html.TextNode, Data: newCSS})
			headNode.AppendChild(styleNode)
		}
	}

	if bodyNode != nil {
		for c := bodyNode.FirstChild; c != nil; {
			next := c.NextSibling
			bodyNode.RemoveChild(c)
			c = next
		}
		fragments, err := html.ParseFragment(strings.NewReader(newHTML), bodyNode)
		if err != nil {
			return "", fmt.Errorf("parse body fragment: %w", err)
		}
		for _, f := range fragments {
			bodyNode.AppendChild(f)
		}
	}

	// DocumentNode itself isn't formatted (its children — doctype, html —
	// sit at column 0), so start the depth counter at -1 so <html> lands at
	// depth 0.
	prettyPrintBlockElements(doc, -1)

	var out bytes.Buffer
	err = html.Render(&out, doc)
	if err != nil {
		return "", fmt.Errorf("render document: %w", err)
	}
	return out.String(), nil
}

func findHeadBody(n *html.Node) (head, body *html.Node) {
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "head":
				head = n
			case "body":
				body = n
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return head, body
}

// blockContainers are HTML elements whose children are conventionally
// laid out on separate lines. We only inject indentation when every
// direct child is itself a block element or comment — mixed inline/text
// children would render with extra visible spaces if we touched them.
var blockContainers = map[string]bool{
	"html": true, "head": true, "body": true,
	"main": true, "header": true, "footer": true, "nav": true,
	"section": true, "article": true, "aside": true,
	"div": true, "ul": true, "ol": true, "dl": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true,
	"figure": true, "form": true, "fieldset": true, "details": true,
}

// blockChildren are element names that may appear as direct children of a
// block container without forcing us to leave the container's formatting
// alone. Inline elements and text deliberately aren't here.
var blockChildren = map[string]bool{
	"html": true, "head": true, "body": true,
	"main": true, "header": true, "footer": true, "nav": true,
	"section": true, "article": true, "aside": true,
	"div": true, "ul": true, "ol": true, "dl": true, "dt": true, "dd": true,
	"li": true, "p": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"table": true, "thead": true, "tbody": true, "tfoot": true, "tr": true,
	"th": true, "td": true, "caption": true, "colgroup": true, "col": true,
	"figure": true, "figcaption": true,
	"form": true, "fieldset": true, "legend": true, "details": true, "summary": true,
	"blockquote": true, "hr": true, "address": true,
	"link": true, "meta": true, "title": true, "style": true, "script": true,
	"base": true, "noscript": true,
}

// whitespaceSensitive marks elements whose descendants must be left
// byte-identical: their text content is rendered verbatim (pre, textarea)
// or visible whitespace would change visual output (code, kbd, samp).
var whitespaceSensitive = map[string]bool{
	"pre": true, "textarea": true, "script": true,
	"code": true, "kbd": true, "samp": true, "title": true,
}

// prettyPrintBlockElements walks the html.Node tree and injects newline +
// indentation TextNode children between block-level siblings, so that
// html.Render emits one block element per line. This restores stable line
// numbering after the visual editor saves a document whose body fragment
// came from GrapesJS without inter-tag whitespace.
//
// Containers whose direct children mix in text or inline elements are left
// untouched (otherwise we'd introduce visible whitespace between, e.g.,
// adjacent <span>s inside a <button>). Whitespace-sensitive subtrees are
// skipped entirely. Existing pure-whitespace TextNodes are stripped before
// injection so repeated saves stay idempotent.
func prettyPrintBlockElements(n *html.Node, depth int) {
	if n == nil {
		return
	}
	if n.Type == html.ElementNode && whitespaceSensitive[n.Data] {
		return
	}
	if n.Type == html.ElementNode && blockContainers[n.Data] && allChildrenAreBlock(n) {
		stripWhitespaceTextChildren(n)
		injectIndentation(n, depth)
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		prettyPrintBlockElements(c, depth+1)
	}
}

// allChildrenAreBlock reports whether every non-whitespace child of n is
// either a block-level element or a comment. Returns false for an empty
// container — there's nothing to format and we don't want <div></div> to
// expand to <div>\n</div>.
func allChildrenAreBlock(n *html.Node) bool {
	hasChild := false
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		switch c.Type {
		case html.ElementNode:
			if !blockChildren[c.Data] {
				return false
			}
			hasChild = true
		case html.CommentNode, html.DoctypeNode:
			hasChild = true
		case html.TextNode:
			if strings.TrimSpace(c.Data) != "" {
				return false
			}
		case html.ErrorNode, html.DocumentNode, html.RawNode:
			// These shouldn't appear as direct children of a parsed
			// element subtree; if they do, bail rather than guess.
			return false
		}
	}
	return hasChild
}

// stripWhitespaceTextChildren removes pure-whitespace TextNode children of
// n. Called before injectIndentation so repeated formatting passes don't
// stack newlines.
func stripWhitespaceTextChildren(n *html.Node) {
	c := n.FirstChild
	for c != nil {
		next := c.NextSibling
		if c.Type == html.TextNode && strings.TrimSpace(c.Data) == "" {
			n.RemoveChild(c)
		}
		c = next
	}
}

// injectIndentation inserts TextNode children carrying "\n" plus depth*2
// spaces of indentation: one before each existing child, and one final
// "\n" + (depth-1)*2 spaces before the closing tag so the close lines up
// with the opening one. Assumes pure-whitespace TextNodes have already
// been stripped.
func injectIndentation(n *html.Node, depth int) {
	if n.FirstChild == nil {
		return
	}
	childIndent := "\n" + strings.Repeat("  ", depth+1)
	closeIndent := "\n" + strings.Repeat("  ", depth)
	// Snapshot children before mutating; otherwise we walk the freshly-
	// inserted whitespace nodes too.
	var children []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		children = append(children, c)
	}
	for _, c := range children {
		n.InsertBefore(&html.Node{Type: html.TextNode, Data: childIndent}, c)
	}
	n.AppendChild(&html.Node{Type: html.TextNode, Data: closeIndent})
}
