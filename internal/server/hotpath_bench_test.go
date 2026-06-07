package server

import "testing"

// These benchmarks cover the pure-function validators that run on every
// request (subdomain dispatch, path classification, traversal checks).
// They exist so `task bench:save` can snapshot a baseline and
// `task bench:diff` flags regressions before they reach production.

func BenchmarkStripPort(b *testing.B) {
	hosts := []string{
		"example.com",
		"example.com:8080",
		"[::1]:443",
		"sub.example.com:1234",
	}
	for i := 0; b.Loop(); i++ {
		_ = stripPort(hosts[i%len(hosts)])
	}
}

func BenchmarkValidateSlug(b *testing.B) {
	slugs := []string{"abc", "my-app", "blue-fox-42", "very-long-slug-name-30"}
	for i := 0; b.Loop(); i++ {
		_ = validateSlug(slugs[i%len(slugs)])
	}
}

func BenchmarkIsTraversal(b *testing.B) {
	paths := []string{
		"index.html",
		"assets/logo.png",
		"../escape",
		"a/b/c/d.html",
	}
	for i := 0; b.Loop(); i++ {
		_ = isTraversal(paths[i%len(paths)])
	}
}

func BenchmarkClassifyUserPath(b *testing.B) {
	paths := []string{
		"index.html",
		"assets/logo.png",
		"functions/submit.js",
		"deeply/nested/page.html",
	}
	for i := 0; b.Loop(); i++ {
		_, _ = classifyUserPath(paths[i%len(paths)])
	}
}
