package templates

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from templates tests. The
// registry is populated synchronously at init; tests only read it.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
