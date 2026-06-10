package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/sandbox"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
)

// This file drives registered MCP tools end-to-end through the real stack —
// HTTP server, bearer middleware, tool registration, ownership authorization,
// and the mcpApplyToFile edit pipeline — using the SDK's streamable HTTP
// client. Previously only leaf helpers were tested, so a regression in
// authorizeSlugOwner or the edit orchestration passed CI silently.

const mcpTestSecret = "mcp-e2e-test-secret"

// buildMCPTestServer seeds a site owned by ownerEmail, then stands up the full
// server with the MCP surface enabled. Seeding happens before server.New so
// the registry's initial index rebuild records the ownership.
func buildMCPTestServer(t *testing.T, st *store.Store, slug, ownerEmail string) *httptest.Server {
	t.Helper()
	ctx := context.Background()

	mustWrite(t, ctx, st, slug, "index.html", "<html><head></head><body><h1>Hello</h1></body></html>", "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: ownerEmail})

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	authSvc, err := auth.New(auth.Config{
		Store:           st,
		Domain:          "localhost",
		SuperAdminEmail: "super@example.com",
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = authSvc.Close() })

	e, _ := server.New(server.Deps{
		Store:     st,
		Build:     build.New(st, nil, tracker, snapshot.New(st, 0)),
		Events:    tracker,
		Sandbox:   sandbox.New(sandbox.Config{}),
		State:     state.NewMemory(),
		Snapshot:  snapshot.New(st, 0),
		Auth:      authSvc,
		Domain:    "localhost",
		Port:      "8080",
		MCPSecret: mcpTestSecret,
	})
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

// bearerTransport injects the MCP bearer token on every request the SDK client
// makes — the same header a real external agent sends.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r) //nolint:wrapcheck // transparent transport wrapper
}

// connectMCP mints a bearer token for email and opens an SDK client session
// against the server's /mcp endpoint.
func connectMCP(t *testing.T, srv *httptest.Server, email string) *mcp.ClientSession {
	t.Helper()
	token, err := auth.MintMCPToken(mcpTestSecret, email, auth.MCPTokenTTL)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "0.0.1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token, base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("mcp connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// toolText concatenates the text content blocks of a tool result.
func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestMCP_ListAndEditFile_EndToEnd(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv := buildMCPTestServer(t, st, slug, owner)

	// The user record must exist for authorizeSlugOwner's lookup; the auth
	// store is shared with the server through the same backing store.
	session := connectMCP(t, srv, owner)
	seedUser(t, srv, st, owner)

	// list_files sees the seeded page.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_files", Arguments: map[string]any{"slug": slug},
	})
	if err != nil {
		t.Fatalf("list_files: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_files errored: %s", toolText(res))
	}
	if !strings.Contains(toolText(res), "index.html") {
		t.Fatalf("list_files output missing index.html: %s", toolText(res))
	}

	// edit_file runs the full mcpApplyToFile pipeline and persists the change.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "edit_file", Arguments: map[string]any{
			"slug": slug, "path": "index.html",
			"old_text": "<h1>Hello</h1>", "new_text": "<h1>Howdy</h1>",
		},
	})
	if err != nil {
		t.Fatalf("edit_file: %v", err)
	}
	if res.IsError {
		t.Fatalf("edit_file errored: %s", toolText(res))
	}

	obj, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(obj.Content, "<h1>Howdy</h1>") {
		t.Fatalf("edit not persisted: %s", obj.Content)
	}
	if obj.ContentType != "text/html; charset=utf-8" {
		t.Fatalf("content type not preserved: %q", obj.ContentType)
	}
}

func TestMCP_NonOwnerSeesNotFound(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"
	const stranger = "stranger@example.com"

	srv := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, srv, st, owner)
	seedUser(t, srv, st, stranger)

	session := connectMCP(t, srv, stranger)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "edit_file", Arguments: map[string]any{
			"slug": slug, "path": "index.html",
			"old_text": "<h1>Hello</h1>", "new_text": "<h1>Pwned</h1>",
		},
	})
	if err != nil {
		t.Fatalf("edit_file transport error: %v", err)
	}
	// Ownership failures surface as tool errors phrased "not found" so the
	// slug's existence never leaks to a non-owner.
	if !res.IsError {
		t.Fatal("non-owner edit_file should be a tool error")
	}
	if !strings.Contains(strings.ToLower(toolText(res)), "not found") {
		t.Fatalf("non-owner error should read as not-found, got: %s", toolText(res))
	}

	obj, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(obj.Content, "Pwned") {
		t.Fatal("non-owner edit must not be persisted")
	}
}

func TestMCP_BadTokenRejectedByBearerMiddleware(t *testing.T) {
	st := minioStore(t)
	slug := freshSlug(t)
	srv := buildMCPTestServer(t, st, slug, "owner@example.com")

	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test", Version: "0.0.1"}, nil)
	_, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: "garbage", base: http.DefaultTransport}},
		DisableStandaloneSSE: true,
	}, nil)
	if err == nil {
		t.Fatal("connect with a forged bearer token should fail at the middleware")
	}
}

// TestMCP_FullToolSurfaceLifecycle walks the remaining registered tools as one
// authoring session: site discovery, the page write/read/replace/insert/delete
// pipeline, configure_site flipping functions on, the full function lifecycle
// including a real sandboxed test_function run, transcript listing/fetch, and
// lint_site — all over the real HTTP stack with a real bearer token.
func TestMCP_FullToolSurfaceLifecycle(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, srv, st, owner)
	session := connectMCP(t, srv, owner)

	mustOK := func(name string, args map[string]any) string {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s transport error: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s errored: %s", name, toolText(res))
		}
		return toolText(res)
	}
	mustContain := func(name string, args map[string]any, needle string) {
		t.Helper()
		out := mustOK(name, args)
		if !strings.Contains(out, needle) {
			t.Fatalf("%s output missing %q: %s", name, needle, out)
		}
	}

	// Site discovery.
	mustContain("list_sites", nil, slug)
	mustContain("get_site", map[string]any{"slug": slug}, slug)

	// Page pipeline: write → read → replace_lines → insert_at_line compose to
	// an exact document, then delete removes it.
	mustOK("write_file", map[string]any{"slug": slug, "path": "about.html", "content": "alpha\nbeta\ngamma"})
	mustContain("read_file", map[string]any{"slug": slug, "path": "about.html"}, "beta")
	mustOK("replace_lines", map[string]any{"slug": slug, "path": "about.html", "start_line": 3, "end_line": 3, "new_text": "GAMMA"})
	mustOK("insert_at_line", map[string]any{"slug": slug, "path": "about.html", "after_line": 1, "content": "inserted"})
	obj, err := st.Read(ctx, slug, "about.html")
	if err != nil || obj.Content != "alpha\ninserted\nbeta\nGAMMA" {
		t.Fatalf("page pipeline content = %q (%v)", obj.Content, err)
	}
	mustOK("delete_file", map[string]any{"slug": slug, "path": "about.html"})
	obj, err = st.Read(ctx, slug, "about.html")
	if err != nil || obj.Content != "" {
		t.Fatalf("delete_file did not remove the page: %q (%v)", obj.Content, err)
	}

	// configure_site persists the title and enables functions for the next leg.
	mustOK("configure_site", map[string]any{"slug": slug, "title": "Banana Stand", "enable_functions": true})
	sidecar, err := st.Read(ctx, slug, build.MetaFile)
	if err != nil || !strings.Contains(sidecar.Content, "Banana Stand") {
		t.Fatalf("configure_site did not persist the title: %q (%v)", sidecar.Content, err)
	}

	// Function lifecycle, including a real sandboxed invocation.
	const src = `module.exports = function (req) { return response.json({answer: "pong"}); };`
	mustOK("write_function", map[string]any{"slug": slug, "name": "ping", "source": src})
	mustContain("list_functions", map[string]any{"slug": slug}, "ping")
	mustContain("read_function", map[string]any{"slug": slug, "name": "ping"}, "pong")
	mustOK("edit_function", map[string]any{"slug": slug, "name": "ping", "old_text": `"pong"`, "new_text": `"PONG"`})
	mustContain("test_function", map[string]any{"slug": slug, "name": "ping"}, "PONG")
	mustOK("delete_function", map[string]any{"slug": slug, "name": "ping"})
	if out := mustOK("list_functions", map[string]any{"slug": slug}); strings.Contains(out, "ping") {
		t.Fatalf("function still listed after delete: %s", out)
	}

	// The mutations above were recorded as run transcripts; fetch one back.
	var runs struct {
		Runs []string `json:"runs"`
	}
	out := mustOK("list_runs", map[string]any{"slug": slug})
	err = json.Unmarshal([]byte(out), &runs)
	if err != nil || len(runs.Runs) == 0 {
		t.Fatalf("list_runs = %s (%v), want at least one transcript", out, err)
	}
	if tr := mustOK("get_run_transcript", map[string]any{"slug": slug, "key": runs.Runs[0]}); tr == "" {
		t.Fatal("get_run_transcript returned empty")
	}

	// lint_site reports on the seeded page (the minimal index.html lacks the
	// design substrate, so problems are expected — the point is the tool runs).
	if out := mustOK("lint_site", map[string]any{"slug": slug}); out == "" {
		t.Fatal("lint_site returned empty")
	}
}

// seedUser creates a user record (via the auth store the server shares) so
// authorizeSlugOwner's lookup resolves. Uses InjectTestSession's user-creation
// side effect rather than reaching into unexported stores.
func seedUser(t *testing.T, _ *httptest.Server, st *store.Store, email string) {
	t.Helper()
	a, err := auth.New(auth.Config{
		Store:           st,
		Domain:          "localhost",
		SuperAdminEmail: "super@example.com",
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	defer func() { _ = a.Close() }()
	_, err = a.InjectTestSession(context.Background(), email, auth.RoleAdmin)
	if err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
}
