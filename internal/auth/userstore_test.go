package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

func TestUserStore_SaveLoadRoundtrip(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	ctx := context.Background()
	email := "round+" + freshSuffix() + "@example.com"
	t.Cleanup(func() { _ = us.Delete(ctx, email) })

	err = us.Save(ctx, &User{Email: email, Role: RoleAdmin, Quotas: Quotas{MaxApps: 2}, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := us.Load(ctx, email)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Email != email {
		t.Errorf("Email = %q, want %q", got.Email, email)
	}
	if got.Role != RoleAdmin {
		t.Errorf("Role = %q, want admin", got.Role)
	}
	if got.Quotas.MaxApps != 2 {
		t.Errorf("Quotas not persisted: %+v", got.Quotas)
	}
}

func TestUserStore_LoadMissingReturnsErrUserNotFound(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	_, err = us.Load(context.Background(), "missing+"+freshSuffix()+"@example.com")
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("err = %v, want ErrUserNotFound", err)
	}
}

func TestUserStore_SaveRejectsEmptyEmail(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	err = us.Save(context.Background(), &User{Email: "   "}) // trims to empty
	if err == nil {
		t.Errorf("Save accepted blank email")
	}
}

func TestUserStore_EmailNormalisedOnSaveAndLoad(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	ctx := context.Background()
	suffix := freshSuffix()
	raw := "  MiXeD+" + suffix + "@Example.com  "
	canonical := NormalizeEmail(raw)
	t.Cleanup(func() { _ = us.Delete(ctx, canonical) })

	err = us.Save(ctx, &User{Email: raw, Role: RoleAdmin, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Load by the raw mixed-case form also resolves to the same record.
	got, err := us.Load(ctx, raw)
	if err != nil {
		t.Fatalf("Load(raw): %v", err)
	}
	if got.Email != canonical {
		t.Errorf("stored Email = %q, want canonical %q", got.Email, canonical)
	}
}

func TestUserStore_LookupCachedHitAvoidsLoad(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	ctx := context.Background()
	email := "cache+" + freshSuffix() + "@example.com"
	t.Cleanup(func() { _ = us.Delete(ctx, email) })

	err = us.Save(ctx, &User{Email: email, Role: RoleAdmin, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	// First lookup populates the cache (Save also adds it).
	first, err := us.LookupCached(ctx, email)
	if err != nil {
		t.Fatalf("LookupCached first: %v", err)
	}
	// Mutate the in-memory record; the cache returns the same pointer so a
	// second LookupCached should see the mutation. This is how middleware
	// observes role changes between requests without going back to S3.
	first.Role = "experimental"
	second, err := us.LookupCached(ctx, email)
	if err != nil {
		t.Fatalf("LookupCached second: %v", err)
	}
	if second.Role != "experimental" {
		t.Errorf("cache hit returned a different object; expected the shared pointer")
	}
}

func TestUserStore_SaveInvalidatesCache(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	ctx := context.Background()
	email := "inv+" + freshSuffix() + "@example.com"
	t.Cleanup(func() { _ = us.Delete(ctx, email) })

	err = us.Save(ctx, &User{Email: email, Role: RoleAdmin, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Save first: %v", err)
	}
	_, err = us.LookupCached(ctx, email) // populate cache
	if err != nil {
		t.Fatalf("LookupCached: %v", err)
	}

	err = us.Save(ctx, &User{Email: email, Role: RoleSuperAdmin, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Save second: %v", err)
	}
	got, err := us.LookupCached(ctx, email)
	if err != nil {
		t.Fatalf("LookupCached after second Save: %v", err)
	}
	if got.Role != RoleSuperAdmin {
		t.Errorf("cache returned stale Role %q after Save", got.Role)
	}
}

func TestUserStore_DeleteRemovesRecordAndCache(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	ctx := context.Background()
	email := "del+" + freshSuffix() + "@example.com"

	err = us.Save(ctx, &User{Email: email, Role: RoleAdmin, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	_, err = us.LookupCached(ctx, email) // warm cache
	if err != nil {
		t.Fatalf("LookupCached: %v", err)
	}

	err = us.Delete(ctx, email)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = us.LookupCached(ctx, email)
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("LookupCached after Delete = %v, want ErrUserNotFound", err)
	}
}

func TestUserStore_ListIncludesSaved(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	ctx := context.Background()
	email := "list+" + freshSuffix() + "@example.com"
	t.Cleanup(func() { _ = us.Delete(ctx, email) })

	err = us.Save(ctx, &User{Email: email, Role: RoleAdmin, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	users, err := us.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found bool
	for _, u := range users {
		if u.Email == email {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("List did not include %q", email)
	}
}

func TestUserStore_ConcurrentPutCredentialSerialisesViaStripeLock(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	ctx := context.Background()
	email := "race+" + freshSuffix() + "@example.com"
	t.Cleanup(func() { _ = us.Delete(ctx, email) })

	err = us.Save(ctx, &User{Email: email, Role: RoleAdmin, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("Save initial: %v", err)
	}

	// Many goroutines each Save a record with a single distinct credential
	// id. The striped lock should serialise per-email writes so no save
	// goes lost; final state should have whatever the last write planted,
	// without any concurrent-map-write panics. We assert no race detector
	// trips and that the final Load returns a usable record.
	var wg sync.WaitGroup
	const n = 16
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			u, err := us.Load(ctx, email)
			if err != nil {
				t.Errorf("load in goroutine %d: %v", i, err)
				return
			}
			cred := webauthn.Credential{ID: []byte{byte(i)}}
			u.PutCredential(cred)
			err = us.Save(ctx, u)
			if err != nil {
				t.Errorf("save in goroutine %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got, err := us.Load(ctx, email)
	if err != nil {
		t.Fatalf("Load after concurrent saves: %v", err)
	}
	if got.Email != email {
		t.Errorf("Email corrupted: %q", got.Email)
	}
	// At least one credential should have made it through — we don't assert
	// the exact count because last-writer-wins is the documented behaviour
	// (the striped lock doesn't merge writes).
	if len(got.Credentials) == 0 {
		t.Errorf("no credentials persisted after concurrent PutCredential")
	}
}

func TestUserStore_CreateFromInviteIdempotent(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	ctx := context.Background()
	email := "create+" + freshSuffix() + "@example.com"
	t.Cleanup(func() { _ = us.Delete(ctx, email) })

	inv := Invite{Email: email, Role: RoleAdmin, Quotas: Quotas{MaxApps: 4}}
	first, err := us.CreateFromInvite(ctx, inv)
	if err != nil {
		t.Fatalf("first CreateFromInvite: %v", err)
	}
	if first.Role != RoleAdmin || first.Quotas.MaxApps != 4 {
		t.Errorf("first user = %+v", first)
	}

	// Second call must return the existing record, not overwrite it.
	second, err := us.CreateFromInvite(ctx, Invite{Email: email, Role: RoleSuperAdmin, Quotas: Quotas{MaxApps: 99}})
	if err != nil {
		t.Fatalf("second CreateFromInvite: %v", err)
	}
	if second.Role != RoleAdmin {
		t.Errorf("second call overwrote Role: %q", second.Role)
	}
	if second.Quotas.MaxApps != 4 {
		t.Errorf("second call overwrote Quotas: %+v", second.Quotas)
	}
}

func TestUserStore_DeleteUnknownIsNoop(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run user store tests")
	}
	us, err := NewUserStore(st)
	if err != nil {
		t.Fatalf("NewUserStore: %v", err)
	}
	// Deleting a never-existed user shouldn't error — Delete is idempotent.
	err = us.Delete(context.Background(), "ghost+"+freshSuffix()+"@example.com")
	if err != nil {
		t.Errorf("Delete on missing user errored: %v", err)
	}
}
