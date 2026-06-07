package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// renderAuthPage parses the embedded layout + the named auth template the same
// way New does (layout first so its shared partials — head, brand_minimal,
// footer, webauthn_js — resolve) and executes it. Deterministic: no Minio or
// HTTP server, so it runs in plain CI.
func renderAuthPage(t *testing.T, name, body string, data any) string {
	t.Helper()
	tpl := template.New("")
	template.Must(tpl.Parse(layoutTemplate))
	template.Must(tpl.New(name).Parse(body))
	var buf bytes.Buffer
	err := tpl.ExecuteTemplate(&buf, name, data)
	if err != nil {
		t.Fatalf("execute %s template: %v", name, err)
	}
	return buf.String()
}

// TestLoginPage_RendersWithSharedFooter is a regression guard. The shared
// footer reads `{{ if .Year }}`, and html/template errors on a missing struct
// field even inside an `if` — so a page-data struct that renders the footer
// MUST embed Chrome (which carries Year). loginData briefly didn't, and every
// /login render 500'd. Executing the template against the real struct fails
// loudly if the embed is dropped again.
func TestLoginPage_RendersWithSharedFooter(t *testing.T) {
	t.Parallel()
	html := renderAuthPage(t, "login", loginTemplate, loginData{Chrome: Chrome{Active: "login"}})
	if !strings.Contains(html, "Sign in") {
		t.Errorf("login page missing its heading")
	}
	if !strings.Contains(html, "Top Banana") {
		t.Errorf("login page missing the shared footer")
	}
}

// TestRegisterPage_RendersWithSharedFooter is the same guard for registerData.
func TestRegisterPage_RendersWithSharedFooter(t *testing.T) {
	t.Parallel()
	html := renderAuthPage(t, "register", registerTemplate, registerData{
		Chrome:      Chrome{Active: "register"},
		Email:       "newcomer@example.com",
		InviteToken: "tok-123",
	})
	if !strings.Contains(html, "newcomer@example.com") {
		t.Errorf("register page missing the enrolling email")
	}
	if !strings.Contains(html, "Top Banana") {
		t.Errorf("register page missing the shared footer")
	}
}

// Compile-time guard: both auth page-data structs must satisfy chromed so
// render()'s injectChrome can populate .Year / .IsSuperAdmin. Dropping the
// embedded Chrome breaks the build here rather than at runtime.
var (
	_ chromed = (*loginData)(nil)
	_ chromed = (*registerData)(nil)
)
