package lint

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from lint tests. The lint
// package is purely synchronous; if a test starts to leak, it points at a
// new code path that needs an explicit Stop / Close or context cancel.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
