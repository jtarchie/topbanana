package editrec

import (
	"testing"
	"time"
)

// TestKeyRoundTrip verifies a key built by Key parses back to the same
// timestamp (to the nanosecond) and log key.
func TestKeyRoundTrip(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 6, 8, 12, 30, 45, 123456789, time.UTC)
	gotTS, gotLog := parseKey(Key("my-site", ts, "mcp"))
	if !gotTS.Equal(ts) {
		t.Errorf("parseKey timestamp = %v, want %v", gotTS, ts)
	}
	if gotLog != "mcp" {
		t.Errorf("parseKey logKey = %q, want %q", gotLog, "mcp")
	}
}

// TestParseKeyLegacySecondLayout pins backward compatibility: transcripts
// written with the original second-resolution timestamp must still parse so
// pre-existing build history keeps listing.
func TestParseKeyLegacySecondLayout(t *testing.T) {
	t.Parallel()
	gotTS, gotLog := parseKey(Prefix + "my-site/20260608T123045Z-edit.json")
	want := time.Date(2026, 6, 8, 12, 30, 45, 0, time.UTC)
	if !gotTS.Equal(want) {
		t.Errorf("legacy parseKey timestamp = %v, want %v", gotTS, want)
	}
	if gotLog != "edit" {
		t.Errorf("legacy parseKey logKey = %q, want %q", gotLog, "edit")
	}
}

// TestKeyDistinctSubSecond is the collision regression: before the precision
// bump, two edits in the same wall-clock second produced the same key and the
// second silently overwrote the first. MCP edits fire several per second with
// no per-slug serialization, so the key must distinguish sub-second timestamps.
func TestKeyDistinctSubSecond(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 6, 8, 12, 30, 45, 0, time.UTC)
	k1 := Key("s", base, "mcp")
	k2 := Key("s", base.Add(time.Nanosecond), "mcp")
	if k1 == k2 {
		t.Fatalf("keys for timestamps 1ns apart collided: %q", k1)
	}
}
