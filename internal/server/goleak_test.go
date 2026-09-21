package server

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from server tests. The server
// wires Build / Events / Auth — all of which expose Close hooks now — and
// httptest.Server cleans up its own listener on Close. Any leak here points
// at a missing defer somewhere in the test rig.
// regexp2 (via chroma's lexers, used by the function-editor syntax
// highlighter) parks a package-level clock goroutine that sleeps itself out;
// it belongs to the dependency, not to anything the server owns.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreAnyFunction("github.com/dlclark/regexp2/v2.runClock"),
	)
}
