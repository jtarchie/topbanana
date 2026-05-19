// Package auth owns multi-tenant identity for buildabear: user records,
// passkey credentials, sessions, invites, role-based authorization, and
// per-user quotas. Records live in S3 under the reserved `_auth/` prefix so
// no new datastore is introduced.
//
// This file is the skeleton landed in commit 1 of the multi-tenancy rollout.
// It only defines the value types that downstream pieces (S3 stores, the
// egregors/passkey integration, middleware) will reference. The User struct
// and store implementations land in commit 2 alongside the webauthn library.
package auth

import "time"

// Role gates which routes a user can hit. RoleSuperAdmin sees every app and
// can manage users; RoleAdmin only sees apps they own.
type Role string

const (
	RoleSuperAdmin Role = "super_admin"
	RoleAdmin      Role = "admin"
)

// Quotas caps per-user resource usage. The zero value means "use the
// system defaults" (resolved by the quota check at enforcement time);
// RoleSuperAdmin bypasses all checks.
type Quotas struct {
	// MaxApps is the hard cap on owned-app count. 0 = use system default.
	MaxApps int `json:"max_apps,omitempty"`
	// AllowedModels is an explicit allowlist of LLM model IDs the user may
	// drive the agent with. Empty = use whatever the server default is.
	AllowedModels []string `json:"allowed_models,omitempty"`
}

// Invite is a one-shot token a super admin issues to onboard a new user.
// Persisted at _auth/invites/{Token}.json; the file's presence is its
// validity. Consumed (UsedBy set) records are kept briefly for audit but
// can't be reused.
type Invite struct {
	Token   string    `json:"token"`
	Email   string    `json:"email"`
	Role    Role      `json:"role"`
	Quotas  Quotas    `json:"quotas"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	UsedBy  string    `json:"used_by,omitempty"`
}
