package lint

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// This file owns the form data-loss checks: the ways a form can look like it
// works while silently discarding what visitors type. A non-technical owner
// only discovers these when the leads stop arriving — lint catches them at
// build time instead. The platform fact behind the multipart check lives in
// internal/server/api.go (buildSandboxRequest): /api/ functions parse only
// URL-encoded and JSON bodies, never multipart/form-data.

// attrVal returns an element's attribute value, "" when absent.
func attrVal(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// hasAttr reports attribute presence — needed for boolean attributes like
// disabled, where an empty value still counts.
func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// checkForms runs the per-form data-loss checks plus the anywhere-on-page
// file-input check. (Whether a form's action target exists is checkHTMLLinks'
// job — action is one of the attributes it already validates.)
func checkForms(pi pageInfo) []Error {
	var errs []Error
	for _, n := range pi.elements {
		switch n.Data {
		case "form":
			errs = append(errs, checkOneForm(pi.name, n)...)
		case "input":
			if strings.EqualFold(strings.TrimSpace(attrVal(n, "type")), "file") {
				errs = append(errs, multipartError(pi.name, `<input type="file">`))
			}
		}
	}
	return errs
}

// checkOneForm flags a POST form with no destination, a multipart enctype,
// and — for forms that actually submit somewhere (non-empty action) —
// controls whose values the browser will silently drop because they have no
// name. Forms without an action are left alone: inline-JS-handled forms
// (e.g. an onsubmit that returns false) are a legitimate pattern.
func checkOneForm(filename string, form *html.Node) []Error {
	action := strings.TrimSpace(attrVal(form, "action"))
	method := strings.TrimSpace(attrVal(form, "method"))

	var errs []Error
	if strings.EqualFold(method, "post") && action == "" {
		errs = append(errs, Error{
			File:    filename,
			Kind:    KindFormPostNoAction,
			Message: `form posts nowhere — this <form method="post"> has no action, so the browser posts the data back to the HTML page itself, where it is discarded (static pages cannot receive posts). Point action at a function route (e.g. action="/api/submit" backed by functions/submit.js), or if inline JavaScript handles the submit, remove method="post" and keep the onsubmit handler returning false.`,
		})
	}
	if strings.Contains(strings.ToLower(attrVal(form, "enctype")), "multipart/form-data") {
		errs = append(errs, multipartError(filename, `enctype="multipart/form-data"`))
	}
	if action == "" {
		return errs
	}
	return append(errs, checkFormControlNames(filename, form, action)...)
}

// checkFormControlNames flags the controls of one submitting form whose
// values the browser will silently drop because they carry no name.
func checkFormControlNames(filename string, form *html.Node, action string) []Error {
	var unnamed []string
	var named []string
	WalkDOM(form, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		switch n.Data {
		case "input", "select", "textarea":
		default:
			return
		}
		typ := strings.ToLower(strings.TrimSpace(attrVal(n, "type")))
		if n.Data == "input" && (typ == "submit" || typ == "button" || typ == "reset" || typ == "image") {
			return
		}
		if hasAttr(n, "disabled") {
			return
		}
		if name := strings.TrimSpace(attrVal(n, "name")); name != "" {
			named = append(named, name)
			return
		}
		desc := "<" + n.Data
		if typ != "" {
			desc += fmt.Sprintf(" type=%q", typ)
		}
		unnamed = append(unnamed, desc+">")
	})

	sort.Strings(named)
	namedList := "none — every control here is unnamed"
	if len(named) > 0 {
		namedList = capList(named, maxListedItems)
	}
	errs := make([]Error, 0, len(unnamed))
	for _, desc := range unnamed {
		errs = append(errs, Error{
			File: filename,
			Kind: KindFormControlUnnamed,
			Message: fmt.Sprintf(
				`form control will not submit — this %s inside the form posting to %q has no name attribute, so the browser silently drops its value from the submission and the handler never receives it. Named controls in this form: %s. Add a name attribute matching what the handler reads (e.g. name="email"), or remove the control if it shouldn't submit.`,
				desc, action, namedList),
		})
	}
	return errs
}

func multipartError(filename, what string) Error {
	return Error{
		File: filename,
		Kind: KindMultipartForm,
		Message: fmt.Sprintf(
			`file upload won't reach the handler — %s sends multipart/form-data, but /api/ form handlers only parse URL-encoded and JSON submissions, so the uploaded file (and the rest of the form) arrives unreadable and the data is lost. Remove the file input and enctype (collect a URL or text instead), or handle the file entirely in browser JavaScript without submitting it.`,
			what),
	}
}

// checkFetchTargets validates the string-literal URLs a page's inline
// scripts pass to fetch(). The same resolution and exemptions as the
// href/src check apply (resolveSiteTarget), so a fetch to a missing /api/
// function or a missing file fails lint instead of 404ing at runtime — the
// failure mode where a form looks wired up but every submission dies in the
// browser console. Each distinct URL is reported once per page.
func checkFetchTargets(pi pageInfo, facts jsFacts, lc linkCheckContext) []Error {
	var errs []Error
	seen := map[string]bool{}
	for _, ref := range facts.fetchTargets {
		if seen[ref.value] {
			continue
		}
		seen[ref.value] = true
		resolved, ok, skip := resolveSiteTarget(pi.dir, ref.value, lc)
		if skip || ok {
			continue
		}
		errs = append(errs, Error{
			File:    pi.name,
			Kind:    KindBrokenFetch,
			Message: brokenFetchMessage(ref, resolved, lc),
		})
	}
	return errs
}

func brokenFetchMessage(ref jsRef, resolved string, lc linkCheckContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "broken fetch %q in inline <script> #%d (resolved to %q) — no such file in the site.", ref.value, ref.ordinal, resolved)
	if name, isAPI := strings.CutPrefix(strings.TrimSpace(ref.value), "/api/"); isAPI {
		if i := strings.IndexAny(name, "?#"); i != -1 {
			name = name[:i]
		}
		fmt.Fprintf(&b, " /api/ routes are served by functions/{name}.js, and functions/%s.js does not exist. Create functions/%s.js exporting a handler (module.exports = function(request) {...}), or fix the fetch URL.", name, name)
		return b.String()
	}
	if linkable := linkableFiles(lc.fileSet); len(linkable) > 0 {
		fmt.Fprintf(&b, " Site files: %s.", capList(linkable, maxListedItems))
	}
	b.WriteString(" Point the fetch at an existing file or create it.")
	return b.String()
}
