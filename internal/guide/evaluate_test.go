package guide_test

import (
	"context"
	"testing"

	"github.com/jtarchie/topbanana/internal/guide"
	"github.com/jtarchie/topbanana/internal/storetest"
	"github.com/jtarchie/topbanana/internal/templates"
)

// boolptr is a helper for the optional *bool Required field.
func boolptr(b bool) *bool { return &b }

func TestEvaluate_MultiPageScopes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := storetest.New(t, 0)
	const slug = "guide-eval"

	// index: a form, a tap-to-call phone, and an Hours section with real text.
	index := `<!DOCTYPE html><html><head><title>t</title></head><body>
<h1>Joe's Diner</h1>
<h2>Hours</h2><p>Open Monday through Friday, nine in the morning until nine at night.</p>
<a href="tel:+15551234567">Call us</a>
<form action="/api/submit"><input name="email"></form>
</body></html>`
	// about: only an email link — no form, no phone.
	about := `<!DOCTYPE html><html><head><title>about</title></head><body>
<h1>About</h1><a href="mailto:hi@joes.example">Email us</a>
</body></html>`

	err := st.Write(ctx, slug, "index.html", index, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("write index: %v", err)
	}
	err = st.Write(ctx, slug, "about.html", about, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("write about: %v", err)
	}

	tmpl := &templates.SiteTemplate{
		ID: "test",
		Guide: []templates.GuideItem{
			{ID: "hours", Detector: "section_present", Params: templates.GuideParams{Keywords: []string{"hours"}}}, // any-page → index ✓
			{ID: "menu", Detector: "section_present", Params: templates.GuideParams{Keywords: []string{"menu"}}},   // any-page → absent
			{ID: "email", Detector: "email_link"},                                            // any-page → about ✓
			{ID: "phone_every", Detector: "tel_link", Scope: guide.ScopeEvery},               // every-page → about lacks it → ✗
			{ID: "form_index", Detector: "form", Scope: guide.ScopeFile, Page: "index.html"}, // specific-file → index ✓
		},
	}

	rep := guide.Evaluate(ctx, st, slug, tmpl)

	if rep.Total != 5 {
		t.Fatalf("Total = %d, want 5", rep.Total)
	}
	if rep.Present != 3 {
		t.Errorf("Present = %d, want 3", rep.Present)
	}
	want := map[string]bool{"hours": true, "menu": false, "email": true, "phone_every": false, "form_index": true}
	for _, r := range rep.Results {
		if r.Present != want[r.Item.ID] {
			t.Errorf("item %q present = %v, want %v", r.Item.ID, r.Present, want[r.Item.ID])
		}
		if r.WorkspaceURL == "" {
			t.Errorf("item %q has empty WorkspaceURL", r.Item.ID)
		}
	}
	if rep.Complete() {
		t.Error("Complete() = true, want false (menu + phone missing)")
	}
}

func TestEvaluate_SpecificFileDefaultsToIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := storetest.New(t, 0)
	const slug = "guide-defaultpage"
	err := st.Write(ctx, slug, "index.html", `<form><input name="x"></form>`, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	tmpl := &templates.SiteTemplate{
		Guide: []templates.GuideItem{{ID: "form", Detector: "form", Scope: guide.ScopeFile}}, // no Page → defaults to index.html
	}
	rep := guide.Evaluate(ctx, st, slug, tmpl)
	if rep.Total != 1 || rep.Present != 1 {
		t.Fatalf("got %d/%d, want 1/1", rep.Present, rep.Total)
	}
	if rep.Results[0].WorkspaceURL != "/workspace/"+slug+"?page=index.html" {
		t.Errorf("WorkspaceURL = %q", rep.Results[0].WorkspaceURL)
	}
}

func TestEvaluate_EmptyAndNil(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	st := storetest.New(t, 0)

	if rep := guide.Evaluate(ctx, st, "nope", nil); rep.Total != 0 {
		t.Errorf("nil template: Total = %d, want 0", rep.Total)
	}
	empty := &templates.SiteTemplate{Guide: nil}
	if rep := guide.Evaluate(ctx, st, "nope", empty); rep.Total != 0 {
		t.Errorf("no guide items: Total = %d, want 0", rep.Total)
	}
	// A slug with no files at all still returns a Report (items all absent),
	// never an error/panic.
	one := &templates.SiteTemplate{Guide: []templates.GuideItem{{ID: "form", Detector: "form", Required: boolptr(true)}}}
	rep := guide.Evaluate(ctx, st, "empty-site", one)
	if rep.Total != 1 || rep.Present != 0 {
		t.Errorf("empty site: got %d/%d, want 0/1", rep.Present, rep.Total)
	}
}
