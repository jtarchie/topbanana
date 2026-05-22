package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jtarchie/bloomhollow/internal/auth"
	"github.com/jtarchie/bloomhollow/internal/build"
	"github.com/jtarchie/bloomhollow/internal/events"
	"github.com/jtarchie/bloomhollow/internal/server"
	"github.com/jtarchie/bloomhollow/internal/snapshot"
	"github.com/jtarchie/bloomhollow/internal/state"
	"github.com/jtarchie/bloomhollow/internal/store"
)

// privateTestRig is the test-only counterpart to buildServer that surfaces
// the auth service so the private-gating cases can mint extra sessions for
// a "non-owner" and an "owner" user. The default helper in restore_e2e_test.go
// keeps authSvc internal because every other test only needs the super admin.
type privateTestRig struct {
	handler http.Handler
	auth    *auth.Auth
	store   *store.Store
}

func newPrivateRig(t *testing.T, st *store.Store, snapSvc *snapshot.Service) *privateTestRig {
	t.Helper()
	authSvc, err := auth.New(auth.Config{
		Store:           st,
		Domain:          "localhost",
		SuperAdminEmail: testAdminUser,
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	e, _ := server.New(server.Deps{
		Store:    st,
		Build:    build.New(st, nil, events.NewTracker(), snapSvc),
		Events:   events.NewTracker(),
		State:    state.NewMemory(),
		Snapshot: snapSvc,
		Auth:     authSvc,
		Domain:   "localhost",
		Port:     "8080",
	})
	return &privateTestRig{handler: e, auth: authSvc, store: st}
}

// session mints a passkey session for email and returns the cookie. role
// controls whether the seeded user record carries super-admin powers.
func (r *privateTestRig) session(t *testing.T, email string, role auth.Role) *http.Cookie {
	t.Helper()
	token, err := r.auth.InjectTestSession(context.Background(), email, role)
	if err != nil {
		t.Fatalf("inject session for %s: %v", email, err)
	}
	return &http.Cookie{Name: r.auth.SessionCookieName(), Value: token}
}

// writeMeta drops a SiteMeta sidecar into the bucket directly. Used to seed
// ownership + the Private flag without going through the build flow.
func writeMeta(t *testing.T, ctx context.Context, st *store.Store, slug string, meta build.SiteMeta) {
	t.Helper()
	body, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	err = st.Write(ctx, slug, build.MetaFile, string(body), "application/json", nil)
	if err != nil {
		t.Fatalf("write meta: %v", err)
	}
}

// TestPrivateGating_Subdomain covers the dispatchSite gate end-to-end:
// an unauthenticated, non-owner, owner, and super admin request each hit
// the same private slug and see the expected status.
func TestPrivateGating_Subdomain(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "priv-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	// Seed the site files first; the rig's startup rebuild walks the bucket
	// and needs the meta to be there before it reads the privateIndex.
	mustWrite(t, ctx, st, slug, "index.html", "<h1>private hello</h1>", "text/html")
	const ownerEmail = "owner@test"
	const strangerEmail = "stranger@test"
	writeMeta(t, ctx, st, slug, build.SiteMeta{
		Template: "blank",
		OwnerID:  ownerEmail,
		Private:  true,
	})

	rig := newPrivateRig(t, st, snapSvc)
	httpSrv := httptest.NewServer(rig.handler)
	t.Cleanup(httpSrv.Close)

	superCookie := rig.session(t, testAdminUser, auth.RoleSuperAdmin)
	ownerCookie := rig.session(t, ownerEmail, auth.RoleAdmin)
	strangerCookie := rig.session(t, strangerEmail, auth.RoleAdmin)

	host := slug + ".localhost"
	client := &http.Client{}

	cases := []struct {
		name   string
		cookie *http.Cookie
		want   int
	}{
		{"no cookie", nil, http.StatusNotFound},
		{"stranger", strangerCookie, http.StatusNotFound},
		{"owner", ownerCookie, http.StatusOK},
		{"super admin", superCookie, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/", nil)
			if err != nil {
				t.Fatalf("new GET: %v", err)
			}
			req.Host = host
			if tc.cookie != nil {
				req.AddCookie(tc.cookie)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("GET: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status: got %d want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

// TestPrivateToggle_SettingsRoundTrip drives POST /settings/:slug with
// `private=on` and confirms ReadMeta reflects the flag, plus that toggling
// it off un-gates the subdomain.
func TestPrivateToggle_SettingsRoundTrip(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "priv-rt-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", "<h1>toggle me</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{
		Template: "blank",
		OwnerID:  testAdminUser,
	})

	// Use the standard buildServer so we inherit testSessionCookie wiring.
	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)
	client := &http.Client{}

	// Public before toggling: subdomain serves with no cookie.
	{
		req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/", nil)
		req.Host = slug + ".localhost"
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("pre-toggle GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("pre-toggle status: got %d want 200", resp.StatusCode)
		}
	}

	// POST settings with private=on. The form expects the same fields the
	// HTML page emits; everything except `private` carries the existing meta.
	postSettings := func(privateOn bool) {
		t.Helper()
		form := url.Values{"domains": {""}}
		if privateOn {
			form.Set("private", "on")
		}
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/settings/"+slug, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("new POST settings: %v", err)
		}
		req.Host = "localhost"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(testSessionCookie)
		// Don't follow the 303 — we just want the response code.
		noRedirect := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatalf("POST settings: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("settings status: got %d want 303", resp.StatusCode)
		}
	}

	postSettings(true)

	// Meta reflects the flag.
	buildSvc := build.New(st, nil, events.NewTracker(), snapSvc)
	got := buildSvc.ReadMeta(ctx, slug)
	if !got.Private {
		t.Fatalf("meta.Private after toggle on: got false want true")
	}

	// Subdomain now 404s for an unauthenticated request.
	{
		req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/", nil)
		req.Host = slug + ".localhost"
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("private GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("private status: got %d want 404", resp.StatusCode)
		}
	}

	// Toggling off re-opens the site.
	postSettings(false)
	got = buildSvc.ReadMeta(ctx, slug)
	if got.Private {
		t.Fatalf("meta.Private after toggle off: got true want false")
	}
	{
		req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/", nil)
		req.Host = slug + ".localhost"
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("post-untoggle GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("post-untoggle status: got %d want 200", resp.StatusCode)
		}
	}
}
