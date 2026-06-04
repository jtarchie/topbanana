package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// substrateMissingPage is valid HTML except it omits the /app.css substrate
// link, so a lint pass reports exactly one deterministically fixable error
// (KindDesignSubstrate). The body markers must survive the relint.
const substrateMissingPage = `<!DOCTYPE html>
<html lang="en" data-theme="cupcake">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Survivor</title>
</head>
<body>
<main class="p-6"><h1>Do not wipe me</h1><p>Original paragraph.</p></main>
</body>
</html>`

// TestRelint_DeterministicAutoFix_SkipsAgent is the regression test for the
// relint data-loss bug: a missing-/app.css error is repaired in-code and the
// request redirects to a clean workspace WITHOUT ever invoking the agent. The
// runner is a failingRunner, so if relint reached the agent the build would
// fail and the handler would redirect to ?building=1 instead — the assertion
// below catches that. The original page content must survive, now carrying the
// /app.css link.
func TestRelint_DeterministicAutoFix_SkipsAgent(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	handler := buildServerWithRunner(t, st, snapSvc, failingRunner{})

	mustWrite(t, ctx, st, slug, "index.html", substrateMissingPage, "text/html; charset=utf-8")

	req := httptest.NewRequest(http.MethodPost, "/relint/"+slug, nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303; body=%q", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "/workspace/"+slug+"?flash=lint-autofixed" {
		t.Fatalf("redirect: got %q, want /workspace/%s?flash=lint-autofixed (a ?building=1 redirect means the agent path was taken)", loc, slug)
	}

	got := mustRead(t, ctx, st, slug, "index.html")
	if !strings.Contains(got, `href="/app.css"`) {
		t.Errorf("relint did not inject the /app.css link:\n%s", got)
	}
	for _, want := range []string{"Do not wipe me", "Original paragraph.", `data-theme="cupcake"`} {
		if !strings.Contains(got, want) {
			t.Errorf("relint wiped original content %q:\n%s", want, got)
		}
	}
}
