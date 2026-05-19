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

func TestInjectBoolField(t *testing.T) {
	t.Run("sets struct via pointer", func(t *testing.T) {
		v := &chromeFixture{}
		injectBoolField(v, "IsSuper", true)
		if !v.IsSuper {
			t.Fatalf("expected IsSuper=true on *struct, got false")
		}
	})

	t.Run("sets map[string]any in place", func(t *testing.T) {
		m := map[string]any{"existing": "preserved"}
		injectBoolField(m, "IsSuperAdmin", true)
		if got := m["IsSuperAdmin"]; got != true {
			t.Fatalf("expected map[IsSuperAdmin]=true, got %v", got)
		}
		if got := m["existing"]; got != "preserved" {
			t.Fatalf("map mutation clobbered an unrelated key: got %v", got)
		}
	})

	t.Run("plain struct (non-addressable) silently no-ops", func(t *testing.T) {
		// Passing a struct value (not a pointer) yields a non-settable
		// reflect.Value. The injection should silently skip rather than
		// panic — this exercises the f.CanSet branch.
		v := chromeFixture{}
		injectBoolField(v, "IsSuper", true)
		if v.IsSuper {
			t.Fatalf("non-addressable value should not have been mutated, got true")
		}
	})

	t.Run("missing field no-ops", func(t *testing.T) {
		v := &chromeFixture{}
		injectBoolField(v, "NotAField", true)
		// no panic + struct unchanged
		if v.IsSuper || v.Disabled {
			t.Fatalf("missing-field write should leave fixture untouched")
		}
	})

	t.Run("wrong-type field no-ops", func(t *testing.T) {
		v := &chromeFixture{SiteName: "Bear"}
		injectBoolField(v, "SiteName", true)
		if v.SiteName != "Bear" {
			t.Fatalf("write to a string field should be skipped, got %q", v.SiteName)
		}
	})

	t.Run("nil input no-ops", func(t *testing.T) {
		// Just confirm no panic.
		injectBoolField(nil, "IsSuper", true)
	})

	t.Run("non-struct non-map no-ops", func(t *testing.T) {
		injectBoolField("just a string", "IsSuper", true)
		injectBoolField(42, "IsSuper", true)
	})
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
