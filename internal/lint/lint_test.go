package lint

import "testing"

func TestCheckLinkAPIRoutes(t *testing.T) {
	t.Parallel()

	// File set is intentionally empty: /api/* routes don't have backing files,
	// the test only verifies the enablesFns gate.
	fileSet := map[string]bool{
		"index.html": true,
		"about.html": true,
	}

	cases := []struct {
		name       string
		raw        string
		enablesFns bool
		wantErr    bool
	}{
		{"absolute /api/ allowed when functions enabled", "/api/sign", true, false},
		{"absolute /api/ rejected when functions disabled", "/api/sign", false, true},
		{"static link still validated when functions enabled", "missing.html", true, true},
		{"static link to existing file passes either way", "about.html", true, false},
		{"static link to existing file passes either way (off)", "about.html", false, false},
		{"deep /api/ subpath allowed", "/api/cart/add", true, false},
		// Relative "api/foo" is not a dynamic route — it'd resolve to a static
		// file under the page's directory. We do NOT skip those.
		{"relative api/ still validated when functions enabled", "api/sign", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkLink("index.html", ".", tc.raw, fileSet, tc.enablesFns)
			if tc.wantErr && got == nil {
				t.Fatalf("checkLink(%q, enablesFns=%v) = nil, want error", tc.raw, tc.enablesFns)
			}
			if !tc.wantErr && got != nil {
				t.Fatalf("checkLink(%q, enablesFns=%v) = %v, want nil", tc.raw, tc.enablesFns, got)
			}
		})
	}
}
