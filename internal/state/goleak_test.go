package state_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from state tests. The memory
// backend is goroutine-free; the S3 backend uses the AWS SDK's pooled
// transport whose idle conn-reapers tear down inside Close, so leaks here
// flag missing Close() pairings rather than SDK issues.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
