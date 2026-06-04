package build

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jtarchie/topbanana/internal/assets"
)

// cssCompileTimeout caps the Tailwind compile subprocess. The compile itself
// runs in tens of milliseconds; the headroom covers an `npx @tailwindcss/cli`
// cold start (download) on a developer machine.
const cssCompileTimeout = 90 * time.Second

// localStylesheetTag is the self-hosted substitute for the three CDN substrate
// tags. Served at /app.css per-site (the runtime compile output) and on the
// platform domain (the embedded admin sheet).
const localStylesheetTag = `<link rel="stylesheet" href="/app.css">`

var (
	// cdnDaisyLinkRE matches both the daisyUI base and themes <link> tags.
	cdnDaisyLinkRE = regexp.MustCompile(`(?i)<link\b[^>]*cdn\.jsdelivr\.net/npm/daisyui[^>]*>`)
	// cdnTailwindScriptRE matches the in-browser Tailwind JIT compiler tag.
	cdnTailwindScriptRE = regexp.MustCompile(`(?i)<script\b[^>]*@tailwindcss/browser[^>]*>\s*</script>`)
)

// OptimizeCSS compiles a minimal, self-contained stylesheet for the slug's
// pages, writes it to {slug}/app.css, and rewrites each page to link it
// (stripping any legacy CDN substrate tags). Called both by the post-build
// step (Service.Start) and by the MCP lint_site tool so directly-authored
// sites get the same self-hosted sheet.
//
// It is best-effort: any failure (no Tailwind CLI available, compile error,
// store error) is logged and returns without mutating the site. The store
// write of app.css happens before any page rewrite, so a page never points at
// a stylesheet that isn't there.
func (svc *Service) OptimizeCSS(ctx context.Context, slug string) {
	cli, args, ok := svc.resolveTailwindCLI()
	if !ok {
		slog.Warn("css.optimize.skipped", "slug", slug, "reason", "no tailwind cli on PATH; set --tailwind-cli")
		return
	}

	daisyDir, err := svc.ensureDaisyUI()
	if err != nil {
		slog.Warn("css.optimize.daisyui_failed", "slug", slug, "err", err)
		return
	}

	pages, contents, err := svc.readPages(ctx, slug)
	if err != nil {
		slog.Warn("css.optimize.read_failed", "slug", slug, "err", err)
		return
	}
	if len(pages) == 0 {
		return
	}

	css, err := compileSiteCSS(ctx, cli, args, daisyDir, contents)
	if err != nil {
		slog.Warn("css.optimize.compile_failed", "slug", slug, "err", err)
		return
	}

	// Publish the stylesheet before any page references it.
	err = svc.store.Write(ctx, slug, "app.css", string(css), "text/css; charset=utf-8", nil)
	if err != nil {
		slog.Warn("css.optimize.write_css_failed", "slug", slug, "err", err)
		return
	}

	rewritten := svc.rewritePages(ctx, slug, pages, contents)
	slog.Info("css.optimize.done", "slug", slug, "bytes", len(css), "pages", len(pages), "rewritten", rewritten)
}

// readPages lists the slug's HTML pages and reads their content into a map
// keyed by path.
func (svc *Service) readPages(ctx context.Context, slug string) ([]string, map[string]string, error) {
	files, err := svc.store.List(ctx, slug)
	if err != nil {
		return nil, nil, fmt.Errorf("list files: %w", err)
	}
	pages, _ := SplitFilesByKind(files)
	contents := make(map[string]string, len(pages))
	for _, page := range pages {
		obj, err := svc.store.Read(ctx, slug, page)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", page, err)
		}
		if obj == nil {
			return nil, nil, fmt.Errorf("read %s: not found", page)
		}
		contents[page] = obj.Content
	}
	return pages, contents, nil
}

// compileSiteCSS stages the pages + a Tailwind input in an isolated temp dir
// (so content auto-detection scans exactly this site), runs the CLI, and
// returns the minified stylesheet bytes.
func compileSiteCSS(ctx context.Context, cli string, args []string, daisyDir string, contents map[string]string) ([]byte, error) {
	work, err := os.MkdirTemp("", "tb-css-")
	if err != nil {
		return nil, fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	for page, content := range contents {
		dst := filepath.Join(work, filepath.FromSlash(page))
		err = os.MkdirAll(filepath.Dir(dst), 0o755)
		if err != nil {
			return nil, fmt.Errorf("stage dir %s: %w", page, err)
		}
		err = os.WriteFile(dst, []byte(content), 0o644)
		if err != nil {
			return nil, fmt.Errorf("stage %s: %w", page, err)
		}
	}

	err = os.WriteFile(filepath.Join(work, "input.css"), []byte(assets.SiteInputCSS(daisyDir)), 0o644)
	if err != nil {
		return nil, fmt.Errorf("write input.css: %w", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, cssCompileTimeout)
	defer cancel()
	cmdArgs := append(append([]string{}, args...), "-i", "input.css", "-o", "app.css", "--minify")
	cmd := exec.CommandContext(runCtx, cli, cmdArgs...)
	cmd.Dir = work
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("compile: %w (%s)", err, strings.TrimSpace(string(out)))
	}

	css, err := os.ReadFile(filepath.Join(work, "app.css"))
	if err != nil {
		return nil, fmt.Errorf("read output: %w", err)
	}
	if len(css) == 0 {
		return nil, errors.New("compile produced empty output")
	}
	return css, nil
}

// rewritePages swaps the CDN substrate for the local /app.css link on every
// page and writes the changed ones back. Returns how many pages changed.
func (svc *Service) rewritePages(ctx context.Context, slug string, pages []string, contents map[string]string) int {
	rewritten := 0
	for _, page := range pages {
		swapped, changed := swapSubstrateForLocalCSS(contents[page])
		if !changed {
			continue
		}
		err := svc.store.Write(ctx, slug, page, swapped, "text/html; charset=utf-8", nil)
		if err != nil {
			slog.Warn("css.optimize.rewrite_failed", "slug", slug, "page", page, "err", err)
			continue
		}
		rewritten++
	}
	return rewritten
}

// resolveTailwindCLI locates the Tailwind compiler. Order: the operator
// override, then a `tailwindcss` binary on PATH (the standalone build we COPY
// into the image), then `npx @tailwindcss/cli` as a developer fallback. The
// returned args are a prefix to prepend before the -i/-o flags.
func (svc *Service) resolveTailwindCLI() (name string, args []string, ok bool) {
	if svc.tailwindCLI != "" {
		return svc.tailwindCLI, nil, true
	}
	p, err := exec.LookPath("tailwindcss")
	if err == nil {
		return p, nil, true
	}
	p, err = exec.LookPath("npx")
	if err == nil {
		return p, []string{"@tailwindcss/cli"}, true
	}
	return "", nil, false
}

// ensureDaisyUI extracts the vendored daisyUI package to a stable per-version
// temp directory once per process and returns its path. The runtime per-site
// input.css references it as a plugin via an absolute path.
func (svc *Service) ensureDaisyUI() (string, error) {
	svc.daisyOnce.Do(func() {
		parent := filepath.Join(os.TempDir(), "topbanana-daisyui-"+assets.DaisyUIVersion)
		svc.daisyDir, svc.daisyErr = assets.ExtractDaisyUI(parent)
	})
	return svc.daisyDir, svc.daisyErr
}

// swapSubstrateForLocalCSS strips the three CDN substrate tags and injects the
// self-hosted /app.css link before </head>. Idempotent: a page that already
// links /app.css and carries no CDN tags is returned unchanged. Returns the
// new content and whether anything changed.
func swapSubstrateForLocalCSS(content string) (string, bool) {
	out := cdnDaisyLinkRE.ReplaceAllString(content, "")
	out = cdnTailwindScriptRE.ReplaceAllString(out, "")

	hasLocal := strings.Contains(out, `href="/app.css"`)
	if !hasLocal {
		idx := strings.Index(strings.ToLower(out), "</head>")
		if idx != -1 {
			out = out[:idx] + localStylesheetTag + "\n" + out[idx:]
		}
	}
	if out == content {
		return content, false
	}
	return out, true
}
