package store_test

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from store tests. The store
// wraps the AWS S3 SDK; tests that exercise it must let the SDK's pooled
// transport return to idle before they finish (a leftover goroutine here
// usually means a deferred Close was missed).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
