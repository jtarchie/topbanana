package auth

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain enforces that no goroutines leak from auth tests. The user and
// session stores are mutex-guarded maps with no background goroutines;
// memAuthSessionStore's sweep is paired with the stop function returned
// from NewMemAuthSessionStore (and surfaced as Auth.Close), so tests that
// build an Auth must defer that Close.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
