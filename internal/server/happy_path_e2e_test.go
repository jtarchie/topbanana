package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
)

// stubRunner is a deterministic agent.Runner used by the happy-path test. It
// synthesises a tiny DaisyUI-enabled index.html so the lint pass that fires
// after every Run() doesn't reject the build for missing tags, and it emits
// the SSE events a real run would emit so the progress page's friendly-event
// translation gets exercised end-to-end.
type stubRunner struct {
	title, desc string
}

const stubIndexHTML = `<!DOCTYPE html>
<html lang="en" data-theme="cupcake">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Hello from the stub agent</title>
<link rel="stylesheet" href="/app.css">
</head>
<body>
<main class="p-6"><h1 class="text-3xl">Hello, world.</h1></main>
</body>
</html>
`

func (r *stubRunner) Run(ctx context.Context, s *store.Store, req build.RunRequest, emit func(events.Event), _ *events.Tracker) (agent.Usage, error) {
	now := time.Now().UTC()
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseStart, Path: "/index.html", Time: now})
	err := s.Write(ctx, req.Slug, "index.html", stubIndexHTML, "text/html; charset=utf-8", nil)
	if err != nil {
		emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseError, Path: "/index.html", Message: err.Error(), Time: time.Now().UTC()})
		return agent.Usage{}, fmt.Errorf("stub write: %w", err)
	}
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseDone, Path: "/index.html", Time: time.Now().UTC()})
	return agent.Usage{}, nil
}

func (r *stubRunner) Describe(_ context.Context, _ *store.Store, _ string, _ string) (agent.SiteDescription, error) {
	return agent.SiteDescription{Title: r.title, Description: r.desc}, nil
}

// testSessionCookie is filled in by buildServerWithRunnerAndInfo and read
// back by every authenticated test request. Package-scoped because each
// test rebuilds the server fresh and gets its own cookie value, and
// threading it through every helper would be more code than the global
// is worth in test scope.
var testSessionCookie *http.Cookie

// buildServerWithRunner is the happy-path counterpart to buildServer: same
// auth + deps but threads a Runner all the way through the build service so
// the test can drive a deterministic build without a real LLM.
func buildServerWithRunner(t *testing.T, st *store.Store, snapSvc *snapshot.Service, runner build.Runner) http.Handler {
	return buildServerWithRunnerAndInfo(t, st, snapSvc, runner, server.SystemInfo{})
}

// buildServerWithRunnerAndInfo is buildServerWithRunner plus a SystemInfo
// override. The system dashboard test uses it to plant a known model string
// so it can assert /system surfaces config it didn't make up.
func buildServerWithRunnerAndInfo(t *testing.T, st *store.Store, snapSvc *snapshot.Service, runner build.Runner, info server.SystemInfo) http.Handler {
	t.Helper()
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	buildSvc := build.NewWithConfig(build.Config{
		Store:      st,
		Events:     tracker,
		Snapshot:   snapSvc,
		Runner:     runner,
		RecordEdit: true,
	})
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
	testSessionCookie = &http.Cookie{Name: authSvc.SessionCookieName(), Value: token}
	e, _ := server.New(server.Deps{
		Store:      st,
		Build:      buildSvc,
		Events:     tracker,
		State:      state.NewMemory(),
		Snapshot:   snapSvc,
		Auth:       authSvc,
		Domain:     "localhost",
		Port:       "8080",
		SystemInfo: info,
	})
	return e
}

// TestHappyPath_EndToEnd drives a fresh user through the redesigned UI flow:
// land on /, submit a build, watch the SSE event stream complete, see the
// site listed on /apps, fetch the live site through the subdomain proxy,
// open the edit page, then delete the app and confirm it vanishes from
// /apps. The point is to catch the kinds of regressions a template-rewrite
// can quietly introduce — broken handlers, missing partials, wrong field
// names, dead links — without requiring a real LLM.
//
//nolint:gocognit,cyclop // single end-to-end script intentionally walks many steps.
func TestHappyPath_EndToEnd(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	runner := &stubRunner{title: "Test Site", desc: "A tiny stub-built site."}
	handler := buildServerWithRunner(t, st, snapSvc, runner)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	client := &http.Client{Timeout: 10 * time.Second}
	authedGET := func(path string) (*http.Response, string) {
		req, err := http.NewRequest(http.MethodGet, httpSrv.URL+path, nil)
		if err != nil {
			t.Fatalf("new GET %s: %v", path, err)
		}
		req.Host = "localhost"
		req.AddCookie(testSessionCookie)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body %s: %v", path, err)
		}
		return resp, string(body)
	}

	// 1. Landing page renders with DaisyUI lemonade chrome.
	// authedGET defers Body.Close() inside the helper before returning resp;
	// bodyclose can't see through the closure, hence the suppress.
	resp, body := authedGET("/") //nolint:bodyclose // see comment.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /: %d", resp.StatusCode)
	}
	// Includes the bootstrap script + toggle markers so the dark-mode pass
	// can't silently regress. `tb_theme` is the localStorage key the
	// bootstrap script reads; `theme-toggle` is the input id wired up to
	// flip the data-theme attribute.
	for _, want := range []string{
		`data-theme="lemonade"`, "Top Banana", "Build a new app", `href="/app.css"`,
		"tb_theme", `id="theme-toggle"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("landing missing %q", want)
		}
	}

	// 2. POST /build kicks off a build. The new flow 303s straight to the
	//    workspace with ?building=1 — the workspace template renders the
	//    inline status strip in that mode rather than a separate /progress.
	slug := "happy-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	form := url.Values{
		"template": {"blank"},
		"slug":     {slug},
		"prompt":   {"A friendly hello page."},
	}
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/build", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new POST /build: %v", err)
	}
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(testSessionCookie)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	workspaceBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /build (after redirect): %d body=%q", resp.StatusCode, string(workspaceBody))
	}
	if !strings.Contains(resp.Request.URL.Path, "/workspace/"+slug) {
		t.Errorf("expected redirect to /workspace/%s, landed at %s", slug, resp.Request.URL.Path)
	}
	for _, want := range []string{"status-strip", `data-step="design"`, "Designing", ">Workspace<"} {
		if !strings.Contains(string(workspaceBody), want) {
			t.Errorf("workspace (building) missing %q", want)
		}
	}

	// 3. Consume the SSE event stream until the build reports completed/failed.
	consumeBuild(t, httpSrv.URL, slug, 30*time.Second)

	// 4. Site is listed on /apps after the build finishes — dense-list
	//    layout, so we check the row-level markers rather than card markup:
	//    whole-row workspace link, the small Open ↗ button, and the kebab
	//    dropdown wrapper.
	resp, body = authedGET("/apps") //nolint:bodyclose // see authedGET comment above.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apps: %d", resp.StatusCode)
	}
	for _, want := range []string{slug, `href="/workspace/` + slug + `"`, "Open ↗", "dropdown"} {
		if !strings.Contains(body, want) {
			t.Errorf("/apps body missing %q", want)
		}
	}

	// 5. Subdomain proxy serves the canned index.html from the stub agent.
	siteResp, siteBody := getSite(t, client, httpSrv.URL, slug+".localhost", "/") //nolint:bodyclose // getSite closes body internally.
	if siteResp.StatusCode != http.StatusOK {
		t.Fatalf("GET site: %d", siteResp.StatusCode)
	}
	if !strings.Contains(siteBody, "Hello, world") {
		t.Errorf("site body missing canned content; got %q", trim(siteBody, 200))
	}

	// 6. Workspace renders with the redesigned IA — left rail, prompt input,
	//    theme picker + history side panels. The legacy /edit/:slug path now
	//    redirects to /workspace/:slug; we follow the redirect and assert the
	//    workspace markers land.
	resp, body = authedGET("/edit/" + slug) //nolint:bodyclose // see authedGET comment above.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /edit/%s (after redirect): %d", slug, resp.StatusCode)
	}
	if !strings.Contains(resp.Request.URL.Path, "/workspace/"+slug) {
		t.Errorf("expected /edit/%s to redirect to /workspace, landed at %s", slug, resp.Request.URL.Path)
	}
	for _, want := range []string{">Workspace<", ">Manage<", "Describe a change", "panel-themes", "panel-history"} {
		if !strings.Contains(body, want) {
			t.Errorf("workspace missing %q", want)
		}
	}

	// 7. Legacy /edit/:slug/theme redirects to workspace (theme picker is a
	//    side panel there).
	resp, body = authedGET("/edit/" + slug + "/theme") //nolint:bodyclose // see authedGET comment above.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET theme: %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Request.URL.Path, "/workspace/"+slug) {
		t.Errorf("expected /edit/%s/theme to redirect to /workspace, landed at %s", slug, resp.Request.URL.Path)
	}
	if !strings.Contains(body, "panel-themes") {
		t.Errorf("workspace (theme redirect target) missing themes panel")
	}

	// 8a. /system surfaces the just-built slug in its Apps table.
	//     Cheap piggyback assertion — catches /system regressions caused by
	//     edits to the shared brand partial or the apps walk.
	resp, body = authedGET("/system") //nolint:bodyclose // see authedGET comment above.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /system: %d", resp.StatusCode)
	}
	if !strings.Contains(body, slug) {
		t.Errorf("/system did not list slug %q in the Apps table", slug)
	}

	// 8. Manage page renders with the consolidated sections; legacy /settings
	//    redirects here.
	resp, body = authedGET("/settings/" + slug) //nolint:bodyclose // see authedGET comment above.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET settings: %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Request.URL.Path, "/manage/"+slug) {
		t.Errorf("expected /settings/%s to redirect to /manage, landed at %s", slug, resp.Request.URL.Path)
	}
	for _, want := range []string{"Custom web address", "Permissions", "Form submissions", "Advanced tools", "Delete this app", slug} {
		if !strings.Contains(body, want) {
			t.Errorf("manage missing %q", want)
		}
	}

	// 9. Delete the app via the confirmation form.
	delForm := url.Values{"confirm": {slug}}
	delReq, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/apps/"+slug, strings.NewReader(delForm.Encode()))
	if err != nil {
		t.Fatalf("new POST delete: %v", err)
	}
	delReq.Host = "localhost"
	delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	delReq.Header.Set("X-HTTP-Method-Override", "DELETE")
	delReq.AddCookie(testSessionCookie)
	// Don't follow the redirect so we can assert it.
	noRedirect := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err = noRedirect.Do(delReq)
	if err != nil {
		t.Fatalf("POST delete: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status: %d (want 303)", resp.StatusCode)
	}

	// 10. /apps no longer lists the slug.
	resp, body = authedGET("/apps") //nolint:bodyclose // see authedGET comment above.
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apps post-delete: %d", resp.StatusCode)
	}
	if strings.Contains(body, slug) {
		t.Errorf("/apps still lists %q after delete", slug)
	}
}

// consumeBuild streams /events/:slug until a terminal status arrives or the
// deadline fires. Returns once `completed` is seen; t.Fatals on `failed` or
// timeout.
//
// per event type. Splitting it would scatter related state across helpers
// without making the flow clearer.
//
//nolint:cyclop // SSE-consuming test helper with sequential parse + branch
func consumeBuild(t *testing.T, base, slug string, deadline time.Duration) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/events/"+slug, nil)
	if err != nil {
		t.Fatalf("new GET events: %v", err)
	}
	req.Host = "localhost"
	req.Header.Set("Accept", "text/event-stream")
	// /events/:slug is now gated by requireUser + requireSlugOwnership;
	// pass the test session cookie so we land on the handler rather than a
	// 401.
	if testSessionCookie != nil {
		req.AddCookie(testSessionCookie)
	}
	c := &http.Client{Timeout: deadline + 5*time.Second}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status: %d", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	end := time.NewTimer(deadline)
	defer end.Stop()
	done := make(chan struct{})
	var terminal struct {
		status string
		msg    string
	}
	var sawTool bool
	go func() {
		defer close(done)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var ev struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Tool    string `json:"tool"`
				Phase   string `json:"phase"`
				Message string `json:"message"`
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			err := json.Unmarshal([]byte(payload), &ev)
			if err != nil {
				continue
			}
			if ev.Type == "tool" {
				sawTool = true
			}
			if ev.Type == "status" && (ev.Status == "completed" || ev.Status == "failed") {
				terminal.status = ev.Status
				terminal.msg = ev.Message
				return
			}
		}
	}()
	select {
	case <-done:
	case <-end.C:
		t.Fatalf("build did not complete within %s", deadline)
	}
	if terminal.status != "completed" {
		t.Fatalf("build did not complete cleanly: status=%q msg=%q", terminal.status, terminal.msg)
	}
	if !sawTool {
		t.Errorf("expected to see at least one tool event in the SSE stream")
	}
}

// getSite issues a request whose Host header makes the subdomainMiddleware
// route it to the live-site proxy for `slug.localhost`.
func getSite(t *testing.T, c *http.Client, base, host, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("new GET site: %v", err)
	}
	req.Host = host
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET site: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
