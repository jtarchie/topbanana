package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/photowall"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
)

// newPWServer wires a Server for the photo-wall tests. The store must already
// carry the site's meta (so the registry rebuild in server.New sees the slug),
// and the caller keeps the state.Store handle so tests can inspect or pre-seed
// KV rows. Returns the handler plus a super-admin session cookie for the
// owner-gated moderation routes.
func newPWServer(t *testing.T, st *store.Store, snapSvc *snapshot.Service, stateStore state.Store) (http.Handler, *http.Cookie) {
	t.Helper()
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	buildSvc := build.New(st, nil, tracker, snapSvc)
	authSvc, err := auth.New(auth.Config{
		Store:           st,
		Domain:          "localhost",
		SuperAdminEmail: testAdminUser,
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = authSvc.Close() })
	token, err := authSvc.InjectTestSession(context.Background(), testAdminUser, auth.RoleSuperAdmin)
	if err != nil {
		t.Fatalf("inject test session: %v", err)
	}
	e, _ := server.New(server.Deps{
		Store:    st,
		Build:    buildSvc,
		Events:   tracker,
		State:    stateStore,
		Snapshot: snapSvc,
		Auth:     authSvc,
		Domain:   "localhost",
		Port:     "8080",
	})
	return e, &http.Cookie{Name: authSvc.SessionCookieName(), Value: token}
}

// seedPhotoWallSite writes the meta + a home page for a photo-wall site so the
// registry sees the slug and photoWallEnabled resolves true.
func seedPhotoWallSite(t *testing.T, ctx context.Context, st *store.Store, slug, tmpl string) {
	t.Helper()
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: testAdminUser, Template: tmpl})
	mustWrite(t, ctx, st, slug, "index.html", "<!DOCTYPE html><html><body><h1>wall</h1></body></html>", "text/html")
}

func photoUploadReq(t *testing.T, slug, field, filename string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = fw.Write(content)
	err = mw.Close()
	if err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/_photos", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Host = slug + ".localhost"
	return req
}

func firstPending(t *testing.T, ctx context.Context, stateStore state.Store, slug string) photowall.Photo {
	t.Helper()
	snap, err := stateStore.Load(ctx, slug)
	if err != nil {
		t.Fatalf("state load: %v", err)
	}
	pending := photowall.Collect(snap.Data, photowall.StatusPending)
	if len(pending) == 0 {
		t.Fatalf("no pending photo in state")
	}
	return pending[0]
}

// TestPhotoWall_UploadApproveServeFlow is the end-to-end happy path: a visitor
// uploads, the photo is held pending and unservable, the owner approves it, and
// only then is it listed and served publicly.
func TestPhotoWall_UploadApproveServeFlow(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	stateStore := state.NewMemory()
	slug := "pw-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	seedPhotoWallSite(t, ctx, st, slug, "photowall")

	handler, cookie := newPWServer(t, st, snapSvc, stateStore)

	// Upload.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, photoUploadReq(t, slug, "photo", "party.png", mustTinyPNG(t)))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("upload status = %d want 303; body=%q", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/?submitted=1" {
		t.Errorf("upload redirect = %q want /?submitted=1", loc)
	}

	photo := firstPending(t, ctx, stateStore, slug)
	id, ext := photo.ID, photo.Ext

	// Pending bytes exist in the reserved prefix but are NOT publicly servable.
	if got := mustRead(t, ctx, st, slug, photowall.PendingPath(id, ext)); got == "" {
		t.Fatalf("pending bytes missing at %s", photowall.PendingPath(id, ext))
	}
	pendReq := httptest.NewRequest(http.MethodGet, "/"+photowall.PendingPath(id, ext), nil)
	pendReq.Host = slug + ".localhost"
	pendRec := httptest.NewRecorder()
	handler.ServeHTTP(pendRec, pendReq)
	if pendRec.Code != http.StatusNotFound {
		t.Errorf("GET pending bytes = %d want 404 (must not be servable)", pendRec.Code)
	}

	// Nothing approved yet.
	if items := getApproved(t, handler, slug); len(items) != 0 {
		t.Fatalf("approved list = %d want 0 before approval", len(items))
	}

	// Approve.
	form := url.Values{"id": {id}, "confirm": {id}}
	apReq := httptest.NewRequest(http.MethodPost, "/photos/"+slug+"/approve", strings.NewReader(form.Encode()))
	apReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	apReq.Header.Set("Accept", "application/json")
	apReq.Host = "localhost"
	apReq.AddCookie(cookie)
	apRec := httptest.NewRecorder()
	handler.ServeHTTP(apRec, apReq)
	if apRec.Code != http.StatusOK {
		t.Fatalf("approve status = %d want 200; body=%q", apRec.Code, apRec.Body.String())
	}

	// Pending bytes gone, approved bytes present + publicly servable.
	if got := mustRead(t, ctx, st, slug, photowall.PendingPath(id, ext)); got != "" {
		t.Errorf("pending bytes should be deleted after approval")
	}
	approvedPath := photowall.ApprovedPath(id, ext)
	if got := mustRead(t, ctx, st, slug, approvedPath); got == "" {
		t.Fatalf("approved bytes missing at %s", approvedPath)
	}
	serveReq := httptest.NewRequest(http.MethodGet, "/"+approvedPath, nil)
	serveReq.Host = slug + ".localhost"
	serveRec := httptest.NewRecorder()
	handler.ServeHTTP(serveRec, serveReq)
	if serveRec.Code != http.StatusOK {
		t.Fatalf("GET approved bytes = %d want 200", serveRec.Code)
	}
	if ct := serveRec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("approved bytes content-type = %q want image/png", ct)
	}

	// Approved list now carries the photo.
	items := getApproved(t, handler, slug)
	if len(items) != 1 {
		t.Fatalf("approved list = %d want 1 after approval", len(items))
	}
	if items[0].URL != "/"+approvedPath {
		t.Errorf("approved url = %q want /%s", items[0].URL, approvedPath)
	}
}

// TestPhotoWall_QueueAndManageRender exercises the owner-facing HTML surfaces:
// the moderation queue page (with a pending photo's preview) and the manage
// page's photo-wall card. Locks in that both templates render without error and
// surface the queue.
func TestPhotoWall_QueueAndManageRender(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	stateStore := state.NewMemory()
	slug := "pw-ui-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	seedPhotoWallSite(t, ctx, st, slug, "photowall")

	handler, cookie := newPWServer(t, st, snapSvc, stateStore)

	// One pending upload to populate the queue.
	up := httptest.NewRecorder()
	handler.ServeHTTP(up, photoUploadReq(t, slug, "photo", "p.png", mustTinyPNG(t)))
	if up.Code != http.StatusSeeOther {
		t.Fatalf("upload failed: %d", up.Code)
	}
	id := firstPending(t, ctx, stateStore, slug).ID

	// Queue page renders and shows the pending photo's preview route.
	qReq := httptest.NewRequest(http.MethodGet, "/photos/"+slug, nil)
	qReq.Host = "localhost"
	qReq.AddCookie(cookie)
	qRec := httptest.NewRecorder()
	handler.ServeHTTP(qRec, qReq)
	if qRec.Code != http.StatusOK {
		t.Fatalf("GET /photos/%s = %d want 200; body=%q", slug, qRec.Code, qRec.Body.String())
	}
	if body := qRec.Body.String(); !strings.Contains(body, "/photos/"+slug+"/pending/"+id) {
		t.Errorf("queue page missing preview link for pending photo %s", id)
	}

	// Manage page renders the photo-wall card with the pending count + queue link.
	mReq := httptest.NewRequest(http.MethodGet, "/manage/"+slug, nil)
	mReq.Host = "localhost"
	mReq.AddCookie(cookie)
	mRec := httptest.NewRecorder()
	handler.ServeHTTP(mRec, mReq)
	if mRec.Code != http.StatusOK {
		t.Fatalf("GET /manage/%s = %d want 200", slug, mRec.Code)
	}
	for _, want := range []string{"Photo wall", "/photos/" + slug} {
		if !strings.Contains(mRec.Body.String(), want) {
			t.Errorf("manage page missing %q", want)
		}
	}
}

type approvedItem struct {
	URL string `json:"url"`
	TS  int64  `json:"ts"`
}

func getApproved(t *testing.T, handler http.Handler, slug string) []approvedItem {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/_photos/approved", nil)
	req.Host = slug + ".localhost"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_photos/approved = %d want 200", rec.Code)
	}
	var items []approvedItem
	err := json.Unmarshal(rec.Body.Bytes(), &items)
	if err != nil {
		t.Fatalf("decode approved list: %v (body=%q)", err, rec.Body.String())
	}
	return items
}

// TestPhotoWall_QRCode: the display page's QR endpoint returns a self-contained
// SVG on a photo-wall site and 404s on any other site.
func TestPhotoWall_QRCode(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	stateStore := state.NewMemory()
	slug := "pw-qr-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	seedPhotoWallSite(t, ctx, st, slug, "photowall")

	handler, _ := newPWServer(t, st, snapSvc, stateStore)

	req := httptest.NewRequest(http.MethodGet, "/_photos/qr", nil)
	req.Host = slug + ".localhost"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /_photos/qr = %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Errorf("qr content-type = %q want image/svg+xml", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "</svg>") {
		t.Errorf("qr body is not SVG: %q", body[:min(len(body), 80)])
	}

	// Gated off on a non-wall site.
	offSlug := "pw-qroff-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, offSlug)
	seedPhotoWallSite(t, ctx, st, offSlug, "blank")
	// Rebuild registry to include the new slug by making a fresh server.
	offHandler, _ := newPWServer(t, st, snapSvc, stateStore)
	offReq := httptest.NewRequest(http.MethodGet, "/_photos/qr", nil)
	offReq.Host = offSlug + ".localhost"
	offRec := httptest.NewRecorder()
	offHandler.ServeHTTP(offRec, offReq)
	if offRec.Code != http.StatusNotFound {
		t.Errorf("GET /_photos/qr on non-wall site = %d want 404", offRec.Code)
	}
}

// TestPhotoWall_RejectsNonImage: a non-image upload is refused with 415 and
// leaves no state row.
func TestPhotoWall_RejectsNonImage(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	stateStore := state.NewMemory()
	slug := "pw-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	seedPhotoWallSite(t, ctx, st, slug, "photowall")

	handler, _ := newPWServer(t, st, snapSvc, stateStore)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, photoUploadReq(t, slug, "photo", "notes.txt", []byte("this is plainly text, not an image")))
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("non-image upload = %d want 415", rec.Code)
	}
	snap, _ := stateStore.Load(ctx, slug)
	if n := photowall.CountPending(snap.Data); n != 0 {
		t.Errorf("rejected upload left %d pending rows, want 0", n)
	}
}

// TestPhotoWall_GatingOff: a site whose template does not enable the wall 404s
// on both reserved endpoints.
func TestPhotoWall_GatingOff(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	stateStore := state.NewMemory()
	slug := "pw-off-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	seedPhotoWallSite(t, ctx, st, slug, "blank") // blank template: no photo wall

	handler, _ := newPWServer(t, st, snapSvc, stateStore)

	up := httptest.NewRecorder()
	handler.ServeHTTP(up, photoUploadReq(t, slug, "photo", "x.png", mustTinyPNG(t)))
	if up.Code != http.StatusNotFound {
		t.Errorf("POST /_photos on non-wall site = %d want 404", up.Code)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/_photos/approved", nil)
	listReq.Host = slug + ".localhost"
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, listReq)
	if listRec.Code != http.StatusNotFound {
		t.Errorf("GET /_photos/approved on non-wall site = %d want 404", listRec.Code)
	}
}

// TestPhotoWall_RateLimited: rapid uploads from one client exhaust the burst
// and start returning 429.
func TestPhotoWall_RateLimited(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	stateStore := state.NewMemory()
	slug := "pw-rl-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	seedPhotoWallSite(t, ctx, st, slug, "photowall")

	handler, _ := newPWServer(t, st, snapSvc, stateStore)

	var accepted, limited int
	for i := range 10 {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, photoUploadReq(t, slug, "photo", "p.png", mustTinyPNG(t)))
		switch rec.Code {
		case http.StatusSeeOther:
			accepted++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("upload %d unexpected status %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	if limited == 0 {
		t.Errorf("expected some uploads to be rate-limited (429); accepted=%d limited=%d", accepted, limited)
	}
	if accepted == 0 {
		t.Errorf("expected the initial burst to be accepted; accepted=%d limited=%d", accepted, limited)
	}
}

// TestPhotoWall_PendingCapRefuses: with the queue already full, a new upload is
// refused with 429 rather than piling on.
func TestPhotoWall_PendingCapRefuses(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	stateStore := state.NewMemory()
	slug := "pw-cap-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	seedPhotoWallSite(t, ctx, st, slug, "photowall")

	// Pre-seed the queue to the cap.
	snap, err := stateStore.Load(ctx, slug)
	if err != nil {
		t.Fatalf("state load: %v", err)
	}
	if snap.Data == nil {
		snap.Data = map[string]any{}
	}
	for i := 1; i <= photowall.DefaultPendingCap; i++ {
		id := photowall.FormatID(int64(i))
		snap.Data[photowall.MetaKey(id)] = photowall.Photo{ID: id, Status: photowall.StatusPending, Ext: ".jpg", TS: int64(i)}.ToMeta()
	}
	err = stateStore.Save(ctx, slug, snap)
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}

	handler, _ := newPWServer(t, st, snapSvc, stateStore)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, photoUploadReq(t, slug, "photo", "one-more.png", mustTinyPNG(t)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("upload past pending cap = %d want 429", rec.Code)
	}
}

// TestPhotoWall_ApproveConfirmMismatch: a mismatched confirm token is refused,
// mirroring the submission-delete guard.
func TestPhotoWall_ApproveConfirmMismatch(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	stateStore := state.NewMemory()
	slug := "pw-cm-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	seedPhotoWallSite(t, ctx, st, slug, "photowall")

	handler, cookie := newPWServer(t, st, snapSvc, stateStore)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, photoUploadReq(t, slug, "photo", "p.png", mustTinyPNG(t)))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("upload failed: %d", rec.Code)
	}
	id := firstPending(t, ctx, stateStore, slug).ID

	form := url.Values{"id": {id}, "confirm": {"wrong"}}
	req := httptest.NewRequest(http.MethodPost, "/photos/"+slug+"/approve", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Host = "localhost"
	req.AddCookie(cookie)
	apRec := httptest.NewRecorder()
	handler.ServeHTTP(apRec, req)
	if apRec.Code != http.StatusBadRequest {
		t.Fatalf("approve with mismatched confirm = %d want 400", apRec.Code)
	}
}
