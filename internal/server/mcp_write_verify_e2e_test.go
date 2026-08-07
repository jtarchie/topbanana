package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/state"
)

// These cover the three gaps an external agent hit driving a real site: a
// typo in a CSS file forced a full re-upload because the edit tools were
// HTML-only; large content corrupted silently in transit through JSON tool
// arguments, twice; and test rows created while verifying a form could not be
// removed afterwards.

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// callTool is the raw call — tests here assert on failures as often as
// successes, so the error case can't be a t.Fatal.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s transport error: %v", name, err)
	}
	return res
}

func TestMCP_TextAssetsAreEditable(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner)
	session := connectMCP(t, srv, owner)

	const css = ".banner { color: rebeccapurple; font-size: 18px; }"
	res := callTool(t, session, "write_file", map[string]any{
		"slug": slug, "path": "fonts.css", "content": css,
	})
	if res.IsError {
		t.Fatalf("write_file css errored: %s", toolText(res))
	}

	// The one-character fix that previously required re-sending the file.
	res = callTool(t, session, "edit_file", map[string]any{
		"slug": slug, "path": "fonts.css",
		"old_text": "18px", "new_text": "19px",
	})
	if res.IsError {
		t.Fatalf("edit_file on css errored: %s", toolText(res))
	}

	obj, err := st.Read(ctx, slug, "fonts.css")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(obj.Content, "19px") {
		t.Fatalf("css edit not persisted: %q", obj.Content)
	}
	// The asset must still be served as CSS — an edit that re-labelled it as
	// HTML would make the page silently unstyled.
	if !strings.HasPrefix(obj.ContentType, "text/css") {
		t.Errorf("content type = %q; want text/css", obj.ContentType)
	}
}

// Executable handlers and the site's captured submissions are reachable only
// through their own tools. write_file ran no path validation at all before
// this, so both were one call away from being clobbered.
func TestMCP_WriteFileRefusesPlatformPaths(t *testing.T) {
	st := minioStore(t)
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner)
	session := connectMCP(t, srv, owner)

	for _, path := range []string{"functions/pwn.js", "_state/data.json", ".topbanana.json", "page.php"} {
		res := callTool(t, session, "write_file", map[string]any{
			"slug": slug, "path": path, "content": "nope",
		})
		if !res.IsError {
			t.Errorf("write_file(%q) succeeded; want rejection", path)
		}
	}

	// The site's own sidecar is intact, so ownership still resolves.
	obj, err := st.Read(context.Background(), slug, ".topbanana.json")
	if err != nil || strings.Contains(obj.Content, "nope") {
		t.Fatalf("sidecar clobbered: %q (%v)", obj.Content, err)
	}
}

// delete_file ran no validation at all, which made the site sidecar and the
// stored submissions one call away from destruction — deleting the sidecar
// empties OwnerID, so the owner loses access to their own site.
func TestMCP_DeleteFileRefusesPlatformPaths(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner)
	session := connectMCP(t, srv, owner)

	for _, path := range []string{".topbanana.json", "_state/data.json", "functions/submit.js", "app.css"} {
		res := callTool(t, session, "delete_file", map[string]any{"slug": slug, "path": path})
		if !res.IsError {
			t.Errorf("delete_file(%q) succeeded; want rejection", path)
		}
	}

	// The sidecar is intact, so the site still resolves to its owner.
	obj, err := st.Read(ctx, slug, ".topbanana.json")
	if err != nil || !strings.Contains(obj.Content, owner) {
		t.Fatalf("sidecar lost its owner: %q (%v)", obj.Content, err)
	}

	// An ordinary page still deletes, and so does an uploaded image — the
	// delete gate has to stay wider than the write gate.
	mustWrite(t, ctx, st, slug, "gone.html", "<html></html>", "text/html; charset=utf-8")
	mustWrite(t, ctx, st, slug, "assets/logo.png", "\x89PNG fake", "image/png")
	for _, path := range []string{"gone.html", "assets/logo.png"} {
		res := callTool(t, session, "delete_file", map[string]any{"slug": slug, "path": path})
		if res.IsError {
			t.Errorf("delete_file(%q) errored: %s", path, toolText(res))
		}
	}
}

// An agent that submits test rows to check a form it just built leaves them
// permanently interleaved with real signups in the owner's export. Cleaning up
// after itself needs a delete, and that delete must be as narrow as the manage
// page's: submissions only, one key at a time.
func TestMCP_DeleteSubmission(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, kv := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner)
	session := connectMCP(t, srv, owner)

	err := kv.Save(ctx, slug, &state.Snapshot{Data: map[string]any{
		"sub_real": map[string]any{"email": "real-person@example.com"},
		"sub_test": map[string]any{"email": "agent-test@example.com"},
		// A scalar counter, which is state but not a submission.
		"submission_seq": float64(2),
	}})
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}

	res := callTool(t, session, "list_submissions", map[string]any{"slug": slug})
	if res.IsError {
		t.Fatalf("list_submissions errored: %s", toolText(res))
	}
	if !strings.Contains(toolText(res), "sub_test") {
		t.Fatalf("list_submissions missing the seeded row: %s", toolText(res))
	}

	res = callTool(t, session, "delete_submission", map[string]any{"slug": slug, "key": "sub_test"})
	if res.IsError {
		t.Fatalf("delete_submission errored: %s", toolText(res))
	}

	snap, err := kv.Load(ctx, slug)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if _, still := snap.Data["sub_test"]; still {
		t.Error("test submission was not deleted")
	}
	if _, kept := snap.Data["sub_real"]; !kept {
		t.Error("delete_submission removed the wrong row")
	}

	// Non-submission state stays out of reach, and a missing key is an error
	// rather than a silent success.
	if res := callTool(t, session, "delete_submission", map[string]any{"slug": slug, "key": "submission_seq"}); !res.IsError {
		t.Error("delete_submission should refuse a non-submission key")
	}
	if res := callTool(t, session, "delete_submission", map[string]any{"slug": slug, "key": "nope"}); !res.IsError {
		t.Error("delete_submission should error on a missing key")
	}
}

func TestMCP_WriteFileVerifiesDigest(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner)
	session := connectMCP(t, srv, owner)

	const intended = "<html><head><title>Real</title></head><body>ok</body></html>"
	// What the agent meant to send, versus what a mangled transfer delivers.
	corrupted := strings.Replace(intended, "ok", "o k", 1)

	res := callTool(t, session, "write_file", map[string]any{
		"slug": slug, "path": "verified.html",
		"content": corrupted, "expect_sha256": sha256Hex(intended),
	})
	if !res.IsError {
		t.Fatal("write_file accepted content that does not match expect_sha256")
	}
	if !strings.Contains(toolText(res), "sha256 mismatch") {
		t.Errorf("mismatch error should name the cause, got: %s", toolText(res))
	}
	// A rejected write must not land — that's the whole point of checking
	// before the store call rather than after.
	obj, err := st.Read(ctx, slug, "verified.html")
	if err != nil || obj.Content != "" {
		t.Fatalf("rejected write was persisted anyway: %q (%v)", obj.Content, err)
	}

	// The matching digest goes through, and the result reports it back.
	res = callTool(t, session, "write_file", map[string]any{
		"slug": slug, "path": "verified.html",
		"content": intended, "expect_sha256": sha256Hex(intended),
	})
	if res.IsError {
		t.Fatalf("write_file with correct digest errored: %s", toolText(res))
	}
	var out struct {
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
	}
	err = json.Unmarshal([]byte(toolText(res)), &out)
	if err != nil {
		t.Fatalf("decode write_file result: %v", err)
	}
	if out.SHA256 != sha256Hex(intended) || out.Bytes != len(intended) {
		t.Errorf("result = %+v; want sha %s and %d bytes", out, sha256Hex(intended), len(intended))
	}

	// read_file reports the same digest, so a client can verify a round-trip
	// without leaving the tool surface.
	res = callTool(t, session, "read_file", map[string]any{"slug": slug, "path": "verified.html"})
	if res.IsError {
		t.Fatalf("read_file errored: %s", toolText(res))
	}
	if !strings.Contains(toolText(res), sha256Hex(intended)) {
		t.Errorf("read_file did not echo the stored digest: %s", toolText(res))
	}
}
