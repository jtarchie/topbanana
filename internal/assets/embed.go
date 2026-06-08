// Package assets embeds the self-contained CSS substrate that replaces the
// former CDN-loaded Tailwind + daisyUI design system.
//
// Two consumers:
//
//   - The admin UI links a single precompiled sheet, AppCSS, served at
//     /app.css. It is built from app.input.css by `task css` (and the Docker
//     builder stage) and committed so plain `go run` works without a CLI.
//
//   - Per-site builds compile their own minimal sheet at runtime (see
//     internal/build/css_compile.go). They need the daisyUI plugin on disk:
//     ExtractDaisyUI writes the vendored package out once, and SiteInputCSS
//     produces the Tailwind input that references it.
package assets

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// DaisyUIVersion is the vendored daisyUI release under ./daisyui. Bump it with
// `task vendor:daisyui`; assets_test.go asserts it matches the package.json.
const DaisyUIVersion = "5.5.20"

// AppCSS is the compiled admin-UI stylesheet. See app.input.css for the source
// and the package doc for how it is regenerated.
//
//go:embed app.css
var AppCSS []byte

// ImageDrawerJS is the shared client module that wires up the Images side-
// drawer on the workspace, visual-editor, and manage pages. Served at
// /image_drawer.js — each host page renders the image_drawer.html partial and
// then calls TBImageDrawer.init({slug, mode, onInsert}) once.
//
//go:embed image_drawer.js
var ImageDrawerJS []byte

// daisyUIFS is the vendored daisyUI npm package, embedded so the runtime
// per-site Tailwind compile can load it as a plugin without npm/network.
//
//go:embed daisyui
var daisyUIFS embed.FS

// ExtractDaisyUI writes the vendored daisyUI package into parent/daisyui and
// returns that directory. It is idempotent enough for startup use — files are
// overwritten, so a half-written extraction self-heals on the next call. The
// returned path is what SiteInputCSS expects as daisyUIDir.
func ExtractDaisyUI(parent string) (string, error) {
	err := fs.WalkDir(daisyUIFS, "daisyui", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		target := filepath.Join(parent, p)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := daisyUIFS.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return "", fmt.Errorf("extract daisyui: %w", err)
	}
	return filepath.Join(parent, "daisyui"), nil
}

// SiteInputCSS returns the Tailwind v4 input used to compile a single site's
// stylesheet. daisyUIDir is an absolute path to an extracted daisyUI package
// (from ExtractDaisyUI). It excludes that directory from content scanning —
// otherwise daisyUI's own class-like source tokens inflate the utility layer —
// and loads daisyUI as a plugin with every theme available (sites set their
// palette via <html data-theme> and the owner can switch it in the studio).
func SiteInputCSS(daisyUIDir string) string {
	return fmt.Sprintf(`@import "tailwindcss";
@source not %q;
@plugin %q {
  themes: all;
}
`, daisyUIDir, filepath.Join(daisyUIDir, "index.js"))
}
