package textedit

import (
	"path"
	"strings"
	"testing"
)

// FuzzValidateHTMLPath asserts the HTML-path gate never accepts a path that
// could escape the slug prefix. Both the build agent and the MCP edit tools
// write to S3 keys derived from these paths, so an accepted traversal would be
// a cross-tenant write. Property: a nil error implies the documented
// invariants (relative, forward-slash, canonical, .html, no .. segment).
func FuzzValidateHTMLPath(f *testing.F) {
	for _, s := range []string{
		"index.html", "about/index.html", "", "..", "../x.html", "/x.html",
		`a\b.html`, "a.txt", "a/./b.html", "a/../b.html", "UPPER.html", "页.html",
		".topbanana.json", "assets/x.html", strings.Repeat("a", 300) + ".html",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if ValidateHTMLPath(p) != nil {
			return
		}
		// Accepted paths must be relative, canonical, .html, and free of any
		// "."/".."/"" segment — that combination is what keeps `{slug}/{p}` from
		// escaping the slug. (Mid-segment dots like "a..b.html" are fine and
		// allowed, so check segments, not a naive Contains("..").)
		if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) ||
			!strings.HasSuffix(p, ".html") || path.Clean(p) != p {
			t.Fatalf("ValidateHTMLPath accepted an unsafe path %q", p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("ValidateHTMLPath accepted a path with a relative segment %q", p)
			}
		}
	})
}

// FuzzValidateTextPath asserts the wider write/edit gate keeps the same
// containment guarantees as the HTML one, plus the two exclusions that make it
// safe to widen: an accepted path can never reach a platform-managed area
// (`functions/` executables, `_`-prefixed reserved directories, the dot-file
// sidecars) and can never carry an extension outside the text-asset set.
func FuzzValidateTextPath(f *testing.F) {
	for _, s := range []string{
		"index.html", "fonts.css", "assets/logo.svg", "app.js", "data/x.json",
		"", "..", "../x.css", "/x.css", `a\b.css`, "a/./b.css", "a/../b.css",
		"functions/x.js", "functions/../x.js", "_state/data.json", "a/_state/x.json",
		".topbanana.json", ".env", "UPPER.CSS", "页.css", "x.php", "noext",
		strings.Repeat("a", 300) + ".css",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if ValidateTextPath(p) != nil {
			return
		}
		if strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || path.Clean(p) != p {
			t.Fatalf("ValidateTextPath accepted an unsafe path %q", p)
		}
		if !textAssetExts[strings.ToLower(path.Ext(p))] {
			t.Fatalf("ValidateTextPath accepted an unsupported extension %q", p)
		}
		if strings.HasPrefix(p, "functions/") || reservedWritePaths[p] {
			t.Fatalf("ValidateTextPath accepted a platform-managed path %q", p)
		}
		for _, seg := range strings.Split(p, "/") {
			if seg == "" || seg == "." || seg == ".." {
				t.Fatalf("ValidateTextPath accepted a path with a relative segment %q", p)
			}
			if strings.HasPrefix(seg, "_") || strings.HasPrefix(seg, ".") {
				t.Fatalf("ValidateTextPath accepted a reserved segment in %q", p)
			}
		}
	})
}

// FuzzValidateFunctionName asserts accepted handler names can't escape the
// functions/ prefix. Property: a nil error implies [a-z0-9-_]{1,40}, so the
// name can never contain a slash, dot, or backslash.
func FuzzValidateFunctionName(f *testing.F) {
	for _, s := range []string{
		"submit", "a-b_c", "", "../x", "a/b", "a.js", "a..b", "PascalCase", "页",
		strings.Repeat("a", 41),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, n string) {
		if ValidateFunctionName(n) != nil {
			return
		}
		if len(n) == 0 || len(n) > 40 || strings.ContainsAny(n, `/.\`) {
			t.Fatalf("ValidateFunctionName accepted an unsafe name %q", n)
		}
	})
}
