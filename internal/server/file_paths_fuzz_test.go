package server

import "testing"

// FuzzClassifyUserPath exercises the path-classification chain used by the
// delete/rename UI. Property: the function must never panic on arbitrary
// byte sequences, and when it returns a nil error the result kind must be a
// known value. The classifier is the only gate between user input and the
// S3 key construction in store.Store, so a crash here would translate
// directly into a cross-tenant 5xx.
func FuzzClassifyUserPath(f *testing.F) {
	seeds := []string{
		"index.html",
		"assets/logo.png",
		"functions/submit.js",
		"",
		"..",
		"../etc/passwd",
		"./index.html",
		"/index.html",
		"assets/../index.html",
		"functions/sub/dir.js",
		"functions/.js",
		"assets/",
		"index.html\x00",
		"index.html\n",
		"INDEX.HTML",
		"页面.html",
		"a/b/c/d/e/f/g/h.html",
		`assets\logo.png`,
		"_hidden.html",
		".env",
		"functions/" + string(make([]byte, 300)) + ".js",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, p string) {
		kind, err := classifyUserPath(p)
		if err != nil {
			return
		}
		switch kind {
		case kindHTML, kindAsset, kindFunction:
			// ok
		default:
			t.Fatalf("classifyUserPath(%q) returned nil err with unknown kind %d", p, kind)
		}
	})
}
