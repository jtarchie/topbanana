package lint

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/lint/ariadata"
	"github.com/jtarchie/topbanana/internal/storetest"
)

// Wraps body markup in what every other lint check demands, so a fixture only trips the rule it is written for.
func a11yPage(body string) string {
	return `<!DOCTYPE html><html lang="en"><head>` +
		`<meta charset="utf-8">` +
		`<meta name="viewport" content="width=device-width, initial-scale=1">` +
		`<title>Accessibility fixture</title>` +
		`<meta name="description" content="A fixture page used by the accessibility lint tests.">` +
		`<link rel="stylesheet" href="/app.css">` +
		`</head><body><main>` + body + `</main></body></html>`
}

func lintBody(t *testing.T, body string) []Error {
	t.Helper()
	return lintSite(t, map[string]string{"index.html": a11yPage(body)})
}

func lintSite(t *testing.T, files map[string]string) []Error {
	t.Helper()
	ctx := context.Background()
	s := storetest.New(t, 0)
	slug := "a11y-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	for name, content := range files {
		err := s.Write(ctx, slug, name, content, "text/html; charset=utf-8", nil)
		if err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	return App(ctx, s, slug, nil)
}

func kinds(errs []Error) map[Kind]int {
	out := map[Kind]int{}
	for _, e := range errs {
		out[e.Kind]++
	}
	return out
}

func onlyKind(t *testing.T, errs []Error, want Kind) Error {
	t.Helper()
	var found *Error
	for i := range errs {
		if errs[i].Kind == want {
			found = &errs[i]
			continue
		}
		t.Errorf("unexpected %s error: %s", errs[i].Kind, errs[i].Message)
	}
	if found == nil {
		t.Fatalf("expected a %s error, got %+v", want, kinds(errs))
	}
	return *found
}

func TestARIA_RoleValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want Kind
		says string
	}{
		{
			name: "misspelled role suggests the real one",
			body: `<div role="navigaton"><a href="index.html">Home</a></div>`,
			want: KindARIAInvalidRole,
			says: `Did you mean role="navigation"?`,
		},
		{
			name: "abstract role is named as abstract",
			body: `<div role="widget">x</div>`,
			want: KindARIAInvalidRole,
			says: "abstract role",
		},
		{
			name: "empty role",
			body: `<div role="">x</div>`,
			want: KindARIAInvalidRole,
			says: `empty role=""`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := onlyKind(t, lintBody(t, tc.body), tc.want)
			if !strings.Contains(got.Message, tc.says) {
				t.Errorf("message missing %q:\n%s", tc.says, got.Message)
			}
		})
	}
}

func TestARIA_ValidRolesPass(t *testing.T) {
	t.Parallel()

	body := `<nav role="navigation"><a href="index.html">Home</a></nav>` +
		`<div role="tablist"><button role="tab" aria-selected="true" id="t1">One</button></div>` +
		`<div role="tabpanel" aria-labelledby="t1">Panel</div>` +
		`<div role="none"><span>decorative wrapper</span></div>` +
		`<div role="button presentation" tabindex="0">Fallback list</div>`
	if errs := lintBody(t, body); len(errs) != 0 {
		t.Errorf("expected clean, got %+v", errs)
	}
}

func TestARIA_AttributeNamesAndValues(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want Kind
		says string
	}{
		{
			name: "typo in attribute name",
			body: `<button aria-labell="Save">Save</button>`,
			want: KindARIAInvalidAttr,
			says: "Did you mean aria-label?",
		},
		{
			name: "boolean value outside its grammar",
			body: `<button aria-expanded="yes" aria-controls="p">More</button><div id="p">panel</div>`,
			want: KindARIAInvalidAttrValue,
			says: "true, false, undefined",
		},
		{
			name: "token outside its enum",
			body: `<div aria-live="loud">status</div>`,
			want: KindARIAInvalidAttrValue,
			says: "off, polite, assertive",
		},
		{
			name: "integer attribute given text",
			body: `<div role="heading" aria-level="two">Section</div>`,
			want: KindARIAInvalidAttrValue,
			says: "a whole number",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := onlyKind(t, lintBody(t, tc.body), tc.want)
			if !strings.Contains(got.Message, tc.says) {
				t.Errorf("message missing %q:\n%s", tc.says, got.Message)
			}
		})
	}
}

func TestARIA_BrokenReference(t *testing.T) {
	t.Parallel()

	got := onlyKind(t, lintBody(t, `<h2 id="real">Real</h2><div role="region" aria-labelledby="ghost">x</div>`), KindARIABrokenRef)
	if !strings.Contains(got.Message, "ghost") || !strings.Contains(got.Message, "real") {
		t.Errorf("message should name the missing id and the page's real ids:\n%s", got.Message)
	}

	t.Run("resolving reference is clean", func(t *testing.T) {
		t.Parallel()
		if errs := lintBody(t, `<h2 id="real">Real</h2><div role="region" aria-labelledby="real">x</div>`); len(errs) != 0 {
			t.Errorf("expected clean, got %+v", errs)
		}
	})
}

func TestARIA_RequiredAttrOnlyWhenNoDefault(t *testing.T) {
	t.Parallel()

	// checkbox requires aria-checked outright, while heading's aria-level has a default and must not be demanded.
	got := onlyKind(t, lintBody(t, `<div role="checkbox" tabindex="0">Subscribe</div>`), KindARIARequiredAttr)
	if !strings.Contains(got.Message, "aria-checked") {
		t.Errorf("message should name the missing state:\n%s", got.Message)
	}

	t.Run("implicit default is not required", func(t *testing.T) {
		t.Parallel()
		if errs := lintBody(t, `<div role="heading">Section</div>`); len(errs) != 0 {
			t.Errorf("expected clean, got %+v", errs)
		}
	})

	t.Run("native control is not asked for ARIA state", func(t *testing.T) {
		t.Parallel()
		body := `<label for="c">Subscribe</label><input type="checkbox" id="c" name="c">`
		if errs := lintBody(t, body); len(errs) != 0 {
			t.Errorf("native checkbox should need no aria-checked, got %+v", errs)
		}
	})
}

func TestARIA_RelationshipWarnings(t *testing.T) {
	t.Parallel()

	t.Run("listitem outside a list", func(t *testing.T) {
		t.Parallel()
		errs := lintBody(t, `<div role="listitem">stray</div>`)
		got := onlyKind(t, errs, KindARIARequiredParent)
		if got.Severity() != SeverityWarning {
			t.Errorf("required-parent should be a warning, got %v", got.Severity())
		}
		if !strings.Contains(got.Message, "list") {
			t.Errorf("message should name the container role:\n%s", got.Message)
		}
	})

	t.Run("tablist with no tabs", func(t *testing.T) {
		t.Parallel()
		got := onlyKind(t, lintBody(t, `<div role="tablist"><span>nothing</span></div>`), KindARIARequiredChildren)
		if got.Severity() != SeverityWarning {
			t.Errorf("required-children should be a warning, got %v", got.Severity())
		}
	})

	t.Run("generic wrappers are transparent", func(t *testing.T) {
		t.Parallel()
		body := `<div role="list"><div><span role="listitem">one</span></div></div>`
		if errs := lintBody(t, body); len(errs) != 0 {
			t.Errorf("a plain wrapper should not break the list/listitem pair, got %+v", errs)
		}
	})
}

func TestARIA_ProhibitedNameIsAWarning(t *testing.T) {
	t.Parallel()

	got := onlyKind(t, lintBody(t, `<div aria-label="Wrapper">content</div>`), KindARIAProhibitedAttr)
	if got.Severity() != SeverityWarning {
		t.Errorf("prohibited-attr should be a warning, got %v", got.Severity())
	}
	if !strings.Contains(got.Message, "generic") {
		t.Errorf("message should name the resolved role:\n%s", got.Message)
	}
}

func TestARIA_UnsupportedStateIsAWarning(t *testing.T) {
	t.Parallel()

	got := onlyKind(t, lintBody(t, `<p aria-checked="true">not a checkbox</p>`), KindARIAAllowedAttr)
	if got.Severity() != SeverityWarning {
		t.Errorf("allowed-attr should be a warning, got %v", got.Severity())
	}
}

func TestARIAData_VendorLooksComplete(t *testing.T) {
	t.Parallel()

	if got := len(ariadata.RoleNames()); got < 100 {
		t.Errorf("only %d concrete roles — roles.json looks truncated", got)
	}
	if got := len(ariadata.AttrNames()); got < 45 {
		t.Errorf("only %d aria-* attributes — roles.json looks truncated", got)
	}
	if ariadata.Concrete("widget") {
		t.Error("widget is abstract and must not be writable")
	}
	if !ariadata.Concrete("button") {
		t.Error("button should be concrete")
	}
}

// Every field the checks in this package read, asserted on a role that exercises it.
func TestARIAData_FieldsTheChecksDependOn(t *testing.T) {
	t.Parallel()

	checkbox, ok := ariadata.Get("checkbox")
	if !ok || len(checkbox.Required) != 1 || checkbox.Required[0] != "aria-checked" {
		t.Errorf("checkbox.Required = %v, want [aria-checked]", checkbox.Required)
	}
	if heading, _ := ariadata.Get("heading"); len(heading.Required) != 0 {
		t.Errorf("heading.Required = %v — aria-level has a default and must not be required", heading.Required)
	}
	if generic, _ := ariadata.Get("generic"); !generic.NameProhibited || !generic.Prohibited["aria-label"] {
		t.Error("generic should prohibit naming")
	}
	if button, _ := ariadata.Get("button"); !button.NameRequired || !button.NameFromContents || !button.Props["aria-expanded"] {
		t.Error("button should require a name, take it from contents, and support aria-expanded")
	}
	if listitem, _ := ariadata.Get("listitem"); len(listitem.RequiredContext) == 0 {
		t.Error("listitem should require a list context")
	}
	if list, _ := ariadata.Get("list"); len(list.RequiredOwned) == 0 {
		t.Error("list should require owned listitems")
	}

	// Without the `none` -> presentation normalization every role="none" element would look like it permits nothing.
	none, ok := ariadata.Get("none")
	if !ok || !none.Prohibited["aria-label"] || len(none.Props) == 0 {
		t.Error("role=none should mirror presentation")
	}
}
