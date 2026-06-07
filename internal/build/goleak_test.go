package build

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from build tests. The build
// pipeline spawns a goroutine per Service.Start, so any test that starts a
// build must also drain it (Service.Wait, status-tracked completion, or
// context-cancel). A leak here points at a build the test forgot to await.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
