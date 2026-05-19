package server

import "testing"

// chromeFixture is a stand-in for any real page-data struct: it embeds
// Chrome by value, the way every chrome-using page-data struct in this
// package does. The tests below pin the interface contract that makes
// injectChrome work: addressing the outer struct via *T yields a chromed
// whose chromePtr returns the embedded Chrome — mutations through it
// land on the outer struct, not on a copy.
type chromeFixture struct {
	Chrome
	OtherField string
}

func TestChromeInterfaceWiring(t *testing.T) {
	t.Run("chromePtr returns the embedded chrome by reference", func(t *testing.T) {
		v := &chromeFixture{OtherField: "preserved"}
		var ch chromed = v
		ch.chromePtr().IsSuperAdmin = true
		if !v.IsSuperAdmin {
			t.Fatalf("expected outer struct to see IsSuperAdmin=true after mutation; got false")
		}
		if v.OtherField != "preserved" {
			t.Fatalf("mutation through chromePtr should not have touched OtherField; got %q", v.OtherField)
		}
	})

	t.Run("repeated mutation is idempotent", func(t *testing.T) {
		v := &chromeFixture{}
		var ch chromed = v
		ch.chromePtr().IsSuperAdmin = true
		ch.chromePtr().IsSuperAdmin = true
		if !v.IsSuperAdmin {
			t.Fatalf("expected idempotent true after two writes")
		}
		ch.chromePtr().IsSuperAdmin = false
		if v.IsSuperAdmin {
			t.Fatalf("expected mutation back to false")
		}
	})

	t.Run("non-chromed struct silently fails the type assertion", func(t *testing.T) {
		type bare struct{ X int }
		var data any = &bare{X: 1}
		_, ok := data.(chromed)
		if ok {
			t.Fatalf("plain struct without embedded Chrome should not satisfy chromed")
		}
	})

	t.Run("Chrome by value embedded in a value receiver does not satisfy chromed", func(t *testing.T) {
		// Sanity-check the rewrap-to-pointer step in injectChrome — without
		// it, a struct-value caller would not satisfy the interface.
		v := chromeFixture{}
		_, ok := any(v).(chromed)
		if ok {
			t.Fatalf("struct value should NOT satisfy chromed (pointer-receiver method)")
		}
	})
}
