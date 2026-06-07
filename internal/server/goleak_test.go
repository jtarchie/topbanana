package server

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from server tests. The server
// wires Build / Events / Auth — all of which expose Close hooks now — and
// httptest.Server cleans up its own listener on Close. Any leak here points
// at a missing defer somewhere in the test rig.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
