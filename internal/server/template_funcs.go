package server

import "reflect"

// templateBoolField and templateStringField are reflection-based tolerant
// field accessors registered as template funcs. They let the shared chrome
// partials (brand, site_subnav) read fields like .SiteName, .Slug,
// .Active, and .IsSuperAdmin off any page-data struct without that struct
// having to declare every field — html/template's default behaviour is to
// error on missing fields, which makes the brand partial a leaky abstraction
// that couples every page to the chrome's full surface.
//
// Missing / wrong-typed / nil inputs all return the zero value of the
// requested kind. Both helpers handle either a struct value or a *struct.

func templateBoolField(data any, name string) bool {
	v := derefStruct(data)
	if !v.IsValid() {
		return false
	}
	f := v.FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.Bool {
		return false
	}
	return f.Bool()
}

func templateStringField(data any, name string) string {
	v := derefStruct(data)
	if !v.IsValid() {
		return ""
	}
	f := v.FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.String {
		return ""
	}
	return f.String()
}

// derefStruct returns the underlying reflect.Value of `data` if it's a
// struct (or pointer-to-struct), and an invalid Value otherwise. The two
// helpers above both want exactly that behaviour, so factor it out.
func derefStruct(data any) reflect.Value {
	if data == nil {
		return reflect.Value{}
	}
	v := reflect.ValueOf(data)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v
}

// injectBoolField sets a named bool field on `data` to `value`. Handles
// struct, *struct, and map[string]any. Silently no-ops when the target
// doesn't have a matching settable bool field — same tolerance as the
// read helpers, so render() can inject chrome values without every page
// having to know.
//
// Used by render() to push IsSuperAdmin onto every page-data struct so
// the shared brand partial gates the "Users" link consistently
// regardless of which handler produced the data.
func injectBoolField(data any, name string, value bool) {
	if data == nil {
		return
	}
	// Map case first: handlers like landingHandler / function_edit use
	// map[string]any for their template data, so the struct path below
	// would fall through without writing anything.
	if m, ok := data.(map[string]any); ok {
		m[name] = value
		return
	}
	v := reflect.ValueOf(data)
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return
	}
	f := v.FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.Bool || !f.CanSet() {
		return
	}
	f.SetBool(value)
}
