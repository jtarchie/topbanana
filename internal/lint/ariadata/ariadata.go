// Package ariadata is the WAI-ARIA 1.2 Roles Model as lookup tables; data only, so internal/lint owns every judgement.
package ariadata

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

//go:embed roles.json
var rolesJSON []byte

type Role struct {
	// Abstract roles exist only to be subclassed, so role="widget" is always an authoring error.
	Abstract bool
	// NameRequired is the spec's accessibleNameRequired: meaningless to a screen reader unnamed.
	NameRequired bool
	// NameFromContents means own text supplies the name, so <button>Save</button> needs no aria-label.
	NameFromContents bool
	// NameProhibited means aria-label here is dropped by AT rather than honored.
	NameProhibited bool
	// Props is already flattened upstream, so it includes the global attributes.
	Props      map[string]bool
	Prohibited map[string]bool
	// Required holds only attributes with no implicit default; see README.md.
	Required []string
	// RequiredContext is any-of: a listitem belongs in a list or a directory.
	RequiredContext []string
	// RequiredOwned is any-of, each inner slice one acceptable child-role combination.
	RequiredOwned [][]string
}

// props/requiredProps hold either a name or a [name, default] pair, hence RawMessage.
type rawRole struct {
	Abstract             bool              `json:"abstract"`
	AccessibleNameReq    bool              `json:"accessibleNameRequired"`
	NameFrom             []string          `json:"nameFrom"`
	Props                []json.RawMessage `json:"props"`
	ProhibitedProps      []string          `json:"prohibitedProps"`
	RequiredProps        []json.RawMessage `json:"requiredProps"`
	RequiredContextRole  []string          `json:"requiredContextRole"`
	RequiredOwnedElement [][]string        `json:"requiredOwnedElements"`
}

// Once rather than init so a test binary that never lints ARIA skips the parse.
var table = sync.OnceValue(parse)

type model struct {
	roles map[string]Role
	names []string
	attrs map[string]bool
	attrL []string
}

func parse() *model {
	var raw map[string]rawRole
	err := json.Unmarshal(rolesJSON, &raw)
	if err != nil {
		// Embedded from the repo, so this is a broken vendor step, not a runtime condition.
		panic(fmt.Sprintf("ariadata: roles.json: %v", err))
	}

	m := &model{
		roles: make(map[string]Role, len(raw)),
		attrs: map[string]bool{},
	}
	for name, r := range raw {
		role := Role{
			Abstract:         r.Abstract,
			NameRequired:     r.AccessibleNameReq,
			NameFromContents: contains(r.NameFrom, "contents"),
			NameProhibited:   contains(r.NameFrom, "prohibited"),
			Props:            make(map[string]bool, len(r.Props)),
			Prohibited:       make(map[string]bool, len(r.ProhibitedProps)),
			RequiredContext:  r.RequiredContextRole,
			RequiredOwned:    r.RequiredOwnedElement,
		}
		for _, p := range r.Props {
			if n, _ := attrName(p); n != "" {
				role.Props[n] = true
				m.attrs[n] = true
			}
		}
		for _, p := range r.ProhibitedProps {
			role.Prohibited[p] = true
			m.attrs[p] = true
		}
		for _, p := range r.RequiredProps {
			n, bare := attrName(p)
			if n == "" {
				continue
			}
			m.attrs[n] = true
			// A pair carries an implicit default, so the element is well-formed without it.
			if bare {
				role.Required = append(role.Required, n)
			}
		}
		sort.Strings(role.Required)
		m.roles[name] = role

		if !r.Abstract {
			m.names = append(m.names, name)
		}
	}

	// `none` is a synonym for `presentation` that ships empty upstream.
	if p, ok := m.roles["presentation"]; ok {
		m.roles["none"] = p
	}

	sort.Strings(m.names)
	for a := range m.attrs {
		m.attrL = append(m.attrL, a)
	}
	sort.Strings(m.attrL)
	return m
}

func attrName(msg json.RawMessage) (name string, bare bool) {
	var s string
	if json.Unmarshal(msg, &s) == nil {
		return s, true
	}
	var pair []string
	if json.Unmarshal(msg, &pair) == nil && len(pair) > 0 {
		return pair[0], false
	}
	return "", false
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// Case-sensitive because role attribute values are.
func Get(name string) (Role, bool) {
	r, ok := table().roles[name]
	return r, ok
}

func Concrete(name string) bool {
	r, ok := Get(name)
	return ok && !r.Abstract
}

func RoleNames() []string { return table().names }

// Derived from the roles table rather than hand-listed, so it tracks the same spec version.
func IsAttr(name string) bool { return table().attrs[name] }

func AttrNames() []string { return table().attrL }

// Only single edits are offered: a guess further away than that is noise in an error message.
func NearestAttr(name string) string {
	best, bestDist := "", 3
	for _, cand := range AttrNames() {
		if abs(len(cand)-len(name)) >= bestDist {
			continue
		}
		if d := editDistance(name, cand); d < bestDist {
			best, bestDist = cand, d
		}
	}
	return best
}

func NearestRole(name string) string {
	best, bestDist := "", 3
	lower := strings.ToLower(name)
	for _, cand := range RoleNames() {
		if abs(len(cand)-len(lower)) >= bestDist {
			continue
		}
		if d := editDistance(lower, cand); d < bestDist {
			best, bestDist = cand, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
