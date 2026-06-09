package auth

import (
	"strconv"
	"time"
)

// freshSuffix is a per-test-run unique string so re-runs against the same
// bucket don't see stale data (and so unit runs against the in-memory store
// don't collide between tests in the same process).
func freshSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}
