package model

import (
	"errors"
	"testing"
)

func TestTierMap_Resolve_FallbackToAuthor(t *testing.T) {
	t.Parallel()

	m := TierMap{
		TierAuthor: "openai/gpt-4o",
	}

	for _, tt := range []Tier{TierAuthor, TierEditor, TierUtility, TierVision} {
		if got := m.Resolve(tt); got != "openai/gpt-4o" {
			t.Errorf("Resolve(%q) = %q, want %q", tt, got, "openai/gpt-4o")
		}
	}
}

func TestTierMap_Resolve_PerTierOverrides(t *testing.T) {
	t.Parallel()

	m := TierMap{
		TierAuthor:  "openai/gpt-4o",
		TierEditor:  "openai/gpt-4o-mini",
		TierUtility: "openai/gpt-3.5-turbo",
		TierVision:  "openai/gpt-4-vision",
	}

	cases := map[Tier]string{
		TierAuthor:  "openai/gpt-4o",
		TierEditor:  "openai/gpt-4o-mini",
		TierUtility: "openai/gpt-3.5-turbo",
		TierVision:  "openai/gpt-4-vision",
	}
	for tier, want := range cases {
		if got := m.Resolve(tier); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", tier, got, want)
		}
	}
}

func TestTierMap_Resolve_EmptyEntryFallsBack(t *testing.T) {
	t.Parallel()

	m := TierMap{
		TierAuthor: "openai/gpt-4o",
		TierEditor: "", // explicitly empty
	}

	if got := m.Resolve(TierEditor); got != "openai/gpt-4o" {
		t.Errorf("Resolve(Editor) with empty entry = %q, want fallback to Author %q", got, "openai/gpt-4o")
	}
}

func TestTierMap_Validate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		m    TierMap
		err  error
	}{
		{name: "author set", m: TierMap{TierAuthor: "x"}, err: nil},
		{name: "all tiers set", m: TierMap{TierAuthor: "a", TierEditor: "e", TierUtility: "u", TierVision: "v"}, err: nil},
		{name: "author missing", m: TierMap{TierEditor: "e"}, err: ErrEmptyAuthorTier},
		{name: "empty map", m: TierMap{}, err: ErrEmptyAuthorTier},
		{name: "author empty string", m: TierMap{TierAuthor: ""}, err: ErrEmptyAuthorTier},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.m.Validate()
			if !errors.Is(err, tt.err) {
				t.Errorf("Validate() = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestTierMap_Merge_OverrideTakesPrecedence(t *testing.T) {
	t.Parallel()

	base := TierMap{
		TierAuthor:  "base-author",
		TierEditor:  "base-editor",
		TierUtility: "base-utility",
		TierVision:  "base-vision",
	}
	over := TierMap{
		TierEditor: "over-editor",
		TierVision: "over-vision",
	}

	got := base.Merge(over)

	want := TierMap{
		TierAuthor:  "base-author",
		TierEditor:  "over-editor",
		TierUtility: "base-utility",
		TierVision:  "over-vision",
	}
	for tier, w := range want {
		if g := got[tier]; g != w {
			t.Errorf("Merge[%q] = %q, want %q", tier, g, w)
		}
	}
}

func TestTierMap_Merge_EmptyOverrideDoesNotClobber(t *testing.T) {
	t.Parallel()

	base := TierMap{TierAuthor: "base-author", TierEditor: "base-editor"}
	over := TierMap{TierEditor: ""} // explicit empty

	got := base.Merge(over)

	if got[TierEditor] != "base-editor" {
		t.Errorf("Merge with empty override key clobbered base: got %q, want %q", got[TierEditor], "base-editor")
	}
}

func TestTierMap_Merge_NilBaseAndOver(t *testing.T) {
	t.Parallel()

	got := TierMap(nil).Merge(TierMap(nil))
	if len(got) != 0 {
		t.Errorf("Merge(nil, nil) = %v, want empty map", got)
	}
}

func TestTierMap_Merge_OnlyOverrideHasValue(t *testing.T) {
	t.Parallel()

	got := TierMap(nil).Merge(TierMap{TierAuthor: "x"})
	if got[TierAuthor] != "x" {
		t.Errorf("Merge(nil, {author:x})[author] = %q, want %q", got[TierAuthor], "x")
	}
}

func TestTierMap_String(t *testing.T) {
	t.Parallel()

	m := TierMap{
		TierAuthor: "big",
		TierEditor: "med",
	}
	got := m.String()
	// Author + Editor populated; Utility + Vision fall back.
	want := "TierMap{author=big editor=med utility=→author vision=→author}"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
