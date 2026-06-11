package guide

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from guide tests. Detection is pure
// HTML parsing over an in-memory store — nothing here should spawn a goroutine.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
