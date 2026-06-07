package portable_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from portable tests. The
// portable package archives sites synchronously; there should be nothing
// running in the background when tests finish.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
