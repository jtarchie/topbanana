package lint

import (
	"strings"
	"testing"
)

func externalErrs(t *testing.T, body string) []Error {
	t.Helper()
	return checkExternalResources(pageOf(t, "index.html", `<!DOCTYPE html><html><body>`+body+`</body></html>`))
}

func TestCheckExternalResources_Scripts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     string
		wantKind Kind
	}{
		// The tiny-shop skeleton ships this exact tag — it must stay clean.
		{"stripe buy button allowed (tiny-shop verbatim)", `<script async src="https://js.stripe.com/v3/buy-button.js"></script>`, ""},
		{"stripe pricing table allowed", `<script async src="https://js.stripe.com/v3/pricing-table.js"></script>`, ""},
		{"inline script allowed", `<script>console.log(1)</script>`, ""},
		{"other external script flags", `<script src="https://cdn.example.com/lib.js"></script>`, KindExternalScript},
		{"protocol-relative script flags", `<script src="//cdn.example.com/lib.js"></script>`, KindExternalScript},
		{"http stripe flags as insecure", `<script src="http://js.stripe.com/v3/buy-button.js"></script>`, KindMixedContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := externalErrs(t, tc.body)
			if tc.wantKind == "" {
				if len(errs) != 0 {
					t.Fatalf("expected clean, got %+v", errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Kind != tc.wantKind {
				t.Fatalf("expected one %q error, got %+v", tc.wantKind, errs)
			}
		})
	}
}

func TestCheckExternalResources_Stylesheets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"self-hosted app.css passes", `<link rel="stylesheet" href="/app.css">`, false},
		{"external stylesheet flags", `<link rel="stylesheet" href="https://fonts.example.com/css">`, true},
		{"rel list containing stylesheet flags", `<link rel="preload stylesheet" href="https://x.example.com/a.css">`, true},
		{"external icon link passes", `<link rel="icon" href="https://example.com/favicon.ico">`, false},
		{"external preconnect passes", `<link rel="preconnect" href="https://example.com">`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := externalErrs(t, tc.body)
			var got int
			for _, e := range errs {
				if e.Kind == KindExternalStylesheet {
					got++
				}
			}
			if tc.wantErr != (got == 1) {
				t.Fatalf("wantErr=%v, got %+v", tc.wantErr, errs)
			}
		})
	}
}

func TestCheckExternalResources_MixedContent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"http image flags", `<img src="http://example.com/x.png">`, true},
		{"http anchor flags", `<a href="http://example.com/page">x</a>`, true},
		{"http form action flags", `<form action="http://example.com/post"></form>`, true},
		{"https image passes", `<img src="https://example.com/x.png">`, false},
		{"https iframe passes (maps embeds are legitimate)", `<iframe src="https://maps.google.com/maps?q=x"></iframe>`, false},
		{"relative src passes", `<img src="logo.png">`, false},
		{"svg xmlns is not a resource URL", `<svg xmlns="http://www.w3.org/2000/svg"><circle r="1"/></svg>`, false},
		{"svg xlink:href is namespaced and skipped", `<svg xmlns:xlink="http://www.w3.org/1999/xlink"><use xlink:href="#icon"/></svg>`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := externalErrs(t, tc.body)
			var got int
			for _, e := range errs {
				if e.Kind == KindMixedContent {
					got++
				}
			}
			if tc.wantErr != (got == 1) {
				t.Fatalf("wantErr=%v, got %+v", tc.wantErr, errs)
			}
		})
	}
}

func TestCheckExternalResources_DedupesRepeatedURL(t *testing.T) {
	t.Parallel()

	errs := externalErrs(t, `<img src="http://example.com/x.png"><img src="http://example.com/x.png">`)
	if len(errs) != 1 {
		t.Fatalf("expected the repeated URL reported once, got %+v", errs)
	}
	if !strings.Contains(errs[0].Message, "http://example.com/x.png") {
		t.Errorf("message must name the URL: %s", errs[0].Message)
	}
}
