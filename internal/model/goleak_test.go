package model

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from model tests. Model
// resolution is a pure mapping over CLI flags + env vars; any leak here
// means a new provider client wasn't shut down.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
