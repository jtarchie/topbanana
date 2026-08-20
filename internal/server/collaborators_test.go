package server

import (
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
)

// TestNormalizeCollaborators covers the three things the canonical form has to
// guarantee before it reaches the sidecar: case/whitespace folding, dedupe,
// and that the owner can never appear in their own collaborator list.
func TestNormalizeCollaborators(t *testing.T) {
	cases := []struct {
		name  string
		in    []string
		owner string
		want  []string
	}{
		{"empty is nil", nil, "owner@test", nil},
		{"blanks dropped", []string{"", "   "}, "owner@test", nil},
		{"normalizes case", []string{"Bob@Test"}, "owner@test", []string{"bob@test"}},
		{"dedupes", []string{"bob@test", "BOB@test"}, "owner@test", []string{"bob@test"}},
		{"drops owner", []string{"Owner@Test", "bob@test"}, "owner@test", []string{"bob@test"}},
		{"owner only is nil", []string{"owner@test"}, "owner@test", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeCollaborators(tc.in, tc.owner)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v want %v", got, tc.want)
				}
			}
		})
	}
}

// TestMetaGrantsAccess pins the sidecar-side predicate, including the case
// that matters most: an empty email must not match a pre-migration site whose
// OwnerID was never assigned.
func TestMetaGrantsAccess(t *testing.T) {
	meta := build.SiteMeta{OwnerID: "owner@test", Collaborators: []string{"bob@test"}}
	if !metaGrantsAccess(meta, "owner@test") {
		t.Error("owner denied")
	}
	if !metaGrantsAccess(meta, "bob@test") {
		t.Error("collaborator denied")
	}
	if metaGrantsAccess(meta, "stranger@test") {
		t.Error("stranger allowed")
	}
	if metaGrantsAccess(build.SiteMeta{}, "") {
		t.Error("empty email matched an unowned site")
	}
}

// TestRegistryCanAccess exercises the in-memory gate every request path
// consults, plus the two invariants that keep collaboration from leaking into
// ownership: quota counts owners only, and clearing the list revokes.
func TestRegistryCanAccess(t *testing.T) {
	r := &siteRegistry{
		ownerIndex:  map[string]string{"site-a": "owner@test", "site-b": "other@test"},
		collabIndex: map[string][]string{"site-b": {"owner@test", "bob@test"}},
	}

	if !r.canAccess("site-a", "owner@test") {
		t.Error("owner denied on owned site")
	}
	if !r.canAccess("site-b", "bob@test") {
		t.Error("collaborator denied")
	}
	if r.canAccess("site-a", "bob@test") {
		t.Error("collaborator on site-b reached site-a")
	}
	if r.canAccess("site-a", "") {
		t.Error("empty email granted access")
	}

	// Quota is an ownership concept: owner@test owns one site and
	// collaborates on another, and only the owned one counts.
	if got := r.countAppsFor("owner@test"); got != 1 {
		t.Errorf("countAppsFor: got %d want 1 (collaborations must not count)", got)
	}

	r.setCollaborators("site-b", nil)
	if r.canAccess("site-b", "bob@test") {
		t.Error("access survived an emptied collaborator list")
	}
	if _, still := r.collabIndex["site-b"]; still {
		t.Error("emptied list left an entry behind")
	}
}

// TestRegistrySetCollaboratorsCopies guards against the caller's slice
// aliasing the index: a later append by the caller must not mutate what
// readers see.
func TestRegistrySetCollaborators_Copies(t *testing.T) {
	r := &siteRegistry{ownerIndex: map[string]string{}, collabIndex: map[string][]string{}}
	list := []string{"bob@test"}
	r.setCollaborators("site", list)
	list[0] = "attacker@test"
	if r.canAccess("site", "attacker@test") {
		t.Error("mutating the caller's slice changed the index")
	}
	if !r.canAccess("site", "bob@test") {
		t.Error("stored grant was clobbered")
	}
}
