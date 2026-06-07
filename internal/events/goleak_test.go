package events

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from events tests. The
// Tracker's sweepLoop is paired with Tracker.Close — tests build a tracker
// must defer Close so goleak stays green.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
