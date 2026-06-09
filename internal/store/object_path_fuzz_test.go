package store_test

import (
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/store"
)

// FuzzValidateObjectPath is the last-line cross-tenant escape guard: every
// Read/Write/Copy runs the path through it before joining it as `{slug}/{path}`.
// Property: a nil error implies the key can't escape the slug prefix (no "..",
// no leading "/", no backslash).
func FuzzValidateObjectPath(f *testing.F) {
	for _, s := range []string{
		"index.html", "assets/logo.png", "", "..", "../etc/passwd", "a/../b",
		"/abs", `a\b`, "a..b", "\x00", "页.html", "_snapshots/x", "a/b/c.html",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if store.ValidateObjectPath(p) != nil {
			return
		}
		if strings.Contains(p, "..") || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) {
			t.Fatalf("ValidateObjectPath accepted an unsafe path %q", p)
		}
	})
}
