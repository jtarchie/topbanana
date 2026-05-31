package auth

import (
	"errors"
	"fmt"

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

// QuotaDefaults captures the platform-wide fallbacks applied when a user
// record's Quotas struct is zero-valued. Tiers maps each model tier to
// its operator-configured default (TierAuthor must be non-empty; the
// others fall back via TierMap.Resolve).
type QuotaDefaults struct {
	MaxApps int
	Tiers   model.TierMap
}

// CheckMaxApps gates new-app creation. Super admins bypass; regular
// admins are rejected when their current owned-app count is at or above
// their configured limit (or the system default when their limit is 0).
// currentCount is the slug count the caller has already computed —
// passed in rather than fetched here so handlers stay in control of the
// data flow.
func CheckMaxApps(u *User, currentCount int, defaults QuotaDefaults) error {
	if u == nil {
		return errors.New("auth: missing user for quota check")
	}
	if u.Role == RoleSuperAdmin {
		return nil
	}
	limit := u.Quotas.MaxApps
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
func ResolveTiers(u *User, defaults QuotaDefaults) model.TierMap {
	if u == nil {
		return defaults.Tiers
	}
	return defaults.Tiers.Merge(u.Quotas.AllowedModels)
}

// ResolveModel is the legacy single-model resolver, retained as a thin
// shim during the transition to tier-based dispatch. New code should call
// ResolveTiers and pick the appropriate tier. The shim collapses everything
// to TierAuthor so existing callers continue to behave the way they did
// before tiers existed.
func ResolveModel(u *User, requested string, defaults QuotaDefaults) (string, error) {
	tiers := ResolveTiers(u, defaults)
	if requested != "" {
		return requested, nil
	}
	return tiers.Resolve(model.TierAuthor), nil
}
