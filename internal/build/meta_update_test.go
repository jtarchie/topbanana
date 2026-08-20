package build

import (
	"context"
	"errors"
	"testing"
)

// TestUpdateMeta_RetriesPastAConcurrentWriter is the whole point of the
// compare-and-set: a second writer landing between our read and our write must
// cost a retry, not the other writer's change. The mutate closure fires the
// competing write on its first call only, which is exactly the interleaving a
// plain read-modify-write loses.
func TestUpdateMeta_RetriesPastAConcurrentWriter(t *testing.T) {
	st := minioStoreForBuild(t)
	svc := NewWithConfig(Config{Store: st})
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	err := svc.WriteMeta(ctx, slug, SiteMeta{Template: "blank", OwnerID: "owner@test"})
	if err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	calls := 0
	final, err := svc.UpdateMeta(ctx, slug, func(m *SiteMeta) {
		calls++
		if calls == 1 {
			// Someone else's change, committed after we read and before we write.
			werr := svc.WriteMeta(ctx, slug, SiteMeta{
				Template: "blank", OwnerID: "owner@test", Private: true,
			})
			if werr != nil {
				t.Fatalf("competing write: %v", werr)
			}
		}
		m.Collaborators = []string{"bob@test"}
	})
	if err != nil {
		t.Fatalf("UpdateMeta: %v", err)
	}
	if calls < 2 {
		t.Fatalf("mutate ran %d time(s); the stale write should have lost and retried", calls)
	}
	if !final.Private {
		t.Error("the concurrent writer's Private flag was clobbered")
	}
	if len(final.Collaborators) != 1 || final.Collaborators[0] != "bob@test" {
		t.Errorf("our own change was lost: %v", final.Collaborators)
	}

	// And it is what's actually stored, not just what we returned.
	stored := svc.ReadMeta(ctx, slug)
	if !stored.Private || len(stored.Collaborators) != 1 {
		t.Errorf("persisted meta = %+v; want both changes", stored)
	}
}

// TestUpdateMeta_CreatesWhenAbsent covers the first write to a site that has no
// sidecar yet: an empty ETag means "must not exist", so creation is still a
// one-winner race rather than a blind overwrite.
func TestUpdateMeta_CreatesWhenAbsent(t *testing.T) {
	st := minioStoreForBuild(t)
	svc := NewWithConfig(Config{Store: st})
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	meta, err := svc.UpdateMeta(ctx, slug, func(m *SiteMeta) {
		m.OwnerID = "owner@test"
		m.Template = "blank"
	})
	if err != nil {
		t.Fatalf("UpdateMeta on a fresh slug: %v", err)
	}
	if meta.OwnerID != "owner@test" {
		t.Errorf("OwnerID = %q", meta.OwnerID)
	}
	if got := svc.ReadMeta(ctx, slug); got.OwnerID != "owner@test" {
		t.Errorf("persisted OwnerID = %q", got.OwnerID)
	}
}

// TestUpdateMeta_GivesUpAfterRepeatedLosses pins the bound: a writer that can
// never win reports ErrMetaConflict instead of spinning, and — the part that
// matters — never writes.
func TestUpdateMeta_GivesUpAfterRepeatedLosses(t *testing.T) {
	st := minioStoreForBuild(t)
	svc := NewWithConfig(Config{Store: st})
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	err := svc.WriteMeta(ctx, slug, SiteMeta{Template: "blank", OwnerID: "owner@test"})
	if err != nil {
		t.Fatalf("seed meta: %v", err)
	}

	_, err = svc.UpdateMeta(ctx, slug, func(m *SiteMeta) {
		// A competitor on every single attempt.
		werr := svc.WriteMeta(ctx, slug, SiteMeta{
			Template: "blank", OwnerID: "owner@test", Title: buildSuffix(),
		})
		if werr != nil {
			t.Fatalf("competing write: %v", werr)
		}
		m.Collaborators = []string{"bob@test"}
	})
	if !errors.Is(err, ErrMetaConflict) {
		t.Fatalf("err = %v, want ErrMetaConflict", err)
	}
	if got := svc.ReadMeta(ctx, slug); len(got.Collaborators) != 0 {
		t.Errorf("a give-up still wrote: %v", got.Collaborators)
	}
}
