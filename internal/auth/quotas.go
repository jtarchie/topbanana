package auth

import (
	"errors"
	"fmt"
)

// ErrMaxAppsReached is returned by CheckMaxApps when the user has
// already hit their cap. Wrapped so the build handler can branch on it
// (showing a friendly flash) rather than parsing strings.
var ErrMaxAppsReached = errors.New("max apps reached")

// ErrModelNotAllowed is returned by ResolveModel when the requested
// model isn't in the user's allowlist.
var ErrModelNotAllowed = errors.New("model not allowed")

// QuotaDefaults captures the platform-wide fallbacks applied when a user
// record's Quotas struct is zero-valued. Wired from main.go CLI flags so
// operators can tune the bar without editing every user record.
type QuotaDefaults struct {
	MaxApps int
	Model   string
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

// ResolveModel validates that the user can run with the requested model
// and returns the effective model string. Empty AllowedModels means "use
// the system default" (which the caller passes in); a non-empty list
// rejects anything outside it. Super admins are bound by their own
// allowlist if they bothered to set one but bypass otherwise.
func ResolveModel(u *User, requested string, defaults QuotaDefaults) (string, error) {
	if u == nil {
		return "", errors.New("auth: missing user for model check")
	}
	if len(u.Quotas.AllowedModels) == 0 {
		if requested != "" {
			return requested, nil
		}
		return defaults.Model, nil
	}
	for _, m := range u.Quotas.AllowedModels {
		if m == requested {
			return requested, nil
		}
	}
	// Fall back to the first allowed model when the caller hasn't asked
	// for one specifically — keeps the build flow working when the form
	// doesn't surface a model picker.
	if requested == "" {
		return u.Quotas.AllowedModels[0], nil
	}
	return "", fmt.Errorf("%w: %q", ErrModelNotAllowed, requested)
}
