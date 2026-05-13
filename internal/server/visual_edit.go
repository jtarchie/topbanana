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

	"github.com/jtarchie/buildabear/internal/build"
	"github.com/jtarchie/buildabear/internal/lint"
	"github.com/jtarchie/buildabear/internal/snapshot"
)

const maxVisualSaveBytes = 2 << 20 // 2 MiB

type visualEditData struct {
	Slug       string
	Page       string
	SlugJSON   template.JS
	PageJSON   template.JS
	HTMLJSON   template.JS
	CSSJSON    template.JS
	AssetsJSON template.JS
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
		Slug:       slug,
		Page:       page,
		SlugJSON:   toJSONLiteral(slug),
		PageJSON:   toJSONLiteral(page),
		HTMLJSON:   toJSONLiteral(bodyHTML),
		CSSJSON:    toJSONLiteral(css),
		AssetsJSON: toJSONLiteral(assets),
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
