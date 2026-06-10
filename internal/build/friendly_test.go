package build_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/lint"
	"github.com/jtarchie/topbanana/internal/storetest"
)

const genericHeadline = "Something went wrong while building your site."

func TestHumanizeFailure_KnownSignatures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		raw          string
		wantHeadline string
	}{
		{
			name:         "timeout",
			raw:          "build timed out after 15m0s",
			wantHeadline: "This is taking longer than expected.",
		},
		{
			name:         "missing stylesheet",
			raw:          `lint errors after 3 retries: index.html: missing stylesheet — every page must include ` + "`<link rel=\"stylesheet\" href=\"/app.css\">`",
			wantHeadline: "A page was missing its styling.",
		},
		{
			name:         "missing viewport",
			raw:          "lint errors after 3 retries: about.html: missing responsive viewport — every page must include a meta viewport",
			wantHeadline: "A page wasn't set up to look right on phones.",
		},
		{
			name:         "suspicious attr closing quote",
			raw:          `lint errors after 3 retries: index.html: <meta> attribute "content" has a value containing an embedded <link> tag — the value is missing a closing quote and is swallowing the following element.`,
			wantHeadline: "A page had a formatting glitch we couldn't fix automatically.",
		},
		{
			name:         "broken link",
			raw:          `lint errors after 3 retries: index.html: broken link "missing.html" (resolved to "missing.html")`,
			wantHeadline: "A link pointed to a page that doesn't exist yet.",
		},
		{
			name:         "missing index",
			raw:          "lint errors after 3 retries: index.html: site is missing a non-empty index.html (every site needs an entry point)",
			wantHeadline: "Your site didn't end up with a home page.",
		},
		{
			name:         "template invariant",
			raw:          `lint errors after 3 retries: index.html: required by "contact-form" template but missing or empty`,
			wantHeadline: "A required part of the chosen template was missing.",
		},
		{
			name:         "generic lint exhaustion",
			raw:          `lint errors after 3 retries: index.html: must contain "<form>" (template "x")`,
			wantHeadline: "We built your site, but a few things didn't pass our checks.",
		},
		{
			name:         "wrapped agent error falls back",
			raw:          "scripted failure",
			wantHeadline: genericHeadline,
		},
		{
			name:         "retry wrapper falls back",
			raw:          "retry: agent run aborted",
			wantHeadline: genericHeadline,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			headline, hint, detail := build.HumanizeFailure(tc.raw)
			if headline != tc.wantHeadline {
				t.Errorf("headline = %q, want %q", headline, tc.wantHeadline)
			}
			if hint == "" {
				t.Error("hint is empty; every failure should suggest a next step")
			}
			if detail != tc.raw {
				t.Errorf("detail = %q, want raw %q", detail, tc.raw)
			}
		})
	}
}

// TestHumanizeFailure_RealLintOutput guards the soft coupling between this
// package's substring rules and the literal lint messages in internal/lint:
// it feeds genuine lint.App output through HumanizeFailure and asserts each
// known cause maps to a specific (non-generic) headline. If a lint message is
// reworded without updating friendlyRules, this fails loudly instead of
// silently degrading the user-facing copy to the generic bucket.
func TestHumanizeFailure_RealLintOutput(t *testing.T) {
	t.Parallel()

	const validHead = `<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>t</title>
<link rel="stylesheet" href="/app.css">`

	cases := []struct {
		name         string
		files        map[string]string
		wantHeadline string
	}{
		{
			name: "missing substrate",
			files: map[string]string{
				"index.html": `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><h1>hi</h1></body></html>`,
			},
			wantHeadline: "A page was missing its styling.",
		},
		{
			name: "broken link",
			files: map[string]string{
				"index.html": `<!DOCTYPE html><html lang="en"><head>` + validHead + `</head><body><a href="missing.html">x</a></body></html>`,
			},
			wantHeadline: "A link pointed to a page that doesn't exist yet.",
		},
		{
			name: "missing home page",
			files: map[string]string{
				"about.html": `<!DOCTYPE html><html lang="en"><head>` + validHead + `</head><body><h1>about</h1></body></html>`,
			},
			wantHeadline: "Your site didn't end up with a home page.",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			st := storetest.New(t, 0)
			slug := "friendly-" + strings.ReplaceAll(tc.name, " ", "-")
			for path, body := range tc.files {
				err := st.Write(ctx, slug, path, body, "text/html; charset=utf-8", nil)
				if err != nil {
					t.Fatalf("seed %s: %v", path, err)
				}
			}

			errs := lint.App(ctx, st, slug, nil)
			if len(errs) == 0 {
				t.Fatal("expected lint errors, got none")
			}
			msgs := make([]string, 0, len(errs))
			for _, e := range errs {
				msgs = append(msgs, e.Error())
			}
			raw := "lint errors after 3 retries: " + strings.Join(msgs, "; ")

			headline, _, _ := build.HumanizeFailure(raw)
			if headline == genericHeadline {
				t.Errorf("real lint output fell through to the generic fallback: %q", raw)
			}
			if headline != tc.wantHeadline {
				t.Errorf("headline = %q, want %q (raw: %q)", headline, tc.wantHeadline, raw)
			}
		})
	}
}
