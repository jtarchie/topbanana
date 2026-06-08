package build

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const pageWithCDNSubstrate = `<!DOCTYPE html>
<html data-theme="synthwave">
<head>
<meta charset="utf-8">
<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />
<link href="https://cdn.jsdelivr.net/npm/daisyui@5/themes.css" rel="stylesheet" type="text/css" />
<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>
</head>
<body class="min-h-screen"><button class="btn btn-primary">Go</button></body>
</html>`

func TestSwapSubstrateForLocalCSS(t *testing.T) {
	t.Parallel()

	out, changed := swapSubstrateForLocalCSS(pageWithCDNSubstrate)
	if !changed {
		t.Fatal("expected changed=true for a page carrying CDN substrate tags")
	}
	if strings.Contains(out, "cdn.jsdelivr.net/npm/daisyui") {
		t.Error("daisyUI CDN <link> survived the swap")
	}
	if strings.Contains(out, "@tailwindcss/browser") {
		t.Error("Tailwind browser <script> survived the swap")
	}
	if !strings.Contains(out, `<link rel="stylesheet" href="/app.css">`) {
		t.Errorf("local /app.css link not injected:\n%s", out)
	}
	if !strings.Contains(out, `name="viewport"`) || !strings.Contains(out, "width=device-width") {
		t.Errorf("responsive viewport meta not injected:\n%s", out)
	}
	if !strings.Contains(out, `data-theme="synthwave"`) {
		t.Error("swap should preserve the rest of the document (data-theme lost)")
	}

	// Idempotent: feeding the optimized page back changes nothing.
	again, changed2 := swapSubstrateForLocalCSS(out)
	if changed2 {
		t.Errorf("swap not idempotent; second pass changed the content:\n%s", again)
	}
}

func TestSwapSubstrateForLocalCSS_AlreadyLocal(t *testing.T) {
	t.Parallel()

	// Already carries both the viewport meta and /app.css with no CDN tags, so
	// there is nothing left to inject or strip.
	page := `<html><head><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"></head><body class="btn"></body></html>`
	_, changed := swapSubstrateForLocalCSS(page)
	if changed {
		t.Error("expected changed=false for a complete page (viewport + /app.css, no CDN tags)")
	}
}

// TestSwapSubstrateForLocalCSS_InjectsViewport pins the MCP/web parity: a page
// that already links /app.css but omits the viewport meta still gets the
// responsive viewport injected.
func TestSwapSubstrateForLocalCSS_InjectsViewport(t *testing.T) {
	t.Parallel()

	page := `<html><head><link rel="stylesheet" href="/app.css"></head><body class="btn"></body></html>`
	out, changed := swapSubstrateForLocalCSS(page)
	if !changed {
		t.Fatal("expected changed=true: the viewport meta is missing")
	}
	if !strings.Contains(out, "width=device-width") {
		t.Errorf("responsive viewport meta not injected:\n%s", out)
	}
	if strings.Count(out, `href="/app.css"`) != 1 {
		t.Errorf("must not duplicate the /app.css link:\n%s", out)
	}
	again, changed2 := swapSubstrateForLocalCSS(out)
	if changed2 {
		t.Errorf("swap not idempotent after viewport injection:\n%s", again)
	}
}

func TestResolveTailwindCLI_Override(t *testing.T) {
	t.Parallel()

	svc := &Service{tailwindCLI: "/opt/tw/tailwindcss"}
	name, args, ok := svc.resolveTailwindCLI()
	if !ok || name != "/opt/tw/tailwindcss" || len(args) != 0 {
		t.Fatalf("override resolution = (%q, %v, %v), want (/opt/tw/tailwindcss, [], true)", name, args, ok)
	}
}

// writeStubCLI writes an executable shell script that emulates the Tailwind
// CLI: it locates the -o argument and writes fixed CSS there. When fail is
// true it exits non-zero without writing, to exercise graceful degradation.
func writeStubCLI(t *testing.T, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tailwindcss")
	body := "#!/bin/sh\n"
	if fail {
		body += "echo 'boom' >&2\nexit 1\n"
	} else {
		body += `out=""
while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; shift 2; else shift; fi
done
printf '%s' '/* stub */.btn{color:red}' > "$out"
`
	}
	err := os.WriteFile(path, []byte(body), 0o755)
	if err != nil {
		t.Fatalf("write stub cli: %v", err)
	}
	return path
}

func TestOptimizeCSS_WithStubCLI(t *testing.T) {
	st := minioStoreForBuild(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run optimizeCSS integration tests")
	}
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	err := st.Write(ctx, slug, "index.html", pageWithCDNSubstrate, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}

	svc := NewWithConfig(Config{Store: st, TailwindCLI: writeStubCLI(t, false)})
	svc.OptimizeCSS(ctx, slug)

	css, err := st.Read(ctx, slug, "app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	if !strings.Contains(css.Content, "/* stub */") {
		t.Errorf("app.css not written by compile; got %q", css.Content)
	}
	if !strings.HasPrefix(css.ContentType, "text/css") {
		t.Errorf("app.css content-type = %q, want text/css", css.ContentType)
	}

	page, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if strings.Contains(page.Content, "cdn.jsdelivr.net/npm/daisyui") || strings.Contains(page.Content, "@tailwindcss/browser") {
		t.Error("CDN substrate tags should be gone after optimizeCSS")
	}
	if !strings.Contains(page.Content, `href="/app.css"`) {
		t.Error("page should link /app.css after optimizeCSS")
	}
}

// TestOptimizeCSS_RealCompile exercises the full path with the real Tailwind
// CLI + vendored daisyUI: a minimal, self-contained sheet with components,
// the page's arbitrary-value utility purged in, and no CDN references.
func TestOptimizeCSS_RealCompile(t *testing.T) {
	st := minioStoreForBuild(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run optimizeCSS integration tests")
	}
	_, err := exec.LookPath("tailwindcss")
	if err != nil {
		t.Skip("no tailwindcss on PATH for the real-compile test")
	}
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	page := `<!DOCTYPE html><html data-theme="synthwave"><head>
<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />
<link href="https://cdn.jsdelivr.net/npm/daisyui@5/themes.css" rel="stylesheet" type="text/css" />
<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>
</head><body class="min-h-screen"><div class="card max-w-[65ch] py-24">
<button class="btn btn-primary">Go</button></div></body></html>`
	err = st.Write(ctx, slug, "index.html", page, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}

	// No TailwindCLI override -> resolves the tailwindcss on PATH.
	svc := NewWithConfig(Config{Store: st})
	svc.OptimizeCSS(ctx, slug)

	css, err := st.Read(ctx, slug, "app.css")
	if err != nil || css == nil || css.Content == "" {
		t.Fatalf("real compile produced no app.css (err=%v)", err)
	}
	for _, want := range []string{".btn", "65ch"} {
		if !strings.Contains(css.Content, want) {
			t.Errorf("compiled app.css missing %q (len=%d)", want, len(css.Content))
		}
	}
	if strings.Contains(css.Content, "cdn.jsdelivr.net") {
		t.Error("compiled app.css must not reference the CDN")
	}
}

// TestOptimizeCSS_InjectsLinkForSubstrateLessPage mirrors the MCP lint_site
// path: a page authored without any stylesheet link gets /app.css injected and
// compiled, so the design-substrate lint then passes.
func TestOptimizeCSS_InjectsLinkForSubstrateLessPage(t *testing.T) {
	st := minioStoreForBuild(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run optimizeCSS integration tests")
	}
	_, err := exec.LookPath("tailwindcss")
	if err != nil {
		t.Skip("no tailwindcss on PATH for the real-compile test")
	}
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	// No stylesheet link at all — the shape an MCP author might write.
	page := `<!DOCTYPE html><html data-theme="light"><head><title>x</title></head>
<body class="min-h-screen"><button class="btn btn-primary">Go</button></body></html>`
	err = st.Write(ctx, slug, "index.html", page, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}

	svc := NewWithConfig(Config{Store: st})
	svc.OptimizeCSS(ctx, slug)

	got, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(got.Content, `href="/app.css"`) {
		t.Errorf("OptimizeCSS should inject the /app.css link; got:\n%s", got.Content)
	}
	if css, _ := st.Read(ctx, slug, "app.css"); css == nil || !strings.Contains(css.Content, ".btn") {
		t.Error("app.css should be compiled with the page's daisyUI components")
	}
}

func TestOptimizeCSS_GracefulWhenCompileFails(t *testing.T) {
	st := minioStoreForBuild(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run optimizeCSS integration tests")
	}
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	err := st.Write(ctx, slug, "index.html", pageWithCDNSubstrate, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("seed page: %v", err)
	}

	svc := NewWithConfig(Config{Store: st, TailwindCLI: writeStubCLI(t, true)})
	svc.OptimizeCSS(ctx, slug)

	// No stylesheet should have been published.
	if css, _ := st.Read(ctx, slug, "app.css"); css != nil && css.Content != "" {
		t.Error("app.css must not be written when the compile fails")
	}
	// The page must be untouched — CDN tags still present so it renders.
	page, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if !strings.Contains(page.Content, "@tailwindcss/browser") {
		t.Error("CDN substrate must survive a failed compile (graceful degradation)")
	}
}
