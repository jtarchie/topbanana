package model

import (
	"errors"
	"fmt"
	"strings"
)

// Tier names a phase of the agent lifecycle that can be served by its own
// model. The four tiers map onto Bloomhollow's call sites: TierAuthor for
// initial generation, TierEditor for mechanical fix-ups and surgical edits,
// TierUtility for one-shot summarisation, TierVision for multimodal alt-text.
//
// Any tier whose entry on a TierMap is empty falls back to TierAuthor at
// resolve time, so single-model operators (only --llm-model set) keep
// working untouched. Multi-tier operators set whichever tiers they want to
// split off.
type Tier string

const (
	TierAuthor  Tier = "author"
	TierEditor  Tier = "editor"
	TierUtility Tier = "utility"
	TierVision  Tier = "vision"
)

// AllTiers is the canonical iteration order: roughly descending capability
// expectation. Used by config validation and the system dashboard.
var AllTiers = []Tier{TierAuthor, TierEditor, TierUtility, TierVision}

// TierMap is the operator-configured tier → model assignment. An empty
// value for a tier means "fall back to TierAuthor". TierAuthor itself must
// always be set; Validate enforces that.
type TierMap map[Tier]string

// ErrEmptyAuthorTier is returned by Validate when TierAuthor has no
// configured model. The fallback chain dead-ends at Author, so an empty
// Author tier leaves Resolve nothing to return.
var ErrEmptyAuthorTier = errors.New("model: TierAuthor must be configured")

// Resolve returns the configured model for a tier, walking the fallback
// chain. Any empty tier falls through to TierAuthor.
func (m TierMap) Resolve(t Tier) string {
	if v, ok := m[t]; ok && v != "" {
		return v
	}
	return m[TierAuthor]
}

// Validate ensures TierAuthor is non-empty. The other tiers may be empty —
// they fall back to Author.
func (m TierMap) Validate() error {
	if m[TierAuthor] == "" {
		return ErrEmptyAuthorTier
	}
	return nil
}

// Merge returns a new TierMap with non-empty entries from over layered on
// top of base. Empty entries in over do NOT clobber base — they're treated
// as "no override". Used to apply per-user overrides on top of system
// defaults.
func (base TierMap) Merge(over TierMap) TierMap {
	out := make(TierMap, len(AllTiers))
	for _, t := range AllTiers {
		if v := over[t]; v != "" {
			out[t] = v
			continue
		}
		if v := base[t]; v != "" {
			out[t] = v
		}
	}
	return out
}

// String renders the resolved tier map in a stable order, useful for boot
// logs and the system dashboard. Tiers that fall back to Author render as
// "→author" so operators can see at a glance which tiers are split out.
func (m TierMap) String() string {
	if len(m) == 0 {
		return "TierMap{}"
	}
	var b strings.Builder
	b.WriteString("TierMap{")
	for i, t := range AllTiers {
		if i > 0 {
			b.WriteString(" ")
		}
		v, ok := m[t]
		if !ok || v == "" {
			fmt.Fprintf(&b, "%s=→author", t)
			continue
		}
		fmt.Fprintf(&b, "%s=%s", t, v)
	}
	b.WriteString("}")
	return b.String()
}
