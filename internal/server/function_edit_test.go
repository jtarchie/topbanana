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

// The source pane ships pre-tokenised HTML plus its palette; html/template
// would reject either one if the types were wrong, so rendering the page at
// all is most of the assertion.
func TestFunctionEditPageHighlightsSource(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "fn-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", "<h1>home</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "functions/submit.js",
		"export default async function (req) {\n  return new Response('ok');\n}\n",
		"application/javascript")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	srv := httptest.NewServer(buildServer(t, st, snapSvc))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/edit/"+slug+"/function/submit", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET function edit: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	page := string(body)
	if !strings.Contains(page, `<pre tabindex="0" class="chroma">`) &&
		!strings.Contains(page, `class="chroma"`) {
		t.Errorf("source pane not highlighted")
	}
	if !strings.Contains(page, ".chroma .k") {
		t.Errorf("highlight palette missing from page")
	}
	if !strings.Contains(page, "Response") {
		t.Errorf("source text missing from page")
	}
}
