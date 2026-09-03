package auth

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/auth/blob"
)

// slowBlobs adds a fixed delay to every Get, standing in for the round trip
// to a remote object store. Everything else passes through.
type slowBlobs struct {
	blob.Blobs
	delay time.Duration
}

func (s slowBlobs) Get(ctx context.Context, key string) (blob.Object, error) {
	time.Sleep(s.delay)

	obj, err := s.Blobs.Get(ctx, key)
	if err != nil {
		return obj, fmt.Errorf("slowBlobs get %s: %w", key, err)
	}

	return obj, nil
}

// listRecords is how many records each listing test writes. Enough that the
// serial cost is unmistakable against the parallel one.
const listRecords = 24

// TestListsReadInParallel pins the property the admin console depends on: a
// listing costs about one round trip, not one per record. With a 20ms Get,
// serial reads take 24*20ms ≈ 480ms; the bound of 16 makes the parallel
// version two waves, ≈ 40ms. The 200ms ceiling sits well clear of both, so
// the test fails on a serial implementation without being flaky on a loaded
// machine.
func TestListsReadInParallel(t *testing.T) {
	t.Parallel()

	const delay = 20 * time.Millisecond
	const ceiling = 200 * time.Millisecond

	ctx := context.Background()

	t.Run("users", func(t *testing.T) {
		t.Parallel()

		st := slowBlobs{Blobs: blob.NewMemory(), delay: delay}

		us, err := NewUserStore(st)
		if err != nil {
			t.Fatalf("NewUserStore: %v", err)
		}
		suffix := freshSuffix()

		for i := range listRecords {
			email := "listed" + string(rune('a'+i)) + "+" + suffix + "@example.com"
			err := us.Save(ctx, &User{Email: email, Role: RoleAdmin, Created: time.Now().UTC()})
			if err != nil {
				t.Fatalf("Save: %v", err)
			}
		}

		start := time.Now()
		users, err := us.List(ctx)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(users) != listRecords {
			t.Fatalf("List returned %d users, want %d", len(users), listRecords)
		}
		if elapsed > ceiling {
			t.Errorf("List took %v, want under %v — reads are serial", elapsed, ceiling)
		}
	})

	t.Run("invites", func(t *testing.T) {
		t.Parallel()

		st := slowBlobs{Blobs: blob.NewMemory(), delay: delay}
		is := NewInviteStore(st)
		for i := range listRecords {
			_, err := is.Issue(ctx, "invited"+string(rune('a'+i))+"@example.com", RoleAdmin, nil, DefaultInviteTTL)
			if err != nil {
				t.Fatalf("Issue: %v", err)
			}
		}

		start := time.Now()
		invites, err := is.List(ctx)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(invites) != listRecords {
			t.Fatalf("List returned %d invites, want %d", len(invites), listRecords)
		}
		if elapsed > ceiling {
			t.Errorf("List took %v, want under %v — reads are serial", elapsed, ceiling)
		}
	})
}
