package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// cancellableBlobs fails a read whose caller has gone away, as S3 does;
// blob.Memory ignores the context.
type cancellableBlobs struct{ blob.Blobs }

func (b cancellableBlobs) Get(ctx context.Context, key string) (blob.Object, error) {
	err := ctx.Err()
	if err != nil {
		return blob.Object{}, err //nolint:wrapcheck // the caller must see context.Canceled itself
	}
	return b.Blobs.Get(ctx, key) //nolint:wrapcheck // transparent wrapper
}

// TestRequireUser_LookupFaultKeepsSession: requireUser logged out on *any*
// user-lookup error, and logging out deletes the session record. So a request
// whose client disconnected while the user cache was cold (every 60s) cost a
// live session — found in knowhere, where a polling page aborted its own
// requests. Only "no such account" and "disabled" are verdicts.
func TestRequireUser_LookupFaultKeepsSession(t *testing.T) {
	st := minioStore(t)
	snapSvc := snapshot.New(st, 0)
	blobs := cancellableBlobs{blob.NewMemory()}

	warm := newPrivateRigOver(t, st, snapSvc, blobs)
	// Not the super admin: bootstrap reads that record, which would warm the
	// cache this needs cold.
	cookie := warm.session(t, "regular-fault@test", auth.RoleAdmin)

	cold := newPrivateRigOver(t, st, snapSvc, blobs)

	get := func(ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/users", nil).WithContext(ctx)
		req.Host = "localhost"
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		cold.handler.ServeHTTP(rec, req)
		return rec
	}

	gone, cancel := context.WithCancel(context.Background())
	cancel()
	rec := get(gone)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("lookup fault: got %d want 503", rec.Code)
	}
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Errorf("lookup fault cleared the cookie: %q", got)
	}

	// 404 is a signed-in user who is not a super admin; a lost session is a
	// 303 to /login.
	if rec := get(context.Background()); rec.Code != http.StatusNotFound {
		t.Errorf("after the fault: got %d want 404 (session should have survived)", rec.Code)
	}
}
