package server

import "testing"

// White-box tests for the tolerant template field accessors. The brand +
// site_subnav partials rely on these returning zero values for missing /
// wrong-type / nil inputs, so each branch matters — a regression here
// reintroduces the "can't evaluate field X" runtime template error
// across half the site.

type chromeFixture struct {
	SiteName string
	Active   string
	Empty    string
	IsSuper  bool
	Disabled bool
}

func TestTemplateStringField(t *testing.T) {
	full := chromeFixture{SiteName: "Bear", Active: "workspace"}
	cases := []struct {
		name  string
		data  any
		field string
		want  string
	}{
		{"happy path", full, "SiteName", "Bear"},
		{"happy path second field", full, "Active", "workspace"},
		{"missing field", full, "DoesNotExist", ""},
		{"empty string field", full, "Empty", ""},
		{"wrong type", full, "IsSuper", ""}, // IsSuper is a bool, not a string
		{"nil input", nil, "SiteName", ""},
		{"non-struct input", "just a string", "SiteName", ""},
		{"pointer to struct", &full, "SiteName", "Bear"},
		{"nil pointer", (*chromeFixture)(nil), "SiteName", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := templateStringField(tc.data, tc.field)
			if got != tc.want {
				t.Fatalf("templateStringField(%q): got %q want %q", tc.field, got, tc.want)
			}
		})
	}
}

func TestTemplateBoolField(t *testing.T) {
	full := chromeFixture{IsSuper: true, SiteName: "Bear"}
	cases := []struct {
		name  string
		data  any
		field string
		want  bool
	}{
		{"true field", full, "IsSuper", true},
		{"false field", full, "Disabled", false},
		{"missing field", full, "DoesNotExist", false},
		{"wrong type", full, "SiteName", false}, // SiteName is a string
		{"nil input", nil, "IsSuper", false},
		{"non-struct input", 42, "IsSuper", false},
		{"pointer to struct", &full, "IsSuper", true},
		{"nil pointer", (*chromeFixture)(nil), "IsSuper", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := templateBoolField(tc.data, tc.field)
			if got != tc.want {
				t.Fatalf("templateBoolField(%q): got %v want %v", tc.field, got, tc.want)
			}
		})
	}
}
