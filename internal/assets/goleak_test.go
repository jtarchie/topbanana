package assets_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from assets tests. The assets
// package only serves an embedded stylesheet — purely synchronous.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
