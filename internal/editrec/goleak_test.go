package editrec

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from editrec tests. The edit
// recorder is a pure log writer; nothing should outlive the test process.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
