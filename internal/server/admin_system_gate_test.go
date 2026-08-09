package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestAdminSystem_RequiresSuperAdmin locks the gate on the instance dashboard.
// It used to hang off the plain requireUser group while listing every app on
// the instance (slugs, titles, sizes, edit counts, build transcripts) plus the
// server's S3 bucket and LLM configuration — so any signed-in hobbyist could
// enumerate every other tenant. requireSuperAdmin answers 404 rather than 403
// so the route's existence stays hidden, matching the other operator routes.
func TestAdminSystem_RequiresSuperAdmin(t *testing.T) {
	st := minioStore(t)
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	regular := rig.session(t, "regular-sys@test", auth.RoleAdmin)
	super := rig.session(t, testAdminUser, auth.RoleSuperAdmin)

	for _, path := range []string{"/admin/system", "/admin/users", "/admin/clients"} {
		status, _ := authedGetStatus(t, srv.URL, path, regular)
		if status != http.StatusNotFound {
			t.Errorf("GET %s as a regular admin: got %d want 404", path, status)
		}
		status, _ = authedGetStatus(t, srv.URL, path, super)
		if status != http.StatusOK {
			t.Errorf("GET %s as a super admin: got %d want 200", path, status)
		}
	}

	// The pre-split bookmark carries the same gate, and only ever redirects.
	if status, _ := authedGetStatus(t, srv.URL, "/system", regular); status != http.StatusNotFound {
		t.Errorf("GET /system as a regular admin: got %d want 404", status)
	}
	if status, loc := authedGetStatus(t, srv.URL, "/system", super); status != http.StatusMovedPermanently || loc != "/admin/system" {
		t.Errorf("GET /system as a super admin: got %d %q want 301 /admin/system", status, loc)
	}
}
