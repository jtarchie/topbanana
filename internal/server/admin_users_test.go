package server

import (
	"testing"

	"github.com/jtarchie/topbanana/internal/model"
)

// makeFormLookup returns a func(string) string that matches values from a
// fixture map. Missing keys yield "" — mirrors echo.FormValue semantics.
func makeFormLookup(values map[string]string) func(string) string {
	return func(name string) string {
		return values[name]
	}
}

func TestParseTierForm_AllFourFieldsPopulated(t *testing.T) {
	t.Parallel()

	got := parseTierForm(makeFormLookup(map[string]string{
		"model_author":  "openai/gpt-4o",
		"model_editor":  "openai/gpt-4o-mini",
		"model_utility": "openai/gpt-3.5-turbo",
		"model_vision":  "openai/gpt-4-vision",
	}))

	want := model.TierMap{
		model.TierAuthor:  "openai/gpt-4o",
		model.TierEditor:  "openai/gpt-4o-mini",
		model.TierUtility: "openai/gpt-3.5-turbo",
		model.TierVision:  "openai/gpt-4-vision",
	}
	for tier, w := range want {
		if g := got[tier]; g != w {
			t.Errorf("[%q] = %q, want %q", tier, g, w)
		}
	}
}

func TestParseTierForm_EmptyFieldsDropped(t *testing.T) {
	t.Parallel()

	got := parseTierForm(makeFormLookup(map[string]string{
		"model_author":  "x",
		"model_editor":  "", // explicitly empty
		"model_utility": "",
		"model_vision":  "y",
	}))

	if _, ok := got[model.TierEditor]; ok {
		t.Errorf("empty editor field should be dropped, got %v", got)
	}
	if _, ok := got[model.TierUtility]; ok {
		t.Errorf("empty utility field should be dropped, got %v", got)
	}
	if got[model.TierAuthor] != "x" || got[model.TierVision] != "y" {
		t.Errorf("populated fields lost: %v", got)
	}
}

func TestParseTierForm_WhitespaceOnlyDropped(t *testing.T) {
	t.Parallel()

	got := parseTierForm(makeFormLookup(map[string]string{
		"model_author": "  \t  ",
		"model_editor": " openai/x ",
	}))

	if _, ok := got[model.TierAuthor]; ok {
		t.Errorf("whitespace-only author should be dropped, got %v", got)
	}
	if got[model.TierEditor] != "openai/x" {
		t.Errorf("editor should be trimmed: got %q", got[model.TierEditor])
	}
}

func TestParseTierForm_AllEmptyReturnsNil(t *testing.T) {
	t.Parallel()

	got := parseTierForm(makeFormLookup(map[string]string{}))

	if got != nil {
		t.Errorf("expected nil for all-empty form, got %v", got)
	}
}

// TestNormalizeEmailParam guards the :email path-param decoding. The quotas
// panel builds its action with JS encodeURIComponent, so '@' arrives as
// '%40'; the server-rendered disable/enable forms send '@' literally. Both
// must resolve to the same canonical address, or the user lookup 404s.
func TestNormalizeEmailParam(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"bradarchie%40gmail.com":  "bradarchie@gmail.com",  // encoded (quotas panel)
		"bradarchie@gmail.com":    "bradarchie@gmail.com",  // literal (disable/enable)
		"Brad.Archie%40Gmail.com": "brad.archie@gmail.com", // encoded + mixed case
		"brad%2Btag%40gmail.com":  "brad+tag@gmail.com",    // plus-addressed, fully encoded
		"brad+tag@gmail.com":      "brad+tag@gmail.com",    // plus stays literal (not a space)
		"":                        "",
	}
	for raw, want := range cases {
		if got := normalizeEmailParam(raw); got != want {
			t.Errorf("normalizeEmailParam(%q) = %q, want %q", raw, got, want)
		}
	}
}
