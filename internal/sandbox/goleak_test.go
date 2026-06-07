package sandbox

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from sandbox tests. The
// sandbox holds CPU/memory limits behind a mutex — no background workers —
// so any leak here is a test mistake.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
