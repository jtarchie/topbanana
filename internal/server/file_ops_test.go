package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// postFileOps wraps the boilerplate for hitting the file_ops endpoints with a
// form body and the test admin's session cookie, without following the 303
// redirect so the caller can assert on the Location header.
func postFileOps(t *testing.T, srvURL, path string, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new POST %s: %v", path, err)
	}
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(testSessionCookie)
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// TestDeleteFileHandler exercises the happy path (HTML page goes away),
// confirm-mismatch belt-and-suspenders, and reserved-path rejection on
// a single live Minio bucket.
//

func TestDeleteFileHandler(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "del-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", "<h1>home</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "about.html", "<h1>about</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "_state/data.json", `{}`, "application/json")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	handler := buildServer(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Confirm mismatch: 400, file survives.
	resp := postFileOps(t, srv.URL, "/files/"+slug, url.Values{
		"_method": {"DELETE"},
		"path":    {"about.html"},
		"confirm": {"oops.html"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("confirm mismatch: status %d want 400", resp.StatusCode)
	}
	if mustRead(t, ctx, st, slug, "about.html") == "" {
		t.Errorf("about.html removed despite confirm mismatch")
	}

	// Reserved path: 400.
	resp = postFileOps(t, srv.URL, "/files/"+slug, url.Values{
		"_method": {"DELETE"},
		"path":    {"_state/data.json"},
		"confirm": {"_state/data.json"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("reserved path: status %d want 400", resp.StatusCode)
	}
	if mustRead(t, ctx, st, slug, "_state/data.json") == "" {
		t.Errorf("_state/data.json was deleted despite reserved-path guard")
	}

	// Happy path: about.html is removed and the redirect lands on /files.
	resp = postFileOps(t, srv.URL, "/files/"+slug, url.Values{
		"_method": {"DELETE"},
		"path":    {"about.html"},
		"confirm": {"about.html"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("happy path: status %d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/files/"+slug) {
		t.Errorf("redirect: got %q want /files/%s…", loc, slug)
	}
	if mustRead(t, ctx, st, slug, "about.html") != "" {
		t.Errorf("about.html still present after delete")
	}
}

// TestRenameFileHandler covers HTML rename (happy path + cross-kind reject +
// destination-exists reject) plus a function rename via the `to_name` form
// field used by the function editor.
//

func TestRenameFileHandler(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "ren-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	const aboutHTML = "<h1>about</h1>"
	mustWrite(t, ctx, st, slug, "index.html", "<h1>home</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "about.html", aboutHTML, "text/html")
	mustWrite(t, ctx, st, slug, "contact.html", "<h1>contact</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "functions/submit.js", "export default () => new Response('ok')", "text/javascript")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser, EnablesFunctions: true})

	handler := buildServer(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Cross-kind: 400, no movement.
	resp := postFileOps(t, srv.URL, "/files/"+slug, url.Values{
		"_method": {"PATCH"},
		"from":    {"about.html"},
		"to":      {"functions/about.js"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("cross-kind: status %d want 400", resp.StatusCode)
	}
	if mustRead(t, ctx, st, slug, "about.html") != aboutHTML {
		t.Errorf("about.html lost after rejected cross-kind rename")
	}

	// Destination exists: 400.
	resp = postFileOps(t, srv.URL, "/files/"+slug, url.Values{
		"_method": {"PATCH"},
		"from":    {"about.html"},
		"to":      {"contact.html"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("dest exists: status %d want 400", resp.StatusCode)
	}
	if mustRead(t, ctx, st, slug, "about.html") != aboutHTML {
		t.Errorf("about.html disappeared after rejected dest-exists rename")
	}

	// Happy path: about.html → info.html. Content moves, original is gone,
	// redirect lands on the workspace editor for the new path.
	resp = postFileOps(t, srv.URL, "/files/"+slug, url.Values{
		"_method": {"PATCH"},
		"from":    {"about.html"},
		"to":      {"info.html"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("html rename: status %d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/workspace/"+slug) || !strings.Contains(loc, "page=info.html") {
		t.Errorf("html rename redirect: got %q want /workspace/%s?page=info.html…", loc, slug)
	}
	if mustRead(t, ctx, st, slug, "about.html") != "" {
		t.Errorf("about.html still present after rename")
	}
	if got := mustRead(t, ctx, st, slug, "info.html"); got != aboutHTML {
		t.Errorf("info.html content: got %q want %q", got, aboutHTML)
	}

	// Function rename via to_name. The form field on function_edit.html
	// only carries the bare name; the handler rebuilds the path.
	resp = postFileOps(t, srv.URL, "/files/"+slug, url.Values{
		"_method": {"PATCH"},
		"from":    {"functions/submit.js"},
		"to_name": {"intake"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("function rename: status %d want 303", resp.StatusCode)
	}
	loc = resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/edit/"+slug+"/function/intake") {
		t.Errorf("function rename redirect: got %q want /edit/%s/function/intake…", loc, slug)
	}
	if mustRead(t, ctx, st, slug, "functions/submit.js") != "" {
		t.Errorf("functions/submit.js still present after rename")
	}
	if mustRead(t, ctx, st, slug, "functions/intake.js") == "" {
		t.Errorf("functions/intake.js missing after rename")
	}
}

// TestFileOpsRejectsNonOwner sanity-checks that requireSlugOwnership is wired
// to both routes — without the session cookie, the POSTs never reach the
// handlers (login redirect, not 400 or 200).
func TestFileOpsRejectsNonOwner(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "guard-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	mustWrite(t, ctx, st, slug, "index.html", "<h1>guarded</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	handler := buildServer(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	noCookie := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, verb := range []string{http.MethodDelete, http.MethodPatch} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/files/"+slug, strings.NewReader(""))
		req.Host = "localhost"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-HTTP-Method-Override", verb)
		resp, err := noCookie.Do(req)
		if err != nil {
			t.Fatalf("%s /files/%s: %v", verb, slug, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
			t.Errorf("%s /files/%s without cookie: status %d loc %q want 303 /login",
				verb, slug, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if mustRead(t, ctx, st, slug, "index.html") == "" {
		t.Errorf("index.html removed even though POST was unauthenticated")
	}
}
