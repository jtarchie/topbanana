package guide

import (
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/templates"
)

func mustParse(t *testing.T, h string) parsedPage {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(h))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return parsedPage{Path: "index.html", Doc: doc}
}

func TestDetectors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		det    detector
		params templates.GuideParams
		html   string
		want   bool
	}{
		// tel_link
		{"tel present", detectTelLink, templates.GuideParams{}, `<a href="tel:+15551234567">Call</a>`, true},
		{"tel uppercase scheme", detectTelLink, templates.GuideParams{}, `<a href="TEL:5551234">Call</a>`, true},
		{"tel missing", detectTelLink, templates.GuideParams{}, `<a href="/about.html">About</a>`, false},
		{"tel plain text not a link", detectTelLink, templates.GuideParams{}, `<p>(555) 123-4567</p>`, false},

		// email_link
		{"email present", detectEmailLink, templates.GuideParams{}, `<a href="mailto:hi@x.com">Email</a>`, true},
		{"email missing", detectEmailLink, templates.GuideParams{}, `<a href="https://x.com">Site</a>`, false},

		// form
		{"form present", detectForm, templates.GuideParams{}, `<form action="/api/submit"><input name="email"></form>`, true},
		{"form missing", detectForm, templates.GuideParams{}, `<div>no form here</div>`, false},

		// heading_matches
		{"heading match", detectHeadingMatches, templates.GuideParams{Keywords: []string{"hours"}}, `<h2>Our Hours</h2>`, true},
		{"heading no match", detectHeadingMatches, templates.GuideParams{Keywords: []string{"hours"}}, `<h2>Welcome</h2>`, false},
		{"heading match ignores body text", detectHeadingMatches, templates.GuideParams{Keywords: []string{"hours"}}, `<h2>Welcome</h2><p>our hours are long</p>`, false},

		// section_present
		{"section with real body", detectSectionPresent, templates.GuideParams{Keywords: []string{"menu"}},
			`<h2>Menu</h2><ul><li>Classic Burger — beef, lettuce, tomato, $9</li><li>Hand-cut Fries, $4</li></ul>`, true},
		{"section empty placeholder", detectSectionPresent, templates.GuideParams{Keywords: []string{"menu"}},
			`<h2>Menu</h2><p>Soon</p>`, false},
		{"section heading absent", detectSectionPresent, templates.GuideParams{Keywords: []string{"menu"}},
			`<h2>Hours</h2><p>Open Monday to Friday from nine until five.</p>`, false},
		{"section counts deeper subheadings", detectSectionPresent, templates.GuideParams{Keywords: []string{"menu"}},
			`<h2>Menu</h2><h3>Starters</h3><p>Soup of the day with fresh bread.</p>`, true},
		{"section stops at sibling heading", detectSectionPresent, templates.GuideParams{Keywords: []string{"menu"}},
			`<h2>Menu</h2><h2>Hours</h2><p>Open late every single day of the week here.</p>`, false},

		// address
		{"address element", detectAddress, templates.GuideParams{}, `<address>123 Main St, Springfield</address>`, true},
		{"address empty element falls back to heading", detectAddress, templates.GuideParams{}, `<address>   </address>`, false},
		{"address via location heading", detectAddress, templates.GuideParams{}, `<h2>Find Us</h2>`, true},
		{"address missing", detectAddress, templates.GuideParams{}, `<h2>Menu</h2>`, false},

		// map_link
		{"map google maps path", detectMapLink, templates.GuideParams{}, `<a href="https://www.google.com/maps/place/Joes">Map</a>`, true},
		{"map maps subdomain", detectMapLink, templates.GuideParams{}, `<iframe src="https://maps.google.com/?q=joes"></iframe>`, true},
		{"map apple", detectMapLink, templates.GuideParams{}, `<a href="https://maps.apple.com/?q=joes">Map</a>`, true},
		{"map non-maps google not counted", detectMapLink, templates.GuideParams{}, `<a href="https://www.google.com/search?q=joes">Search</a>`, false},
		{"map missing", detectMapLink, templates.GuideParams{}, `<a href="/contact.html">Contact</a>`, false},

		// min_images
		{"min_images enough real images", detectMinImages, templates.GuideParams{Min: 2},
			`<img src="a.jpg" width="600"><img src="b.jpg" width="600">`, true},
		{"min_images excludes icons", detectMinImages, templates.GuideParams{Min: 2},
			`<img src="hero.jpg" width="600"><img src="logo.png" width="32">`, false},
		{"min_images alt logo excluded", detectMinImages, templates.GuideParams{Min: 1},
			`<img src="x.png" alt="Company logo">`, false},

		// min_links
		{"min_links enough", detectMinLinks, templates.GuideParams{Min: 3},
			`<a href="https://a.com">a</a><a href="https://b.com">b</a><a href="https://c.com">c</a>`, true},
		{"min_links too few", detectMinLinks, templates.GuideParams{Min: 3},
			`<a href="https://a.com">a</a><a href="/about.html">about</a>`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.det(tc.params, []parsedPage{mustParse(t, tc.html)})
			if got != tc.want {
				t.Errorf("detector = %v, want %v", got, tc.want)
			}
		})
	}
}
