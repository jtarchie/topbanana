package lint

import (
	"strings"
	"testing"
)

// Seeds the image files the fixtures point at, so a name assertion is not drowned by a broken-link error.
func lintWithImage(t *testing.T, body string) []Error {
	t.Helper()
	files := map[string]string{"index.html": a11yPage(body)}
	for _, img := range []string{"hero.png", "swirl.png", "dot.png", "logo.png"} {
		files[img] = "png-bytes"
	}
	return lintSite(t, files)
}

func TestNames_ImageAlt(t *testing.T) {
	t.Parallel()

	t.Run("missing alt is an error", func(t *testing.T) {
		t.Parallel()
		got := onlyKind(t, lintWithImage(t, `<img src="hero.png">`), KindMissingAlt)
		if got.Severity() != SeverityError {
			t.Errorf("missing alt should block a build, got %v", got.Severity())
		}
		if !strings.Contains(got.Message, "hero.png") {
			t.Errorf("message should name the image:\n%s", got.Message)
		}
	})

	t.Run("empty alt is a valid decorative declaration", func(t *testing.T) {
		t.Parallel()
		if errs := lintWithImage(t, `<img src="swirl.png" alt="">`); len(errs) != 0 {
			t.Errorf(`alt="" should be accepted, got %+v`, errs)
		}
	})

	t.Run("described image is clean", func(t *testing.T) {
		t.Parallel()
		if errs := lintWithImage(t, `<img src="hero.png" alt="The shop front at dusk">`); len(errs) != 0 {
			t.Errorf("expected clean, got %+v", errs)
		}
	})

	t.Run("aria-hidden image is not judged", func(t *testing.T) {
		t.Parallel()
		if errs := lintWithImage(t, `<img src="dot.png" aria-hidden="true">`); len(errs) != 0 {
			t.Errorf("hidden image should be skipped, got %+v", errs)
		}
	})
}

func TestNames_ControlsNeedNames(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		body  string
		want  Kind
		clean bool
		image bool
	}{
		{name: "icon-only button", body: `<button><svg viewBox="0 0 1 1"><path d="M0 0"/></svg></button>`, want: KindMissingControlName},
		{name: "empty link", body: `<a href="index.html"><span class="icon"></span></a>`, want: KindMissingControlName},
		{name: "button named by text", body: `<button>Save</button>`, clean: true},
		{name: "button named by aria-label", body: `<button aria-label="Close the dialog"><span>&times;</span></button>`, clean: true},
		{name: "link named by an image inside", body: `<a href="index.html"><img src="logo.png" alt="Acme home"></a>`, clean: true, image: true},
		{name: "button named by an svg title", body: `<button><svg viewBox="0 0 1 1"><title>Search</title></svg></button>`, clean: true},
		{name: "link named by aria-labelledby", body: `<h2 id="h">Read the guide</h2><a href="index.html" aria-labelledby="h"><span></span></a>`, clean: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := lintBody(t, tc.body)
			if tc.image {
				errs = lintWithImage(t, tc.body)
			}
			if tc.clean {
				if len(errs) != 0 {
					t.Errorf("expected clean, got %+v", errs)
				}
				return
			}
			onlyKind(t, errs, tc.want)
		})
	}
}

func TestNames_FieldsNeedLabels(t *testing.T) {
	t.Parallel()

	t.Run("placeholder is not a label", func(t *testing.T) {
		t.Parallel()
		body := `<form><input type="email" name="email" placeholder="you@example.com"><button>Join</button></form>`
		got := onlyKind(t, lintBody(t, body), KindMissingFieldLabel)
		if !strings.Contains(got.Message, "placeholder is not a label") {
			t.Errorf("message should say why a placeholder does not count:\n%s", got.Message)
		}
	})

	cases := []struct {
		name string
		body string
	}{
		{name: "explicit label", body: `<label for="e">Email</label><input type="email" id="e" name="email">`},
		{name: "wrapping label", body: `<label>Email <input type="email" name="email"></label>`},
		{name: "aria-label", body: `<input type="email" name="email" aria-label="Email address">`},
		{name: "select with a label", body: `<label for="s">Size</label><select id="s" name="size"><option>S</option></select>`},
		{name: "textarea with a label", body: `<label for="m">Message</label><textarea id="m" name="message"></textarea>`},
		{name: "submit needs no label", body: `<form><input type="submit"></form>`},
		{name: "hidden field needs no label", body: `<form><input type="hidden" name="src" value="web"><button>Go</button></form>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if errs := lintBody(t, tc.body); len(errs) != 0 {
				t.Errorf("expected clean, got %+v", errs)
			}
		})
	}
}

// A script-built page may fill an empty control at runtime, so the name check stands down exactly as the id-based checks do.
func TestNames_DynamicPageStandsDown(t *testing.T) {
	t.Parallel()

	body := `<div id="cart"></div><button id="add"></button>` +
		`<script>var t=document.getElementById('cart');` +
		`var b=document.createElement('button');b.textContent='Add';t.appendChild(b);` +
		`document.getElementById('add').textContent='Add to cart';</script>`
	for _, e := range lintBody(t, body) {
		if e.Kind == KindMissingControlName {
			t.Errorf("dynamic page should not report an empty control: %s", e.Message)
		}
	}
}

// A <datalist> maps to role=listbox but is never rendered, so demanding a name
// of its own would fail every build that offers input suggestions.
func TestNames_DatalistNeedsNoName(t *testing.T) {
	t.Parallel()

	body := `<label for="q">Search</label><input id="q" type="text" list="dl">` +
		`<datalist id="dl"><option value="alpha"></option></datalist>`
	if errs := lintBody(t, body); len(errs) != 0 {
		t.Errorf("a labelled input with a datalist should lint clean, got %+v", errs)
	}
}
