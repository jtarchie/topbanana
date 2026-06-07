package snapshot_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from snapshot tests. The
// snapshot service walks S3 synchronously and has no background sweeper —
// any leak here is a sign of unsealed context cancels.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
