package docs

import (
	"strings"
	"testing"
)

// top runs a search against the real embedded corpus and returns the
// lowercased top breadcrumb plus the full result set.
func top(t *testing.T, query string) (string, []Result) {
	t.Helper()
	res := Search(query, Options{})
	if len(res) == 0 {
		t.Fatalf("no results for %q", query)
	}
	return strings.ToLower(res[0].Breadcrumb), res
}

func TestSearch_GoldenQueries(t *testing.T) {
	if bc, _ := top(t, "badge"); !strings.Contains(bc, "badge") {
		t.Errorf("'badge' top = %q, want a badge section", bc)
	}

	pbBc, pb := top(t, "primary button")
	if !strings.Contains(pbBc, "button") {
		t.Errorf("'primary button' top = %q, want the button section", pbBc)
	}
	if !strings.Contains(pb[0].Body, "btn-primary") {
		t.Errorf("'primary button' button body lacks btn-primary: %q", pb[0].Body)
	}

	// btn-primary is mentioned more often in the fab examples than in the button
	// section that *defines* it; the component-class bonus must still win.
	if bc, _ := top(t, "btn-primary"); !strings.Contains(bc, "button") {
		t.Errorf("'btn-primary' top = %q, want the button section", bc)
	}

	if bc, _ := top(t, "theme colors"); !strings.Contains(bc, "color") && !strings.Contains(bc, "theme") {
		t.Errorf("'theme colors' top = %q, want a color/theme section", bc)
	}

	if bc, _ := top(t, "timeline horizontal"); !strings.Contains(bc, "timeline") {
		t.Errorf("'timeline horizontal' top = %q, want the timeline section", bc)
	}
}

func TestSearch_HeadingBeatsProseMention(t *testing.T) {
	// "tooltip" must return the tooltip component, not fab (which uses tooltips
	// in its examples).
	if bc, _ := top(t, "tooltip"); !strings.HasSuffix(bc, "tooltip") {
		t.Errorf("'tooltip' top = %q, want the tooltip section", bc)
	}
}

func TestSearch_Deterministic(t *testing.T) {
	a := Search("card actions", Options{})
	b := Search("card actions", Options{})
	if len(a) == 0 {
		t.Fatal("no results")
	}
	if len(a) != len(b) {
		t.Fatalf("len mismatch %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("result %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestSearch_CapsAndBudget(t *testing.T) {
	res := Search("button", Options{MaxResults: 100})
	if len(res) > HardMaxResults {
		t.Errorf("MaxResults not clamped: got %d > %d", len(res), HardMaxResults)
	}
	total := 0
	for _, r := range res {
		if len(r.Body) > DefaultChunkBytes {
			t.Errorf("body exceeds per-chunk cap: %d > %d", len(r.Body), DefaultChunkBytes)
		}
		total += len(r.Body)
	}
	if total > DefaultTotalBytes {
		t.Errorf("total body %d exceeds budget %d", total, DefaultTotalBytes)
	}
}

func TestSearch_EmptyAndUnknown(t *testing.T) {
	if r := Search("", Options{}); r != nil {
		t.Errorf("empty query should return nil, got %v", r)
	}
	if r := Search("   !!!  ", Options{}); r != nil {
		t.Errorf("punctuation-only query should return nil, got %v", r)
	}
	if r := Search("zzqqxxnonsense", Options{}); len(r) != 0 {
		t.Errorf("nonsense query should return no results, got %v", r)
	}
}

func TestSearch_SourceFilter(t *testing.T) {
	if len(Search("button", Options{})) == 0 {
		t.Fatal("expected daisyui results")
	}
	for _, r := range Search("button", Options{Source: "daisyui"}) {
		if r.Source != "daisyUI" {
			t.Errorf("source filter leaked %q", r.Source)
		}
	}
	if r := Search("button", Options{Source: "tailwind"}); len(r) != 0 {
		t.Errorf("unknown source should return nothing, got %d", len(r))
	}
}

func TestTokenize_ClassNames(t *testing.T) {
	got := tokenize("btn-primary")
	for _, want := range []string{"btn", "primary", "btn-primary"} {
		if !containsTok(got, want) {
			t.Errorf("tokenize(btn-primary) = %v, missing %q", got, want)
		}
	}
	// A plain word emits only itself (no spurious joined form).
	if got := tokenize("button"); len(got) != 1 || got[0] != "button" {
		t.Errorf("tokenize(button) = %v, want [button]", got)
	}
}
