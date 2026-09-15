package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/linkcheck"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/store"
)

// seedLinkAnswer mirrors linkcheck's cache key so the test stays offline; fresh answers mean no probe and no background refresh.
func seedLinkAnswer(t *testing.T, st *store.Store, r linkcheck.Result) {
	t.Helper()
	r.CheckedAt = time.Now()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(r.URL))
	err = st.WriteRaw(context.Background(), store.LinkCheckPrefix+hex.EncodeToString(sum[:])+".json", string(b), "application/json", nil)
	if err != nil {
		t.Fatalf("seed link answer: %v", err)
	}
}

func TestManagePage_LinksCard(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	dead := "https://invented-" + slug + ".example/menu"
	ok := "https://ok-" + slug + ".example/"
	mustWrite(t, ctx, st, slug, "index.html", `<a href="`+dead+`">menu</a><a href="`+ok+`">ok</a>`, "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})
	seedLinkAnswer(t, st, linkcheck.Result{URL: dead, Status: linkcheck.StatusDead, Reason: "the domain does not exist"})
	seedLinkAnswer(t, st, linkcheck.Result{URL: ok, Status: linkcheck.StatusOK})

	withChecker := func(d *server.Deps) { d.LinkChecker = linkcheck.New(st) }
	handler := buildServerWithRunnerAndInfo(t, st, snapSvc, &stubRunner{}, server.SystemInfo{}, withChecker)

	body := getManage(t, handler, slug)
	for _, want := range []string{"Links to other sites", "1 of 2 work", "Broken", dead, "the domain does not exist", "On index.html"} {
		if !strings.Contains(body, want) {
			t.Errorf("links card missing %q", want)
		}
	}
	if strings.Contains(body, "being refreshed") {
		t.Error("fresh answers must not report a refresh in progress")
	}

	req := httptest.NewRequest(http.MethodPost, "/manage/"+slug+"/links", nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/manage/"+slug+"?flash=") {
		t.Fatalf("POST links: %d %q", rec.Code, rec.Header().Get("Location"))
	}

	// No checker configured: the card is absent, not an empty shell.
	plain := getManage(t, buildServerWithRunner(t, st, snapSvc, &stubRunner{}), slug)
	if strings.Contains(plain, "Links to other sites") {
		t.Error("links card must be hidden when the server runs no checker")
	}
}
