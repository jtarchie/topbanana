// Package auth owns multi-tenant identity for buildabear: user records,
// passkey credentials, sessions, invites, role-based authorization, and
// per-user quotas. Records live in S3 under the reserved `_auth/` prefix so
// no new datastore is introduced.
package auth

import (
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

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

// User is the persistent identity for a single human in the system. Email
// is the canonical identifier (also the S3 key) — renames are unsupported
// by design. Credentials are stored inline; the passkey library appends to
// this slice via PutCredential after a successful registration.
//
// Implements both webauthn.User (required for the WebAuthn ceremony) and
// passkey.User (the egregors wrapper, which adds PutCredential).
type User struct {
	Email       string                `json:"email"`
	Name        string                `json:"name,omitempty"`
	Role        Role                  `json:"role"`
	Quotas      Quotas                `json:"quotas,omitempty"`
	Credentials []webauthn.Credential `json:"credentials,omitempty"`
	Created     time.Time             `json:"created"`
	Disabled    bool                  `json:"disabled,omitempty"`
}

// WebAuthnID is the user handle stored in the credential and returned in
// assertions. We use the email bytes so passkey login (discoverable
// credential, no username field) maps straight back to the user record.
func (u *User) WebAuthnID() []byte { return []byte(u.Email) }

// WebAuthnName is the username shown in the platform credential picker.
func (u *User) WebAuthnName() string { return u.Email }

// WebAuthnDisplayName falls back to the email when Name is empty so the
// credential picker always has something human-readable.
func (u *User) WebAuthnDisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// WebAuthnCredentials returns every passkey bound to this account.
func (u *User) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

// PutCredential is called by the passkey library after both successful
// registration (new credential) and successful login (sign-count update on
// an existing credential). We dedupe by credential ID so the same passkey
// touched at login time replaces its own sign-count entry instead of
// stacking.
func (u *User) PutCredential(c webauthn.Credential) {
	for i, existing := range u.Credentials {
		if string(existing.ID) == string(c.ID) {
			u.Credentials[i] = c
			return
		}
	}
	u.Credentials = append(u.Credentials, c)
}

// NormalizeEmail lowercases and trims whitespace so the same address keyed
// inconsistently by the user (mixed-case, leading space) always resolves
// to the same S3 record.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
