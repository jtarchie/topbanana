package agent

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from agent tests. ADK can spawn
// goroutines per tool call; tests that drive the runner must use bounded
// contexts so those goroutines exit before the test returns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
