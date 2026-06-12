package lint

import (
	"strings"
	"testing"
)

// TestResolveLinkTarget pins proxy parity: every case mirrors what
// internal/server/proxy.go would serve for the path a browser produces from
// the link, so lint can neither pass a link that 404s nor flag one that works.
func TestResolveLinkTarget(t *testing.T) {
	t.Parallel()

	fileSet := map[string]bool{
		"index.html":      true,
		"about.html":      true,
		"sub/page.html":   true,
		"sub/foo.html":    true,
		"docs/index.html": true,
		"logo.png":        true,
		"weird.html.html": true,
	}

	cases := []struct {
		name         string
		dir, link    string
		wantResolved string
		wantOK       bool
	}{
		{"relative sibling", ".", "about.html", "about.html", true},
		{"relative within subdir", "sub", "foo.html", "sub/foo.html", true},
		{"missing file", ".", "missing.html", "missing.html", false},
		// Absolute links are root-relative in the browser — they must NOT be
		// joined onto the page's directory. The old path.Join(dir, link)
		// behavior produced both a false positive and a false negative here.
		{"absolute from subdir hits root file", "sub", "/about.html", "about.html", true},
		{"absolute from subdir does not see subdir file", "sub", "/foo.html", "foo.html", false},
		{"absolute root", ".", "/", "index.html", true},
		// Extensionless fallbacks, same as the proxy's candidate list.
		{"extensionless falls back to .html", ".", "about", "about.html", true},
		{"directory falls back to index.html", ".", "docs", "docs/index.html", true},
		{"directory with trailing slash", ".", "docs/", "docs/index.html", true},
		// The proxy only tries fallbacks when the path does NOT end in .html:
		// a link to weird.html must not pass just because weird.html.html exists.
		{"no fallback when path already ends in .html", ".", "weird.html", "weird.html", false},
		// Browsers drop ".." segments that climb above the root before
		// sending the request, so such links work when the root file exists.
		{"dotdot above root clamps to root", ".", "../about.html", "about.html", true},
		{"dotdot from subdir", "sub", "../about.html", "about.html", true},
		{"non-html asset", ".", "logo.png", "logo.png", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resolved, ok := resolveLinkTarget(tc.dir, tc.link, fileSet)
			if resolved != tc.wantResolved || ok != tc.wantOK {
				t.Fatalf("resolveLinkTarget(%q, %q) = (%q, %v), want (%q, %v)",
					tc.dir, tc.link, resolved, ok, tc.wantResolved, tc.wantOK)
			}
		})
	}
}

// TestCheckLink_AbsoluteFromSubdir is the end-to-end regression for the
// resolution bug: checkLink on a subdirectory page with root-absolute links.
func TestCheckLink_AbsoluteFromSubdir(t *testing.T) {
	t.Parallel()

	fileSet := map[string]bool{
		"index.html":     true,
		"about.html":     true,
		"sub/page.html":  true,
		"sub/local.html": true,
	}
	lc := linkCheckContext{fileSet: fileSet}

	if errs := checkLink("sub/page.html", "sub", "/about.html", lc); len(errs) != 0 {
		t.Errorf("root-absolute link to an existing root file must pass from a subdir page: %+v", errs)
	}
	if errs := checkLink("sub/page.html", "sub", "/local.html", lc); len(errs) == 0 {
		t.Error("root-absolute /local.html must fail — only sub/local.html exists, the proxy would 404")
	}
}

func TestClosestMatch(t *testing.T) {
	t.Parallel()

	candidates := []string{"about.html", "contact.html", "index.html"}

	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"transposed letters", "abuot.html", "about.html"},
		{"case mismatch is distance zero", "About.html", "about.html"},
		{"dropped character", "contct.html", "contact.html"},
		{"nothing plausible", "zzzzzzzz.png", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := closestMatch(tc.target, candidates); got != tc.want {
				t.Fatalf("closestMatch(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

func TestBrokenLinkMessage(t *testing.T) {
	t.Parallel()

	fileSet := map[string]bool{
		"index.html":          true,
		"about.html":          true,
		"functions/submit.js": true,
		".topbanana.json":     true,
		"_state/data.json":    true,
	}

	msg := brokenLinkMessage("abuot.html", "abuot.html", fileSet)
	for _, want := range []string{`broken link "abuot.html"`, `Did you mean "about.html"?`, "Site files: about.html, index.html."} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	// Unlinkable files must never be suggested: functions are only reachable
	// via /api/, and dot/underscore paths are reserved from the proxy.
	for _, reject := range []string{"functions/", ".topbanana.json", "_state/"} {
		if strings.Contains(msg, reject) {
			t.Errorf("message must not steer the agent toward %q:\n%s", reject, msg)
		}
	}
}

func TestCapList(t *testing.T) {
	t.Parallel()

	items := []string{"a", "b", "c", "d"}
	if got := capList(items, 4); got != "a, b, c, d" {
		t.Errorf("capList under limit = %q", got)
	}
	if got := capList(items, 2); got != "a, b, +2 more" {
		t.Errorf("capList over limit = %q", got)
	}
}

func TestLevenshtein(t *testing.T) {
	t.Parallel()

	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"abc", "", 3},
		{"kitten", "sitting", 3},
		{"abuot", "about", 2},
	}
	for _, tc := range cases {
		if got := levenshtein(tc.a, tc.b); got != tc.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
