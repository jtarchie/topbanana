package server

import (
	"strings"
	"testing"
)

// FuzzStripPort fuzzes the Host-header parser that every host-based check
// funnels through. It must never panic and must never return a string longer
// than its input (a longer return would imply we manufactured bytes from
// thin air, which would break downstream slug suffix-stripping).
func FuzzStripPort(f *testing.F) {
	seeds := []string{
		"example.com",
		"example.com:8080",
		"[::1]:8080",
		"[::1]",
		"localhost",
		"localhost:80",
		":8080",
		"",
		":",
		"a:b:c:d",
		"foo.bar.baz.example.com:1234",
		"日本.example.com",
		"\x00:80",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, host string) {
		got := stripPort(host)
		if len(got) > len(host) {
			t.Fatalf("stripPort(%q) returned longer string %q", host, got)
		}
		if !strings.HasPrefix(host, got) {
			t.Fatalf("stripPort(%q) = %q is not a prefix of input", host, got)
		}
	})
}

// FuzzValidateSlug pushes random strings through the slug shape checker.
// validateSlug feeds into both subdomain dispatch and the path route, so an
// off-by-one or panic here changes which Host headers reach S3. Property:
// no panic; when err == nil the slug must be all-lowercase alnum-and-hyphen
// in the documented length window with no edge hyphens.
func FuzzValidateSlug(f *testing.F) {
	seeds := []string{
		"abc",
		"my-site",
		"a", "ab",
		"-foo", "foo-",
		"Foo",
		"site_one",
		"site.one",
		"www", "api", "admin",
		strings.Repeat("a", 31),
		strings.Repeat("a", 30),
		"123",
		"a--b",
		"日本",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, slug string) {
		err := validateSlug(slug)
		if err != nil {
			return
		}
		if n := len(slug); n < 3 || n > 30 {
			t.Fatalf("validateSlug(%q) accepted length %d", slug, n)
		}
		if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") {
			t.Fatalf("validateSlug(%q) accepted edge hyphen", slug)
		}
		for _, r := range slug {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= '0' && r <= '9':
			case r == '-':
			default:
				t.Fatalf("validateSlug(%q) accepted rune %q", slug, r)
			}
		}
	})
}

// FuzzIsTraversal asserts the traversal detector never panics and that any
// path containing "..", a leading "/", or a backslash is flagged. This
// matches the documented invariant in server.go:1696-1702.
func FuzzIsTraversal(f *testing.F) {
	seeds := []string{
		"index.html",
		"../etc/passwd",
		"/abs/path",
		`win\style`,
		"a/../b",
		"",
		"....//",
		"page..html",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		got := isTraversal(p)
		want := strings.Contains(p, "..") || strings.HasPrefix(p, "/") || strings.Contains(p, `\`)
		if got != want {
			t.Fatalf("isTraversal(%q) = %v, want %v", p, got, want)
		}
	})
}
