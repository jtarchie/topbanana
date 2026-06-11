package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestAPIHandler_NativeFormPOSTBodyReachesHandler is the regression for the
// production bug where a native HTML `<form method=post>` to /api/* arrived at
// the handler with an empty request.form.
//
// Root cause: methodOverrideMiddleware runs Pre (before routing) and, for any
// urlencoded POST, calls r.PostFormValue("_method") to look for a `_method`
// override. PostFormValue triggers Go's ParseForm, which reads r.Body to EOF
// and never restores it. By the time apiHandler -> buildSandboxRequest did
// io.ReadAll(r.Body), the body was already drained, so req.Form was empty and
// every field validated as "required".
//
// Symptom in production (fast-flame-71 / topbanana.dev): the invite form POSTed
// email=... and got back {"errors":[{"field":"email","message":"required"}]}.
// The existing api_gate test only asserted status 200, so it never caught that
// the field itself was missing — this test asserts the value round-trips.
func TestAPIHandler_NativeFormPOSTBodyReachesHandler(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := "apiform-" + freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	// Echo the field back so we can prove request.form actually carried it,
	// not just that the handler ran.
	const submitJS = `module.exports = function (request) {
		return response.json({ email: request.form.email || "" });
	};`
	mustWrite(t, ctx, st, slug, "functions/submit.js", submitJS, "text/javascript")
	writeMeta(t, ctx, st, slug, build.SiteMeta{
		OwnerID:          testAdminUser,
		EnablesFunctions: true,
	})

	handler := buildServerWithSandbox(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Exactly what a browser sends for <form method=post> with no enctype:
	// urlencoded body, no _method field.
	body := strings.NewReader("email=test%40example.com")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/submit", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = slug + ".localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	// The handler must see request.form.email. If the body was drained by the
	// method-override middleware this is `{"email":""}`.
	if !strings.Contains(string(got), `"email":"test@example.com"`) {
		t.Fatalf("handler saw empty form — native form POST body was consumed before apiHandler; got %s", got)
	}
}

// TestMethodOverride_StillWorksForAPIRoutes guards the other half: the
// _method override must keep working for urlencoded POSTs even after we stop
// draining the body, so DELETE-via-form routes don't silently regress while we
// fix the read-back. It rides the same /api/* path: a POST carrying
// _method=DELETE should reach the handler seeing request.method == "DELETE"
// AND still expose the rest of the form.
func TestMethodOverride_StillWorksForAPIRoutes(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := "apiform-mo-" + freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	const submitJS = `module.exports = function (request) {
		return response.json({ method: request.method, email: request.form.email || "" });
	};`
	mustWrite(t, ctx, st, slug, "functions/submit.js", submitJS, "text/javascript")
	writeMeta(t, ctx, st, slug, build.SiteMeta{
		OwnerID:          testAdminUser,
		EnablesFunctions: true,
	})

	handler := buildServerWithSandbox(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	body := strings.NewReader("_method=DELETE&email=test%40example.com")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/submit", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = slug + ".localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(got), `"method":"DELETE"`) {
		t.Fatalf("_method override not applied; got %s", got)
	}
	if !strings.Contains(string(got), `"email":"test@example.com"`) {
		t.Fatalf("form field lost after override; got %s", got)
	}
}
