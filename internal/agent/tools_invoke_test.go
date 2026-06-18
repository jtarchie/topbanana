package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	adk "google.golang.org/adk/agent"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// This file drives the REAL registered tool closures — the exact functions the
// LLM calls in production — through the ADK functiontool Run surface, against
// the in-memory store. Previously only extracted helpers (applyToFile,
// invokeAskUser, seeds) were tested and the closure bodies (validation, caps,
// guard wiring, store round-trips, result shaping) were dark.

// stubInvocation is the minimal adk.InvocationContext the tools need: they
// only ever use it as a context.Context, and adk.NewToolContext touches just
// Artifacts() at construction.
//
//nolint:containedctx // adk.InvocationContext itself embeds context.Context; a stub must too.
type stubInvocation struct{ context.Context }

func (stubInvocation) Agent() adk.Agent            { return nil }
func (stubInvocation) Artifacts() adk.Artifacts    { return nil }
func (stubInvocation) Memory() adk.Memory          { return nil }
func (stubInvocation) Session() session.Session    { return nil }
func (stubInvocation) InvocationID() string        { return "test-invocation" }
func (stubInvocation) Branch() string              { return "" }
func (stubInvocation) UserContent() *genai.Content { return nil }
func (stubInvocation) RunConfig() *adk.RunConfig   { return nil }
func (stubInvocation) EndInvocation()              {}
func (stubInvocation) Ended() bool                 { return false }
func (s stubInvocation) WithContext(ctx context.Context) adk.InvocationContext {
	return stubInvocation{ctx}
}

// invokeTool calls a registered tool the way the ADK runner would, decoding
// args from the wire-shaped map and returning the wire-shaped result map.
func invokeTool(t *testing.T, tl tool.Tool, args map[string]any) map[string]any {
	t.Helper()
	r, ok := tl.(interface {
		Run(adk.ToolContext, any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("tool %s does not expose the functiontool Run surface", tl.Name())
	}
	tctx := adk.NewToolContext(stubInvocation{t.Context()}, "", &session.EventActions{}, nil)
	out, err := r.Run(tctx, args)
	if err != nil {
		t.Fatalf("tool %s run: %v", tl.Name(), err)
	}
	return out
}

// newToolSet builds the full production tool surface (functions enabled,
// examples + attachments present, tracker wired) via buildAgentTools and
// indexes it by name.
func newToolSet(t *testing.T) (map[string]tool.Tool, *store.Store, *buildState) {
	t.Helper()
	s, err := store.NewInMemory(0)
	if err != nil {
		t.Fatalf("store.NewInMemory: %v", err)
	}
	state := newBuildState()
	tmpl := &templates.SiteTemplate{
		EnablesFunctions: true,
		Examples:         map[string]string{"hero": "<h1>Example Hero</h1>"},
	}
	atts := []Attachment{{Name: "notes.md", Content: "# the brief"}}
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)

	tools, err := buildAgentTools(s, "toolslug", tmpl, atts, func(events.Event) {}, state, tracker)
	if err != nil {
		t.Fatalf("buildAgentTools: %v", err)
	}
	byName := make(map[string]tool.Tool, len(tools))
	for _, tl := range tools {
		byName[tl.Name()] = tl
	}
	return byName, s, state
}

func resStr(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func resBool(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func TestAgentTools_RegistersFullSurface(t *testing.T) {
	tools, _, _ := newToolSet(t)
	for _, name := range []string{
		"write_file", "edit_file", "replace_lines", "insert_at_line",
		"read_file", "list_files", "grep_files", "list_assets", "search_docs",
		"read_attachment", "read_example", "fetch_reference",
		"write_function", "edit_function", "delete_function", "read_function", "list_functions",
		"ask_user",
	} {
		if tools[name] == nil {
			t.Errorf("tool %s not registered", name)
		}
	}
}

func TestAgentTools_WriteReadListRoundTrip(t *testing.T) {
	tools, s, _ := newToolSet(t)
	ctx := context.Background()

	res := invokeTool(t, tools["write_file"], map[string]any{
		"path": "index.html", "content": "line one\nline two\nline three",
	})
	if !resBool(res, "ok") {
		t.Fatalf("write_file failed: %v", res)
	}
	if !strings.Contains(resStr(res, "hints"), "1 of 25 HTML files used") {
		t.Errorf("budget hint missing/wrong: %q", resStr(res, "hints"))
	}
	obj, err := s.Read(ctx, "toolslug", "index.html")
	if err != nil || !strings.Contains(obj.Content, "line two") {
		t.Fatalf("write_file did not persist: %v / %q", err, obj.Content)
	}

	// Traversal and oversize writes are refused as in-result errors (the model
	// recovers from those; a Go error would abort the run).
	res = invokeTool(t, tools["write_file"], map[string]any{"path": "../escape.html", "content": "x"})
	if resBool(res, "ok") || resStr(res, "error") == "" {
		t.Fatalf("traversal path accepted: %v", res)
	}
	res = invokeTool(t, tools["write_file"], map[string]any{
		"path": "big.html", "content": strings.Repeat("a", maxHTMLFileBytes+1),
	})
	if !strings.Contains(resStr(res, "error"), "too large") {
		t.Fatalf("oversize write not capped: %v", res)
	}

	// read_file returns cat -n numbered content, sliced 1-indexed inclusive.
	res = invokeTool(t, tools["read_file"], map[string]any{"path": "index.html"})
	if !strings.Contains(resStr(res, "content"), "2\tline two") {
		t.Errorf("read_file not numbered: %q", resStr(res, "content"))
	}
	if fmt.Sprint(res["total_lines"]) != "3" {
		t.Errorf("total_lines = %v, want 3", res["total_lines"])
	}
	res = invokeTool(t, tools["read_file"], map[string]any{"path": "index.html", "start_line": 2, "end_line": 2})
	content := resStr(res, "content")
	if !strings.Contains(content, "2\tline two") || strings.Contains(content, "line one") {
		t.Errorf("read_file slice wrong: %q", content)
	}
	// An out-of-range slice clamps to empty rather than erroring; total_lines
	// still tells the model the real size so it can correct itself.
	res = invokeTool(t, tools["read_file"], map[string]any{"path": "index.html", "start_line": 9, "end_line": 9})
	if resStr(res, "content") != "" || fmt.Sprint(res["total_lines"]) != "3" {
		t.Errorf("out-of-range slice = %v, want empty content with total_lines 3", res)
	}
}

func TestAgentTools_ListFilesFiltersToHTML(t *testing.T) {
	tools, s, _ := newToolSet(t)
	mustStoreWrite(t, s, "toolslug", "index.html", "<p>hi</p>", "text/html; charset=utf-8", nil)
	mustStoreWrite(t, s, "toolslug", "assets/pic.png", "png-bytes", "image/png", nil)

	res := invokeTool(t, tools["list_files"], nil)
	files := fmt.Sprint(res["files"])
	if !strings.Contains(files, "index.html") || strings.Contains(files, "assets/pic.png") {
		t.Errorf("list_files = %v, want HTML only", files)
	}
}

func TestAgentTools_WriteFileCapBlocksNewAllowsOverwrite(t *testing.T) {
	tools, s, _ := newToolSet(t)

	for i := range maxHTMLFiles {
		mustStoreWrite(t, s, "toolslug", fmt.Sprintf("page%d.html", i), "<p>x</p>", "text/html; charset=utf-8", nil)
	}

	// A NEW page beyond the cap is blocked...
	res := invokeTool(t, tools["write_file"], map[string]any{"path": "extra.html", "content": "<p>y</p>"})
	if !strings.Contains(resStr(res, "error"), "file limit") {
		t.Fatalf("cap did not block a new file: %v", res)
	}
	// ...but overwriting an existing page is always allowed.
	res = invokeTool(t, tools["write_file"], map[string]any{"path": "page0.html", "content": "<p>updated</p>"})
	if !resBool(res, "ok") {
		t.Fatalf("overwrite at the cap should be allowed: %v", res)
	}
	// The anti-loop guard rejects the byte-identical repeat write.
	res = invokeTool(t, tools["write_file"], map[string]any{"path": "page0.html", "content": "<p>updated</p>"})
	if !strings.Contains(resStr(res, "error"), "repeated") {
		t.Fatalf("guard did not catch the repeated write: %v", res)
	}
}

func TestAgentTools_EditToolsThroughClosures(t *testing.T) {
	tools, s, _ := newToolSet(t)
	ctx := context.Background()
	mustStoreWrite(t, s, "toolslug", "index.html", "alpha\nbeta\ngamma", "text/html; charset=utf-8", nil)

	// edit_file: no-op short-circuit, then a real replacement.
	res := invokeTool(t, tools["edit_file"], map[string]any{
		"path": "index.html", "old_text": "beta", "new_text": "beta",
	})
	if !resBool(res, "ok") || !strings.Contains(resStr(res, "note"), "identical") {
		t.Fatalf("no-op edit should succeed with a note: %v", res)
	}
	res = invokeTool(t, tools["edit_file"], map[string]any{
		"path": "index.html", "old_text": "beta", "new_text": "BETA",
	})
	if !resBool(res, "ok") {
		t.Fatalf("edit_file failed: %v", res)
	}

	// replace_lines swaps a 1-indexed range; insert_at_line appends after N.
	res = invokeTool(t, tools["replace_lines"], map[string]any{
		"path": "index.html", "start_line": 3, "end_line": 3, "new_text": "GAMMA",
	})
	if !resBool(res, "ok") {
		t.Fatalf("replace_lines failed: %v", res)
	}
	res = invokeTool(t, tools["insert_at_line"], map[string]any{
		"path": "index.html", "after_line": 1, "content": "inserted",
	})
	if !resBool(res, "ok") {
		t.Fatalf("insert_at_line failed: %v", res)
	}

	obj, err := s.Read(ctx, "toolslug", "index.html")
	if err != nil {
		t.Fatal(err)
	}
	want := "alpha\ninserted\nBETA\nGAMMA"
	if obj.Content != want {
		t.Fatalf("after edit pipeline content = %q, want %q", obj.Content, want)
	}

	// Missing target surfaces as an in-result error.
	res = invokeTool(t, tools["edit_file"], map[string]any{
		"path": "nope.html", "old_text": "x", "new_text": "y",
	})
	if !strings.Contains(resStr(res, "error"), "not found") {
		t.Fatalf("edit of missing file: %v", res)
	}
}

func TestAgentTools_GrepAndAssets(t *testing.T) {
	tools, s, _ := newToolSet(t)
	mustStoreWrite(t, s, "toolslug", "index.html", "<p>needle in page</p>", "text/html; charset=utf-8", nil)
	mustStoreWrite(t, s, "toolslug", "assets/pic.png", "png-bytes", "image/png",
		map[string]string{"alt": "a smiling banana", "description": "hero art"})

	res := invokeTool(t, tools["grep_files"], map[string]any{"pattern": "needle"})
	if fmt.Sprint(res["total_matches"]) != "1" || !strings.Contains(fmt.Sprint(res["matches"]), "index.html") {
		t.Fatalf("grep_files = %v", res)
	}
	// The jsonschema layer enforces presence; the closure's own guard catches a
	// present-but-empty pattern.
	res = invokeTool(t, tools["grep_files"], map[string]any{"pattern": ""})
	if !strings.Contains(resStr(res, "error"), "pattern is required") {
		t.Fatalf("empty pattern should error: %v", res)
	}

	res = invokeTool(t, tools["list_assets"], nil)
	assets := fmt.Sprint(res["assets"])
	if !strings.Contains(assets, "assets/pic.png") || !strings.Contains(assets, "a smiling banana") || !strings.Contains(assets, "hero art") {
		t.Fatalf("list_assets missing path/alt/description: %v", assets)
	}
}

func TestAgentTools_FunctionLifecycle(t *testing.T) {
	tools, s, _ := newToolSet(t)
	ctx := context.Background()
	const src = `module.exports = function (req) { return response.json({ok:true}); };`

	res := invokeTool(t, tools["write_function"], map[string]any{"name": "submit", "source": src})
	if !resBool(res, "ok") || resStr(res, "path") != "functions/submit.js" {
		t.Fatalf("write_function = %v", res)
	}
	obj, err := s.Read(ctx, "toolslug", "functions/submit.js")
	if err != nil || obj.Content != src {
		t.Fatalf("function not persisted: %v / %q", err, obj.Content)
	}
	if !strings.HasPrefix(obj.ContentType, "application/javascript") {
		t.Errorf("function content type = %q", obj.ContentType)
	}

	res = invokeTool(t, tools["write_function"], map[string]any{"name": "Bad/Name", "source": src})
	if resStr(res, "error") == "" {
		t.Fatalf("invalid function name accepted: %v", res)
	}

	res = invokeTool(t, tools["list_functions"], nil)
	if !strings.Contains(fmt.Sprint(res["functions"]), "submit") {
		t.Fatalf("list_functions = %v", res)
	}

	res = invokeTool(t, tools["read_function"], map[string]any{"name": "submit"})
	if resStr(res, "source") != src {
		t.Fatalf("read_function = %v", res)
	}

	res = invokeTool(t, tools["edit_function"], map[string]any{
		"name": "submit", "old_text": "ok:true", "new_text": "ok:false",
	})
	if !resBool(res, "ok") {
		t.Fatalf("edit_function = %v", res)
	}
	res = invokeTool(t, tools["edit_function"], map[string]any{
		"name": "missing", "old_text": "a", "new_text": "b",
	})
	if !strings.Contains(resStr(res, "error"), "not found") {
		t.Fatalf("edit of missing function: %v", res)
	}

	res = invokeTool(t, tools["delete_function"], map[string]any{"name": "submit"})
	if !resBool(res, "ok") {
		t.Fatalf("delete_function = %v", res)
	}
	res = invokeTool(t, tools["list_functions"], nil)
	if strings.Contains(fmt.Sprint(res["functions"]), "submit") {
		t.Fatalf("function still listed after delete: %v", res)
	}
}

func TestAgentTools_ReadAttachmentAndExample(t *testing.T) {
	tools, _, _ := newToolSet(t)

	// Attachment content comes back cat -n numbered, like read_file.
	res := invokeTool(t, tools["read_attachment"], map[string]any{"name": "notes.md"})
	if !strings.Contains(resStr(res, "content"), "# the brief") {
		t.Fatalf("read_attachment = %v", res)
	}
	res = invokeTool(t, tools["read_attachment"], map[string]any{"name": "missing.md"})
	if !strings.Contains(resStr(res, "error"), "available: notes.md") {
		t.Fatalf("unknown attachment should name the available set: %v", res)
	}

	res = invokeTool(t, tools["read_example"], map[string]any{"name": "hero"})
	if !strings.Contains(resStr(res, "content"), "Example Hero") {
		t.Fatalf("read_example = %v", res)
	}
	res = invokeTool(t, tools["read_example"], map[string]any{"name": "nope"})
	if !strings.Contains(resStr(res, "error"), "available: hero") {
		t.Fatalf("unknown example should name the available set: %v", res)
	}
}

func TestAgentTools_SearchDocs(t *testing.T) {
	tools, _, _ := newToolSet(t)

	// A real lookup returns daisyUI reference sections for the component.
	res := invokeTool(t, tools["search_docs"], map[string]any{"query": "badge sizes"})
	results := fmt.Sprint(res["results"])
	if results == "[]" || !strings.Contains(strings.ToLower(results), "badge") {
		t.Fatalf("search_docs(badge sizes) = %v", res)
	}

	// An empty query surfaces as an in-result error (the model recovers).
	res = invokeTool(t, tools["search_docs"], map[string]any{"query": ""})
	if !strings.Contains(resStr(res, "error"), "query is required") {
		t.Fatalf("empty query should error: %v", res)
	}
}

func mustStoreWrite(t *testing.T, s *store.Store, slug, path, content, ct string, meta map[string]string) {
	t.Helper()
	err := s.Write(context.Background(), slug, path, content, ct, meta)
	if err != nil {
		t.Fatalf("seed write %s: %v", path, err)
	}
}
