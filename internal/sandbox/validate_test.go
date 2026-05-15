package sandbox

import (
	"strings"
	"testing"
)

func TestValidateInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		input     map[string]any
		schema    map[string]any
		wantData  map[string]any
		wantErrs  map[string]string // field -> error substring; empty == no errors
		dropExtra string            // a key in input that must NOT appear in wantData
	}{
		{
			name: "happy path string + email",
			input: map[string]any{
				"name":  "Alice",
				"email": "alice@example.com",
				"extra": "ignored",
			},
			schema: map[string]any{
				"name":  map[string]any{"type": "string", "required": true, "maxLen": int64(60)},
				"email": map[string]any{"type": "email", "required": true, "maxLen": int64(200)},
			},
			wantData: map[string]any{
				"name":  "Alice",
				"email": "alice@example.com",
			},
			dropExtra: "extra",
		},
		{
			name:   "missing required",
			input:  map[string]any{},
			schema: map[string]any{"name": map[string]any{"type": "string", "required": true}},
			wantErrs: map[string]string{
				"name": "required",
			},
		},
		{
			name:  "default maxLen rejects 2 KiB string",
			input: map[string]any{"note": strings.Repeat("x", 2048)},
			schema: map[string]any{
				"note": map[string]any{"type": "string"},
			},
			wantErrs: map[string]string{
				"note": "at most",
			},
		},
		{
			name:  "explicit maxLen overrides default",
			input: map[string]any{"note": strings.Repeat("x", 100)},
			schema: map[string]any{
				"note": map[string]any{"type": "string", "maxLen": int64(50)},
			},
			wantErrs: map[string]string{
				"note": "at most 50",
			},
		},
		{
			name:  "trim before length check",
			input: map[string]any{"name": "   Alice   "},
			schema: map[string]any{
				"name": map[string]any{"type": "string", "trim": true, "maxLen": int64(5)},
			},
			wantData: map[string]any{"name": "Alice"},
		},
		{
			name:  "pattern enforces shape",
			input: map[string]any{"slug": "Has Spaces"},
			schema: map[string]any{
				"slug": map[string]any{"type": "string", "pattern": `^[a-z-]+$`},
			},
			wantErrs: map[string]string{"slug": "invalid"},
		},
		{
			name:  "email shape rejects garbage",
			input: map[string]any{"email": "not-an-email"},
			schema: map[string]any{
				"email": map[string]any{"type": "email", "required": true},
			},
			wantErrs: map[string]string{"email": "valid email"},
		},
		{
			name:  "url requires scheme",
			input: map[string]any{"site": "example.com"},
			schema: map[string]any{
				"site": map[string]any{"type": "url"},
			},
			wantErrs: map[string]string{"site": "valid URL"},
		},
		{
			name:  "integer coerces from form string",
			input: map[string]any{"age": "42"},
			schema: map[string]any{
				"age": map[string]any{"type": "integer", "min": int64(0), "max": int64(120)},
			},
			wantData: map[string]any{"age": int64(42)},
		},
		{
			name:  "integer rejects fractional",
			input: map[string]any{"age": "12.5"},
			schema: map[string]any{
				"age": map[string]any{"type": "integer"},
			},
			wantErrs: map[string]string{"age": "integer"},
		},
		{
			name:  "integer out of range",
			input: map[string]any{"age": int64(200)},
			schema: map[string]any{
				"age": map[string]any{"type": "integer", "max": int64(120)},
			},
			wantErrs: map[string]string{"age": "at most"},
		},
		{
			name:  "boolean from on/off",
			input: map[string]any{"agree": "on"},
			schema: map[string]any{
				"agree": map[string]any{"type": "boolean", "required": true},
			},
			wantData: map[string]any{"agree": true},
		},
		{
			name:  "boolean required false fails",
			input: map[string]any{"agree": "off"},
			schema: map[string]any{
				"agree": map[string]any{"type": "boolean", "required": true},
			},
			wantErrs: map[string]string{"agree": "required"},
		},
		{
			name:   "unknown type surfaces clearly",
			input:  map[string]any{"x": "y"},
			schema: map[string]any{"x": map[string]any{"type": "uuid"}},
			wantErrs: map[string]string{
				"x": "unknown type",
			},
		},
		{
			name:     "empty optional string drops out",
			input:    map[string]any{"middle": ""},
			schema:   map[string]any{"middle": map[string]any{"type": "string"}},
			wantData: map[string]any{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			data, errs := validateInput(tc.input, tc.schema)
			if len(tc.wantErrs) > 0 {
				assertValidationErrors(t, errs, tc.wantErrs, data)
				return
			}
			assertValidationSuccess(t, data, errs, tc.wantData, tc.dropExtra)
		})
	}
}

func assertValidationErrors(t *testing.T, errs []validationError, want map[string]string, data map[string]any) {
	t.Helper()
	if len(errs) == 0 {
		t.Fatalf("expected errors %v, got none; data=%v", want, data)
	}
	got := map[string]string{}
	for _, e := range errs {
		got[e.Field] = e.Message
	}
	for field, sub := range want {
		msg, ok := got[field]
		if !ok {
			t.Errorf("missing error on field %q (got %v)", field, got)
			continue
		}
		if !strings.Contains(msg, sub) {
			t.Errorf("error on %q = %q, want substring %q", field, msg, sub)
		}
	}
}

func assertValidationSuccess(t *testing.T, data map[string]any, errs []validationError, wantData map[string]any, dropExtra string) {
	t.Helper()
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	for k, want := range wantData {
		got, ok := data[k]
		if !ok {
			t.Errorf("missing field %q in data", k)
			continue
		}
		if got != want {
			t.Errorf("field %q = %v, want %v", k, got, want)
		}
	}
	if dropExtra != "" {
		if _, ok := data[dropExtra]; ok {
			t.Errorf("extra field %q should have been dropped", dropExtra)
		}
	}
}
