package portable

import (
	"strings"
	"testing"
)

// FuzzCleanArchiveName gates names pulled from an attacker-controlled import
// archive before they're handed to store.Write. Property: a non-empty result
// can't be absolute, can't contain a backslash, and has no ".." path segment —
// so it stays under the destination slug.
func FuzzCleanArchiveName(f *testing.F) {
	for _, s := range []string{
		"index.html", "./about.html", "../escape", "/abs", `a\b`, "a/../b",
		"a..b", "", ".", "a/b/c.html", "页.html", "./../x",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		got := cleanArchiveName(name)
		if got == "" {
			return
		}
		if strings.HasPrefix(got, "/") || strings.Contains(got, `\`) {
			t.Fatalf("cleanArchiveName(%q) = %q leaks an absolute path or backslash", name, got)
		}
		for _, seg := range strings.Split(got, "/") {
			if seg == ".." {
				t.Fatalf("cleanArchiveName(%q) = %q has a .. segment", name, got)
			}
		}
	})
}
