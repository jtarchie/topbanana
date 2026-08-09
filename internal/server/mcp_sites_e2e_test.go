package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/store"
)

// readSiteMeta decodes the per-site sidecar straight from the bucket — the
// counterpart to writeMeta, used to assert on what a tool persisted rather
// than on what it reported.
func readSiteMeta(t *testing.T, ctx context.Context, st *store.Store, slug string) build.SiteMeta {
	t.Helper()
	obj, err := st.Read(ctx, slug, build.MetaFile)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	meta := build.SiteMeta{}
	err = json.Unmarshal([]byte(obj.Content), &meta)
	if err != nil {
		t.Fatalf("decode meta: %v", err)
	}
	return meta
}

// create_site is the only MCP tool that brings a slug into existence, so these
// cover the three things that make a new site usable rather than merely
// present: the skeleton lands, the owner is recorded (a site with no OwnerID is
// orphaned — nobody can reach it again), and the very next tool call
// authorizes against it without waiting on an index sweep.

func TestMCP_CreateSite_SeedsAndOwns(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, freshSlug(t), owner)
	seedUser(t, authBlobs, owner)
	session := connectMCP(t, srv, owner)

	newSlug := freshSlug(t)
	out := mustCallTool(t, session, "create_site", map[string]any{
		"slug": newSlug, "template": "restaurant",
		"title": "Pepe's", "description": "Tacos",
	})
	if out["slug"] != newSlug {
		t.Fatalf("slug = %v; want %s", out["slug"], newSlug)
	}
	files, _ := json.Marshal(out["files"])
	if !strings.Contains(string(files), "index.html") {
		t.Errorf("skeleton not seeded: %s", files)
	}
	// The template's checklist comes back so the agent knows the target
	// before it writes a line.
	guide, _ := json.Marshal(out["guide"])
	if !strings.Contains(string(guide), "Opening hours") {
		t.Errorf("template guide missing from result: %s", guide)
	}

	// Ownership is on the sidecar, not just in memory — an in-memory-only
	// owner is lost on restart and the site becomes unreachable.
	meta := readSiteMeta(t, ctx, st, newSlug)
	if meta.OwnerID != owner {
		t.Errorf("OwnerID = %q; want %q", meta.OwnerID, owner)
	}
	if meta.Template != "restaurant" {
		t.Errorf("Template = %q; want restaurant", meta.Template)
	}
	if meta.Title != "Pepe's" || meta.Description != "Tacos" {
		t.Errorf("title/description not persisted: %+v", meta)
	}

	// The registry was updated in-band, so editing works immediately rather
	// than after the next ListApps sweep.
	mustCallTool(t, session, "write_file", map[string]any{
		"slug": newSlug, "path": "about.html",
		"content": "<html><head><title>About</title></head><body>hi</body></html>",
	})
}

func TestMCP_CreateSite_RejectsUnknownTemplateAndTakenSlug(t *testing.T) {
	st := minioStore(t)
	const owner = "owner@example.com"
	taken := freshSlug(t)

	srv, authBlobs, _ := buildMCPTestServer(t, st, taken, owner)
	seedUser(t, authBlobs, owner)
	session := connectMCP(t, srv, owner)

	// Unknown template errors rather than silently falling back to blank —
	// an agent asking for "restraunt" should be corrected, not handed a blank
	// site it will treat as a restaurant one.
	res := callTool(t, session, "create_site", map[string]any{"template": "restraunt"})
	if !res.IsError || !strings.Contains(toolText(res), "unknown template") {
		t.Errorf("unknown template not rejected: %s", toolText(res))
	}

	// A taken slug reports the conflict as a sentence, not as an
	// echo-shaped "code=409, message=..." string.
	res = callTool(t, session, "create_site", map[string]any{"slug": taken})
	text := toolText(res)
	if !res.IsError || !strings.Contains(text, "already taken") {
		t.Errorf("taken slug not rejected: %s", text)
	}
	if strings.Contains(text, "code=") {
		t.Errorf("HTTP error leaked into tool result: %s", text)
	}
}

// TestMCP_GetSiteGuide_ReportsMissingEssentials: the checklist answers what
// lint cannot — the page is valid HTML and still missing everything a
// restaurant page needs.
func TestMCP_GetSiteGuide_ReportsMissingEssentials(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner)
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: owner, Template: "restaurant"})
	session := connectMCP(t, srv, owner)

	out := mustCallTool(t, session, "get_site_guide", map[string]any{"slug": slug})
	if out["complete"] != false {
		t.Errorf("a bare page should not read as complete: %v", out)
	}
	total, _ := out["total"].(float64)
	if total == 0 {
		t.Fatalf("restaurant template should declare guide items: %v", out)
	}
	items, _ := json.Marshal(out["items"])
	// Each item carries the why/how the owner-facing card renders, so an
	// agent can act on a miss without another round trip.
	if !strings.Contains(string(items), `"why"`) || !strings.Contains(string(items), `"present":false`) {
		t.Errorf("items missing why/present: %s", items)
	}
	if _, ok := out["next"]; !ok {
		t.Errorf("missing essentials should be summarized in next: %v", out)
	}
}

// TestMCP_AssetMetadata_RoundTrip: an image uploaded through the ticket flow is
// visible to list_assets and its alt text is writable — the gap that made
// MCP-authored alt text write-once and unreadable.
func TestMCP_AssetMetadata_RoundTrip(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner)
	mustWrite(t, ctx, st, slug, "assets/hero.png", "\x89PNG\r\n\x1a\nfake", "image/png")
	session := connectMCP(t, srv, owner)

	out := mustCallTool(t, session, "list_assets", map[string]any{"slug": slug})
	body, _ := json.Marshal(out)
	if !strings.Contains(string(body), "assets/hero.png") {
		t.Fatalf("uploaded asset not listed: %s", body)
	}
	// An asset with no alt text is called out, since nothing else on the
	// surface would tell the agent it left one behind.
	if next, _ := out["next"].(string); !strings.Contains(next, "alt text") {
		t.Errorf("missing-alt nudge absent: %v", out["next"])
	}

	// The assets/ prefix is optional on the way in.
	mustCallTool(t, session, "set_asset_metadata", map[string]any{
		"slug": slug, "path": "hero.png",
		"alt": "A plate of tacos", "description": "Hero shot for the home page",
	})

	obj, err := st.Read(ctx, slug, "assets/hero.png")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if obj.Metadata["alt"] != "A plate of tacos" {
		t.Errorf("alt = %q; want %q", obj.Metadata["alt"], "A plate of tacos")
	}
	if obj.ContentType != "image/png" {
		t.Errorf("content type not preserved: %q", obj.ContentType)
	}

	// Updating only alt must not wipe the description. Uploads arrive with a
	// vision-model caption in both fields, so a wholesale metadata replace
	// would silently delete the description an agent never meant to touch.
	mustCallTool(t, session, "set_asset_metadata", map[string]any{
		"slug": slug, "path": "hero.png", "alt": "Tacos, overhead",
	})
	obj, err = st.Read(ctx, slug, "assets/hero.png")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if obj.Metadata["description"] != "Hero shot for the home page" {
		t.Errorf("omitting description cleared it: %q", obj.Metadata["description"])
	}
	if obj.Metadata["alt"] != "Tacos, overhead" {
		t.Errorf("alt not updated: %q", obj.Metadata["alt"])
	}

	// An explicit empty string still clears — that's the documented escape.
	mustCallTool(t, session, "set_asset_metadata", map[string]any{
		"slug": slug, "path": "hero.png", "description": "",
	})
	obj, err = st.Read(ctx, slug, "assets/hero.png")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if obj.Metadata["description"] != "" {
		t.Errorf("explicit empty description did not clear: %q", obj.Metadata["description"])
	}

	// A missing asset is a clear refusal, not a conjured empty object.
	res := callTool(t, session, "set_asset_metadata", map[string]any{
		"slug": slug, "path": "nope.png", "alt": "x",
	})
	if !res.IsError || !strings.Contains(toolText(res), "not found") {
		t.Errorf("missing asset not refused: %s", toolText(res))
	}
}
