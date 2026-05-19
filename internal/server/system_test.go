package server

import (
	"testing"
	"time"

	"github.com/jtarchie/buildabear/internal/editrec"
)

func TestSummarizeBuilds(t *testing.T) {
	finished := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	started := finished.Add(-time.Minute)

	cases := []struct {
		name         string
		in           []editrec.Transcript
		wantSuccess  int
		wantFailed   int
		wantInFlight int
	}{
		{
			name:        "empty slice",
			in:          nil,
			wantSuccess: 0, wantFailed: 0, wantInFlight: 0,
		},
		{
			name: "all completed",
			in: []editrec.Transcript{
				{StartedAt: started, FinishedAt: finished, FinalStatus: "completed"},
				{StartedAt: started, FinishedAt: finished, FinalStatus: "completed"},
			},
			wantSuccess: 2,
		},
		{
			name: "mix of completed and failed",
			in: []editrec.Transcript{
				{StartedAt: started, FinishedAt: finished, FinalStatus: "completed"},
				{StartedAt: started, FinishedAt: finished, FinalStatus: "failed"},
				{StartedAt: started, FinishedAt: finished, FinalStatus: "completed"},
			},
			wantSuccess: 2, wantFailed: 1,
		},
		{
			name: "unset FinishedAt is in-flight",
			in: []editrec.Transcript{
				{StartedAt: started, FinishedAt: finished, FinalStatus: "completed"},
				{StartedAt: started}, // FinishedAt zero
			},
			wantSuccess: 1, wantInFlight: 1,
		},
		{
			name: "FinishedAt set but FinalStatus empty counts as failed",
			in: []editrec.Transcript{
				{StartedAt: started, FinishedAt: finished},
			},
			wantFailed: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotS, gotF, gotI := summarizeBuilds(tc.in)
			if gotS != tc.wantSuccess || gotF != tc.wantFailed || gotI != tc.wantInFlight {
				t.Errorf("got (success=%d, failed=%d, inFlight=%d), want (success=%d, failed=%d, inFlight=%d)",
					gotS, gotF, gotI, tc.wantSuccess, tc.wantFailed, tc.wantInFlight)
			}
		})
	}
}

func TestMostRecentListings(t *testing.T) {
	t0 := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	mk := func(slug string, offsets ...time.Duration) (string, []editrec.Listing) {
		out := make([]editrec.Listing, 0, len(offsets))
		for _, d := range offsets {
			out = append(out, editrec.Listing{Key: slug + "-" + d.String(), Timestamp: t0.Add(d), LogKey: "edit"})
		}
		return slug, out
	}

	t.Run("empty input", func(t *testing.T) {
		got := mostRecentListings(map[string][]editrec.Listing{}, 5)
		if len(got) != 0 {
			t.Errorf("expected empty, got %d", len(got))
		}
	})

	t.Run("fewer than n entries returns all sorted desc", func(t *testing.T) {
		slug, ls := mk("alpha", -3*time.Minute, -1*time.Minute, -5*time.Minute)
		got := mostRecentListings(map[string][]editrec.Listing{slug: ls}, 10)
		if len(got) != 3 {
			t.Fatalf("len: got %d want 3", len(got))
		}
		// Sorted descending by timestamp: -1min, -3min, -5min
		want := []time.Duration{-1 * time.Minute, -3 * time.Minute, -5 * time.Minute}
		for i, g := range got {
			if !g.Timestamp.Equal(t0.Add(want[i])) {
				t.Errorf("idx %d: got %v want offset %v", i, g.Timestamp, want[i])
			}
		}
	})

	t.Run("more than n entries trims to top n", func(t *testing.T) {
		slug, ls := mk("alpha", -5*time.Minute, -4*time.Minute, -3*time.Minute, -2*time.Minute, -1*time.Minute)
		got := mostRecentListings(map[string][]editrec.Listing{slug: ls}, 2)
		if len(got) != 2 {
			t.Fatalf("len: got %d want 2", len(got))
		}
		if !got[0].Timestamp.Equal(t0.Add(-1 * time.Minute)) {
			t.Errorf("first: got %v want -1m", got[0].Timestamp)
		}
		if !got[1].Timestamp.Equal(t0.Add(-2 * time.Minute)) {
			t.Errorf("second: got %v want -2m", got[1].Timestamp)
		}
	})

	t.Run("interleaves slugs and preserves slug attribution", func(t *testing.T) {
		slugA, lsA := mk("alpha", -1*time.Minute, -3*time.Minute)
		slugB, lsB := mk("bravo", -2*time.Minute, -4*time.Minute)
		got := mostRecentListings(map[string][]editrec.Listing{slugA: lsA, slugB: lsB}, 10)
		if len(got) != 4 {
			t.Fatalf("len: got %d want 4", len(got))
		}
		wantSlugs := []string{"alpha", "bravo", "alpha", "bravo"}
		for i, g := range got {
			if g.Slug != wantSlugs[i] {
				t.Errorf("idx %d slug: got %q want %q", i, g.Slug, wantSlugs[i])
			}
		}
	})

	t.Run("n=0 returns everything (no trim)", func(t *testing.T) {
		slug, ls := mk("alpha", -1*time.Minute, -2*time.Minute, -3*time.Minute)
		got := mostRecentListings(map[string][]editrec.Listing{slug: ls}, 0)
		if len(got) != 3 {
			t.Fatalf("len: got %d want 3", len(got))
		}
	})
}

func TestAggregateStorage(t *testing.T) {
	t.Run("empty input produces empty rows and zero total", func(t *testing.T) {
		got := aggregateStorage(nil)
		if len(got.Rows) != 0 {
			t.Errorf("rows: got %d want 0", len(got.Rows))
		}
		if got.TotalBytes != 0 || got.TotalCount != 0 {
			t.Errorf("totals: got bytes=%d count=%d want zero", got.TotalBytes, got.TotalCount)
		}
	})

	t.Run("apps only", func(t *testing.T) {
		got := aggregateStorage([]storageBreakdownEntry{
			{Label: "Apps", Bytes: 1024, Count: 3},
			{Label: "Snapshots", Bytes: 0, Count: 0},
		})
		if len(got.Rows) != 2 {
			t.Fatalf("rows: got %d want 2", len(got.Rows))
		}
		if got.TotalBytes != 1024 {
			t.Errorf("total bytes: got %d want 1024", got.TotalBytes)
		}
		if got.TotalCount != 3 {
			t.Errorf("total count: got %d want 3", got.TotalCount)
		}
		if got.Rows[0].BytesLabel == "" {
			t.Errorf("expected non-empty humanized label on row 0")
		}
	})

	t.Run("multiple categories sum correctly", func(t *testing.T) {
		got := aggregateStorage([]storageBreakdownEntry{
			{Label: "Apps", Bytes: 100, Count: 1},
			{Label: "Snapshots", Bytes: 200, Count: 2},
			{Label: "Transcripts", Bytes: 50, Count: 5},
			{Label: "ACME", Bytes: 0, Count: 0},
			{Label: "State", Bytes: 10, Count: 1},
		})
		if got.TotalBytes != 360 {
			t.Errorf("total bytes: got %d want 360", got.TotalBytes)
		}
		if got.TotalCount != 9 {
			t.Errorf("total count: got %d want 9", got.TotalCount)
		}
		// Order is preserved from input — important for visual stability.
		wantLabels := []string{"Apps", "Snapshots", "Transcripts", "ACME", "State"}
		for i, w := range wantLabels {
			if got.Rows[i].Label != w {
				t.Errorf("row %d label: got %q want %q", i, got.Rows[i].Label, w)
			}
		}
	})
}
