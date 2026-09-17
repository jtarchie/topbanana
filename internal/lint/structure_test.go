package lint

import (
	"strings"
	"testing"
)

// The structure rules are warnings, so every case here also asserts they cannot fail a build.
func TestStructure_AllWarn(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want Kind
	}{
		{name: "skipped heading level", body: `<h1>Title</h1><h3>Detail</h3>`, want: KindHeadingOrder},
		{name: "positive tabindex", body: `<button tabindex="3">Save</button>`, want: KindPositiveTabindex},
		{name: "button inside a link", body: `<a href="index.html">Read <button>Buy</button></a>`, want: KindNestedInteractive},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := onlyKind(t, lintBody(t, tc.body), tc.want)
			if got.Severity() != SeverityWarning {
				t.Errorf("%s should be a warning, got %v", tc.want, got.Severity())
			}
			if len(Blocking(lintBody(t, tc.body))) != 0 {
				t.Errorf("%s must not block a build", tc.want)
			}
		})
	}
}

func TestStructure_HeadingOrder(t *testing.T) {
	t.Parallel()

	t.Run("descending one level at a time is clean", func(t *testing.T) {
		t.Parallel()
		if errs := lintBody(t, `<h1>Title</h1><h2>Part</h2><h3>Detail</h3><h2>Part two</h2>`); len(errs) != 0 {
			t.Errorf("expected clean, got %+v", errs)
		}
	})

	t.Run("starting below h1 is a style choice, not a skip", func(t *testing.T) {
		t.Parallel()
		if errs := lintBody(t, `<h2>Part</h2><h3>Detail</h3>`); len(errs) != 0 {
			t.Errorf("expected clean, got %+v", errs)
		}
	})

	t.Run("message names both levels", func(t *testing.T) {
		t.Parallel()
		got := onlyKind(t, lintBody(t, `<h2>Part</h2><h4>Detail</h4>`), KindHeadingOrder)
		if !strings.Contains(got.Message, "<h4>") || !strings.Contains(got.Message, "<h3>") {
			t.Errorf("message should name the level found and the one expected:\n%s", got.Message)
		}
	})
}

func TestStructure_MainLandmark(t *testing.T) {
	t.Parallel()

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		page := strings.Replace(a11yPage(`<h1>Hi</h1>`), "<main>", "<div>", 1)
		page = strings.Replace(page, "</main>", "</div>", 1)
		got := onlyKind(t, lintSite(t, map[string]string{"index.html": page}), KindLandmarkMain)
		if !strings.Contains(got.Message, "no <main> element") {
			t.Errorf("unexpected message:\n%s", got.Message)
		}
	})

	t.Run("duplicated", func(t *testing.T) {
		t.Parallel()
		got := onlyKind(t, lintBody(t, `<h1>Hi</h1></main><main>second`), KindLandmarkMain)
		if !strings.Contains(got.Message, "2 <main>") {
			t.Errorf("message should count the landmarks:\n%s", got.Message)
		}
	})

	t.Run("role=main counts", func(t *testing.T) {
		t.Parallel()
		page := strings.Replace(a11yPage(`<h1>Hi</h1>`), "<main>", `<div role="main">`, 1)
		page = strings.Replace(page, "</main>", "</div>", 1)
		if errs := lintSite(t, map[string]string{"index.html": page}); len(errs) != 0 {
			t.Errorf("role=main should satisfy the landmark, got %+v", errs)
		}
	})
}

func TestStructure_NestedInteractiveExemptions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{name: "select owns its options", body: `<label for="s">Size</label><select id="s" name="size"><option>Small</option></select>`},
		{name: "label wraps its control", body: `<label>Email <input type="email" name="email"></label>`},
		{name: "anchor without href is not a control", body: `<a><button>Buy</button></a>`},
		{name: "controls side by side", body: `<a href="index.html">Read</a> <button>Buy</button>`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, e := range lintBody(t, tc.body) {
				if e.Kind == KindNestedInteractive {
					t.Errorf("unexpected nesting report: %s", e.Message)
				}
			}
		})
	}
}

func TestStructure_TabIndexZeroAndNegativeAreFine(t *testing.T) {
	t.Parallel()

	if errs := lintBody(t, `<div tabindex="0" role="button" aria-label="Open">x</div><span tabindex="-1">y</span>`); len(errs) != 0 {
		t.Errorf("expected clean, got %+v", errs)
	}
}

func TestSeverity_DefaultsToBlocking(t *testing.T) {
	t.Parallel()

	// A Kind with no entry in warningKinds must gate, so a new check is safe by default.
	e := Error{File: "index.html", Message: "x", Kind: Kind("something_new")}
	if e.Severity() != SeverityError {
		t.Errorf("unknown kinds must default to blocking, got %v", e.Severity())
	}
	if len(Blocking([]Error{e})) != 1 {
		t.Error("Blocking should keep an unknown kind")
	}
	warn := Error{File: "index.html", Message: "x", Kind: KindHeadingOrder}
	if len(Blocking([]Error{warn})) != 0 {
		t.Error("Blocking should drop a warning")
	}
}
