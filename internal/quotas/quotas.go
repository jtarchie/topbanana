// Package quotas is topbanana's per-user policy: how many sites someone may
// own and which LLM model each agent tier uses for them. It lives here rather
// than in internal/auth because none of it is identity — it is what this
// particular product does with an identity. auth carries it as opaque bytes
// on User.Meta and never looks inside.
package quotas

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/model"
)

// ErrMaxAppsReached is returned by CheckMaxApps when the user has
// already hit their cap. Wrapped so the build handler can branch on it
// (showing a friendly flash) rather than parsing strings.
var ErrMaxAppsReached = errors.New("max apps reached")

// ErrModelNotAllowed is returned by ResolveModel when the requested
// model isn't in the user's allowlist. Retained for any caller still on
// the legacy single-model API; the per-tier flow no longer rejects.
var ErrModelNotAllowed = errors.New("model not allowed")

// Defaults captures the platform-wide fallbacks applied when a user
// record's Quotas struct is zero-valued. Tiers maps each model tier to
// its operator-configured default (TierAuthor must be non-empty; the
// others fall back via TierMap.Resolve).
type Defaults struct {
	MaxApps int
	Tiers   model.TierMap
}

// CheckMaxApps gates new-app creation. Super admins bypass; regular
// admins are rejected when their current owned-app count is at or above
// their configured limit (or the system default when their limit is 0).
// currentCount is the slug count the caller has already computed —
// passed in rather than fetched here so handlers stay in control of the
// data flow.
func CheckMaxApps(u *auth.User, currentCount int, defaults Defaults) error {
	if u == nil {
		return errors.New("auth: missing user for quota check")
	}
	if u.Role == auth.RoleSuperAdmin {
		return nil
	}
	limit := Of(u).MaxApps
	if limit == 0 {
		limit = defaults.MaxApps
	}
	if limit <= 0 {
		// 0/negative system default means "unlimited" — useful for
		// dev / single-tenant deployments that opt into the auth stack
		// but don't want to enforce a cap yet.
		return nil
	}
	if currentCount >= limit {
		return fmt.Errorf("%w: %d/%d", ErrMaxAppsReached, currentCount, limit)
	}
	return nil
}

// ResolveTiers returns the effective per-tier model map for a user.
// User overrides layer on top of the platform defaults; empty user entries
// fall through to the default for that tier. Super admins behave the same:
// their per-tier overrides apply if they bothered to set any.
func ResolveTiers(u *auth.User, defaults Defaults) model.TierMap {
	if u == nil {
		return defaults.Tiers
	}
	return defaults.Tiers.Merge(Of(u).AllowedModels)
}

// ResolveModel is the legacy single-model resolver, retained as a thin
// shim during the transition to tier-based dispatch. New code should call
// ResolveTiers and pick the appropriate tier. The shim collapses everything
// to TierAuthor so existing callers continue to behave the way they did
// before tiers existed.
func ResolveModel(u *auth.User, requested string, defaults Defaults) (string, error) {
	tiers := ResolveTiers(u, defaults)
	if requested != "" {
		return requested, nil
	}
	return tiers.Resolve(model.TierAuthor), nil
}

// Quotas caps per-user resource usage. The zero value means "use the
// system defaults" (resolved by the quota check at enforcement time);
// RoleSuperAdmin bypasses all checks.
//
// AllowedModels carries per-tier model overrides — one model per agent
// lifecycle phase (Author/Editor/Utility/Vision). Empty entries fall
// through to the system default for that tier. The shape is a map so
// operators can override exactly the tiers they want without touching the
// rest; see model.TierMap for the fallback semantics.
//
// Legacy records on disk stored AllowedModels as a flat []string; the
// custom UnmarshalJSON below interprets element 0 as the Author override
// and drops the rest, so old user records keep loading without a one-shot
// migration.
type Quotas struct {
	// MaxApps is the hard cap on owned-app count. 0 = use system default.
	MaxApps int `json:"max_apps,omitempty"`
	// AllowedModels is the per-tier override map. Empty entries / missing
	// tiers fall through to QuotaDefaults at resolve time.
	AllowedModels model.TierMap `json:"allowed_models,omitempty"`
}

// UnmarshalJSON accepts either the new object form
// (`{"author":"X","editor":"Y"}`) or the legacy array form
// (`["openai/gpt-4-turbo"]`) for the allowed_models field. The legacy form
// projects element 0 into TierAuthor and drops the rest — the old code
// already treated `AllowedModels[0]` as the user's effective default, so
// no information is lost beyond unused list entries.
func (q *Quotas) UnmarshalJSON(data []byte) error {
	// Decode into a shape that's permissive about allowed_models. Use
	// json.RawMessage so we can dispatch on the underlying type.
	var raw struct {
		MaxApps       int             `json:"max_apps,omitempty"`
		AllowedModels json.RawMessage `json:"allowed_models,omitempty"`
	}
	err := json.Unmarshal(data, &raw)
	if err != nil {
		return fmt.Errorf("decode quotas: %w", err)
	}
	q.MaxApps = raw.MaxApps
	q.AllowedModels = nil

	trimmed := strings.TrimSpace(string(raw.AllowedModels))
	if trimmed == "" || trimmed == "null" {
		return nil
	}

	// Object form: parse straight into a TierMap.
	if trimmed[0] == '{' {
		var tm model.TierMap
		err = json.Unmarshal(raw.AllowedModels, &tm)
		if err != nil {
			return fmt.Errorf("decode allowed_models object: %w", err)
		}
		// Drop empty entries so the map stays canonical.
		for k, v := range tm {
			if v == "" {
				delete(tm, k)
			}
		}
		if len(tm) > 0 {
			q.AllowedModels = tm
		}
		return nil
	}

	// Legacy array form: element 0 becomes the Author override.
	if trimmed[0] == '[' {
		var list []string
		err = json.Unmarshal(raw.AllowedModels, &list)
		if err != nil {
			return fmt.Errorf("decode allowed_models array: %w", err)
		}
		for _, m := range list {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			q.AllowedModels = model.TierMap{model.TierAuthor: m}
			return nil
		}
		return nil
	}

	return fmt.Errorf("allowed_models: unexpected shape %q", trimmed[:1])
}

// Of decodes the quota policy carried on a user record. A user with no meta
// (or meta this application didn't write) yields the zero Quotas, which every
// resolver reads as "use the system defaults" — so an unreadable record
// degrades to default policy rather than to an error the caller must handle
// on a path that has nothing to do with quotas.
func Of(u *auth.User) Quotas {
	if u == nil || len(u.Meta) == 0 {
		return Quotas{}
	}
	q := Quotas{}
	err := json.Unmarshal(u.Meta, &q)
	if err != nil {
		return Quotas{}
	}
	return q
}

// OfInvite is Of for the invite that will seed a user record.
func OfInvite(inv *auth.Invite) Quotas {
	if inv == nil || len(inv.Meta) == 0 {
		return Quotas{}
	}
	q := Quotas{}
	err := json.Unmarshal(inv.Meta, &q)
	if err != nil {
		return Quotas{}
	}
	return q
}

// Encode marshals a policy for storage on User.Meta / Invite.Meta. The zero
// value encodes to nil so an unset policy leaves no field behind.
func Encode(q Quotas) (json.RawMessage, error) {
	// Not a == comparison: Quotas carries a map and isn't comparable.
	if q.MaxApps == 0 && len(q.AllowedModels) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, fmt.Errorf("quotas: encode: %w", err)
	}
	return body, nil
}

// Set writes the policy onto a user record in place.
func Set(u *auth.User, q Quotas) error {
	meta, err := Encode(q)
	if err != nil {
		return err
	}
	u.Meta = meta
	return nil
}
