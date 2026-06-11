package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestManagePage_CompletenessGuide_PartialSite confirms the deterministic
// "Is my site complete?" card renders the right ✓/✗ tally and guides the owner
// to a missing essential. A restaurant page with only hours + a tap-to-call
// number should score 2 of 5 (menu, location, map missing).
func TestManagePage_CompletenessGuide_PartialSite(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	handler := buildServerWithRunner(t, st, snapSvc, &stubRunner{})
	const index = `<!DOCTYPE html><html><head><title>Joe's</title></head><body>
<h1>Joe's Diner</h1>
<h2>Hours</h2><p>Open Monday through Friday, nine in the morning until nine at night.</p>
<a href="tel:+15551234567">Call us</a>
</body></html>`
	mustWrite(t, ctx, st, slug, "index.html", index, "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "restaurant", OwnerID: testAdminUser})

	body := getManage(t, handler, slug)

	for _, want := range []string{
		"Is my site complete?",
		"2 of 5 essentials",
		"no AI", // the trust line
		"Your menu",
		`href="/workspace/` + slug + `?page=index.html"`, // deep link for a missing item
	} {
		if !strings.Contains(body, want) {
			t.Errorf("manage completeness card missing %q", want)
		}
	}
}

// TestManagePage_CompletenessGuide_HiddenWithoutGuide confirms a template that
// declares no guide (blank) renders the manage page without the card at all,
// rather than an empty "0 of 0" shell.
func TestManagePage_CompletenessGuide_HiddenWithoutGuide(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	handler := buildServerWithRunner(t, st, snapSvc, &stubRunner{})
	mustWrite(t, ctx, st, slug, "index.html", "<h1>hi</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	body := getManage(t, handler, slug)
	if strings.Contains(body, "Is my site complete?") {
		t.Error("blank template (no guide) should not render the completeness card")
	}
}

func getManage(t *testing.T, handler http.Handler, slug string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/manage/"+slug, nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /manage/%s: %d body=%q", slug, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}
