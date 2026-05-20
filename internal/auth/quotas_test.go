package auth

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/jtarchie/bloomhollow/internal/model"
)

func TestQuotas_UnmarshalJSON_LegacyArrayForm(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"max_apps": 7, "allowed_models": ["openai/gpt-4-turbo", "openai/gpt-4o"]}`)

	var q Quotas
	err := json.Unmarshal(raw, &q)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if q.MaxApps != 7 {
		t.Errorf("MaxApps = %d, want 7", q.MaxApps)
	}
	if got := q.AllowedModels[model.TierAuthor]; got != "openai/gpt-4-turbo" {
		t.Errorf("AllowedModels[Author] = %q, want %q", got, "openai/gpt-4-turbo")
	}
	if len(q.AllowedModels) != 1 {
		t.Errorf("AllowedModels has %d entries, want 1 (legacy array dropped extras)", len(q.AllowedModels))
	}
}

func TestQuotas_UnmarshalJSON_LegacyArrayEmpty(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"allowed_models": []}`)

	var q Quotas
	err := json.Unmarshal(raw, &q)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if q.AllowedModels != nil {
		t.Errorf("AllowedModels = %v, want nil for empty array", q.AllowedModels)
	}
}

func TestQuotas_UnmarshalJSON_ObjectForm(t *testing.T) {
	t.Parallel()

	raw := []byte(`{
		"max_apps": 3,
		"allowed_models": {
			"author":  "openai/gpt-4o",
			"editor":  "openai/gpt-4o-mini",
			"vision":  "openai/gpt-4-vision"
		}
	}`)

	var q Quotas
	err := json.Unmarshal(raw, &q)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := q.AllowedModels[model.TierAuthor]; got != "openai/gpt-4o" {
		t.Errorf("AllowedModels[Author] = %q", got)
	}
	if got := q.AllowedModels[model.TierEditor]; got != "openai/gpt-4o-mini" {
		t.Errorf("AllowedModels[Editor] = %q", got)
	}
	if _, ok := q.AllowedModels[model.TierUtility]; ok {
		t.Errorf("AllowedModels[Utility] should be absent")
	}
	if got := q.AllowedModels[model.TierVision]; got != "openai/gpt-4-vision" {
		t.Errorf("AllowedModels[Vision] = %q", got)
	}
}

func TestQuotas_UnmarshalJSON_ObjectFormEmptyValuesDropped(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"allowed_models": {"author": "x", "editor": ""}}`)

	var q Quotas
	err := json.Unmarshal(raw, &q)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := q.AllowedModels[model.TierEditor]; ok {
		t.Errorf("empty editor value should be dropped, got map %v", q.AllowedModels)
	}
}

func TestQuotas_UnmarshalJSON_NullAndAbsent(t *testing.T) {
	t.Parallel()

	cases := map[string][]byte{
		"absent":     []byte(`{}`),
		"null":       []byte(`{"allowed_models": null}`),
		"empty obj":  []byte(`{"allowed_models": {}}`),
		"empty list": []byte(`{"allowed_models": []}`),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var q Quotas
			err := json.Unmarshal(raw, &q)
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if q.AllowedModels != nil {
				t.Errorf("AllowedModels = %v, want nil", q.AllowedModels)
			}
		})
	}
}

func TestQuotas_UnmarshalJSON_BadShape(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"allowed_models": "not-a-map-or-list"}`)
	var q Quotas
	err := json.Unmarshal(raw, &q)
	if err == nil {
		t.Errorf("expected error for string allowed_models, got nil")
	}
}

func TestQuotas_RoundTrip_ObjectForm(t *testing.T) {
	t.Parallel()

	in := Quotas{
		MaxApps: 5,
		AllowedModels: model.TierMap{
			model.TierAuthor: "openai/gpt-4o",
			model.TierVision: "openai/gpt-4-vision",
		},
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out Quotas
	err = json.Unmarshal(body, &out)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.MaxApps != in.MaxApps {
		t.Errorf("MaxApps lost in round-trip: %d", out.MaxApps)
	}
	if out.AllowedModels[model.TierAuthor] != "openai/gpt-4o" {
		t.Errorf("Author lost in round-trip: %q", out.AllowedModels[model.TierAuthor])
	}
	if out.AllowedModels[model.TierVision] != "openai/gpt-4-vision" {
		t.Errorf("Vision lost in round-trip: %q", out.AllowedModels[model.TierVision])
	}
}

func TestResolveTiers_NoUserReturnsDefaults(t *testing.T) {
	t.Parallel()

	defaults := QuotaDefaults{
		Tiers: model.TierMap{model.TierAuthor: "big", model.TierEditor: "med"},
	}
	got := ResolveTiers(nil, defaults)
	if got.Resolve(model.TierAuthor) != "big" || got.Resolve(model.TierEditor) != "med" {
		t.Errorf("ResolveTiers(nil) = %v, want defaults", got)
	}
}

func TestResolveTiers_UserOverridesMergeOnTopOfDefaults(t *testing.T) {
	t.Parallel()

	defaults := QuotaDefaults{
		Tiers: model.TierMap{
			model.TierAuthor:  "default-author",
			model.TierEditor:  "default-editor",
			model.TierUtility: "default-utility",
			model.TierVision:  "default-vision",
		},
	}
	u := &User{
		Quotas: Quotas{
			AllowedModels: model.TierMap{
				model.TierEditor: "user-editor",
				model.TierVision: "user-vision",
			},
		},
	}

	got := ResolveTiers(u, defaults)

	cases := map[model.Tier]string{
		model.TierAuthor:  "default-author",
		model.TierEditor:  "user-editor",
		model.TierUtility: "default-utility",
		model.TierVision:  "user-vision",
	}
	for tier, want := range cases {
		if g := got.Resolve(tier); g != want {
			t.Errorf("ResolveTiers[%q] = %q, want %q", tier, g, want)
		}
	}
}

func TestResolveModel_LegacyShim(t *testing.T) {
	t.Parallel()

	defaults := QuotaDefaults{Tiers: model.TierMap{model.TierAuthor: "system"}}
	u := &User{Quotas: Quotas{AllowedModels: model.TierMap{model.TierAuthor: "user-author"}}}

	// Requested empty: user's Author override wins.
	got, err := ResolveModel(u, "", defaults)
	if err != nil || got != "user-author" {
		t.Errorf("ResolveModel(empty) = (%q, %v), want (user-author, nil)", got, err)
	}

	// Requested non-empty: returned as-is (no allowlist enforcement anymore).
	got, err = ResolveModel(u, "openai/gpt-4o", defaults)
	if err != nil || got != "openai/gpt-4o" {
		t.Errorf("ResolveModel(requested) = (%q, %v), want (openai/gpt-4o, nil)", got, err)
	}
}

func TestResolveModel_NilUserErrors(t *testing.T) {
	t.Parallel()

	// nil user still yields a string from defaults via the shim — no error,
	// matching the pre-tier behaviour for the empty-quota path.
	defaults := QuotaDefaults{Tiers: model.TierMap{model.TierAuthor: "system"}}
	got, err := ResolveModel(nil, "", defaults)
	if err != nil || got != "system" {
		t.Errorf("ResolveModel(nil) = (%q, %v), want (system, nil)", got, err)
	}
}

func TestErrModelNotAllowed_StillExported(t *testing.T) {
	t.Parallel()

	// Sanity: the symbol is retained for any external import that compiles
	// against it during the migration window. Not produced by the shim, but
	// still a valid sentinel value.
	if errors.Is(nil, ErrModelNotAllowed) {
		t.Errorf("nil should not match ErrModelNotAllowed")
	}
}
