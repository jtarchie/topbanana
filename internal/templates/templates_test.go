package templates

import (
	"sort"
	"strings"
	"testing"
)

func TestAll_DefaultFirstThenAlphabetical(t *testing.T) {
	t.Parallel()

	all := All()
	if len(all) == 0 {
		t.Fatal("All() returned no templates")
	}
	if all[0].ID != defaultID {
		t.Errorf("All()[0].ID = %q, want %q (default must be first)", all[0].ID, defaultID)
	}
	rest := all[1:]
	if !sort.SliceIsSorted(rest, func(i, j int) bool { return rest[i].ID < rest[j].ID }) {
		ids := make([]string, len(rest))
		for i, t := range rest {
			ids[i] = t.ID
		}
		t.Errorf("rest of All() not alphabetical: %v", ids)
	}
}

func TestGet_KnownReturnsThatTemplate(t *testing.T) {
	t.Parallel()

	for _, want := range All() {
		got := Get(want.ID)
		if got == nil {
			t.Errorf("Get(%q) returned nil", want.ID)
			continue
		}
		if got.ID != want.ID {
			t.Errorf("Get(%q).ID = %q", want.ID, got.ID)
		}
	}
}

func TestGet_UnknownFallsBackToDefault(t *testing.T) {
	t.Parallel()

	got := Get("does-not-exist-anywhere")
	if got == nil {
		t.Fatal("Get(unknown) returned nil")
	}
	if got.ID != defaultID {
		t.Errorf("Get(unknown).ID = %q, want %q", got.ID, defaultID)
	}
}

func TestGet_EmptyStringFallsBackToDefault(t *testing.T) {
	t.Parallel()

	got := Get("")
	if got == nil || got.ID != defaultID {
		t.Errorf("Get(\"\") = %+v, want default", got)
	}
}

// TestEveryTemplate_UserFacingContract is the contract test: every shipped
// template under sites/ must have the fields needed to render in the picker
// UI and feed the agent. New templates inherit this check for free.
func TestEveryTemplate_UserFacingContract(t *testing.T) {
	t.Parallel()

	for _, tmpl := range All() {
		tmpl := tmpl
		t.Run(tmpl.ID, func(t *testing.T) {
			t.Parallel()

			if strings.TrimSpace(tmpl.Label) == "" {
				t.Errorf("Label is empty")
			}
			if strings.TrimSpace(tmpl.Description) == "" {
				t.Errorf("Description is empty")
			}
			// All non-default templates exist precisely because they have a
			// specialised prompt — empty PromptAddendum on anything other than
			// "blank" is almost certainly a typo in the frontmatter close.
			if tmpl.ID != defaultID && strings.TrimSpace(tmpl.PromptAddendum) == "" {
				t.Errorf("non-default template has empty PromptAddendum (frontmatter likely unclosed)")
			}
		})
	}
}

// TestEveryTemplate_ChecksAreWellFormed makes sure any declared Check has the
// fields the lint loop needs. Malformed checks are silent at boot and only
// blow up mid-build.
func TestEveryTemplate_ChecksAreWellFormed(t *testing.T) {
	t.Parallel()

	for _, tmpl := range All() {
		for i, c := range tmpl.Checks {
			if strings.TrimSpace(c.File) == "" {
				t.Errorf("%s checks[%d]: File empty", tmpl.ID, i)
			}
			if len(c.MustContain) == 0 {
				t.Errorf("%s checks[%d]: MustContain empty", tmpl.ID, i)
			}
			for j, m := range c.MustContain {
				if strings.TrimSpace(m) == "" {
					t.Errorf("%s checks[%d].MustContain[%d]: blank", tmpl.ID, i, j)
				}
			}
			if strings.TrimSpace(c.Message) == "" {
				t.Errorf("%s checks[%d]: Message empty (lint feedback to LLM would be useless)", tmpl.ID, i)
			}
		}
	}
}

// TestEnablesFunctions_AtLeastOneTemplateShipsIt locks in the invariant that
// the dynamic-template surface is actually exercised by at least one shipped
// template — otherwise a regression in EnablesFunctions plumbing would be
// silent until a user picked the right preset.
func TestEnablesFunctions_AtLeastOneTemplateShipsIt(t *testing.T) {
	t.Parallel()

	for _, tmpl := range All() {
		if tmpl.EnablesFunctions {
			return
		}
	}
	t.Fatalf("no shipped template has EnablesFunctions=true; dynamic-template plumbing is untested in production")
}

// TestSkeletonContent_NonEmptyWhenPresent — when a template ships a
// skeleton, every file in it must have non-empty content. An empty file
// would seed the bucket with a zero-byte object and confuse the agent.
func TestSkeletonContent_NonEmptyWhenPresent(t *testing.T) {
	t.Parallel()

	for _, tmpl := range All() {
		for path, content := range tmpl.Skeleton {
			if content == "" {
				t.Errorf("%s skeleton %q is empty", tmpl.ID, path)
			}
		}
	}
}

func TestParseFrontmatter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		raw      string
		wantErr  bool
		wantBody string
		check    func(t *testing.T, m templateMeta)
	}{
		{
			name:     "no frontmatter passes through",
			raw:      "just a body",
			wantBody: "just a body",
		},
		{
			name: "well-formed frontmatter and body",
			raw:  "---\n{\"label\":\"L\",\"description\":\"D\"}\n---\nbody text\n",
			check: func(t *testing.T, m templateMeta) {
				if m.Label != "L" || m.Description != "D" {
					t.Errorf("meta = %+v", m)
				}
			},
			wantBody: "body text\n",
		},
		{
			name:     "trailing terminator with no body",
			raw:      "---\n{\"label\":\"L\"}\n---",
			wantBody: "",
		},
		{
			name:    "unclosed frontmatter errors",
			raw:     "---\n{\"label\":\"L\"}\nno close",
			wantErr: true,
		},
		{
			name:    "malformed JSON errors",
			raw:     "---\n{not json\n---\nbody\n",
			wantErr: true,
		},
		{
			name: "enables_functions flag round-trips",
			raw:  "---\n{\"label\":\"L\",\"enables_functions\":true}\n---\n",
			check: func(t *testing.T, m templateMeta) {
				if !m.EnablesFunctions {
					t.Errorf("EnablesFunctions = false, want true")
				}
			},
		},
		{
			name: "checks round-trip",
			raw:  "---\n{\"checks\":[{\"file\":\"index.html\",\"must_contain\":[\"<form\"],\"message\":\"x\"}]}\n---\n",
			check: func(t *testing.T, m templateMeta) {
				if len(m.Checks) != 1 || m.Checks[0].File != "index.html" {
					t.Errorf("checks = %+v", m.Checks)
				}
			},
		},
		{
			name: "crlf is normalised",
			raw:  "---\r\n{\"label\":\"L\"}\r\n---\r\nbody\r\n",
			check: func(t *testing.T, m templateMeta) {
				if m.Label != "L" {
					t.Errorf("meta = %+v", m)
				}
			},
			wantBody: "body\n",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			meta, body, err := parseFrontmatter(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil; meta=%+v body=%q", meta, body)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantBody != "" && body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
			if tc.check != nil {
				tc.check(t, meta)
			}
		})
	}
}

// TestSetupNotes_TrimmedNotEmptyTrails — SetupNotes ships verbatim to the
// manage page, so any trailing whitespace from the frontmatter would land
// in the rendered HTML. loadOne TrimSpaces it; verify.
func TestSetupNotes_Trimmed(t *testing.T) {
	t.Parallel()

	for _, tmpl := range All() {
		if tmpl.SetupNotes == "" {
			continue
		}
		if strings.TrimSpace(tmpl.SetupNotes) != tmpl.SetupNotes {
			t.Errorf("%s SetupNotes has surrounding whitespace: %q", tmpl.ID, tmpl.SetupNotes)
		}
	}
}
