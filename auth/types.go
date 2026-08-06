// Package auth owns multi-tenant identity: user records, passkey
// credentials, sessions, invites, and role-based authorization. Records are
// keyed documents under the reserved `_auth/` prefix, persisted through
// blob.Blobs — no datastore of its own, and no knowledge of which one
// the application picked.
//
// Application policy (resource caps, model allowlists, anything else a
// specific product cares about) rides on User.Meta and is opaque here.
package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
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

// legacyMetaField is what Meta was called on records written before
// application policy was pushed out of this package: a `quotas` object holding
// whatever the application cared about. Records are lifted on read and rewrite
// themselves under the new name on the next save, so no migration pass is
// needed — the same intentional-compat-read approach the rest of the repo uses
// for renamed sidecars.
const legacyMetaField = "quotas"

// liftLegacyMeta returns the meta bytes to use, preferring the current field
// and falling back to the legacy one.
func liftLegacyMeta(data []byte, current json.RawMessage) (json.RawMessage, error) {
	if len(current) > 0 {
		return current, nil
	}
	var probe map[string]json.RawMessage
	err := json.Unmarshal(data, &probe)
	if err != nil {
		return nil, fmt.Errorf("decode record: %w", err)
	}
	legacy, ok := probe[legacyMetaField]
	if !ok || len(legacy) == 0 || string(legacy) == "null" {
		return nil, nil
	}
	return json.RawMessage(legacy), nil
}

// UnmarshalJSON decodes a user record, lifting a pre-rename `quotas` object
// into Meta. The alias type sheds the method set so this doesn't recurse.
func (u *User) UnmarshalJSON(data []byte) error {
	type alias User
	tmp := (*alias)(u)
	err := json.Unmarshal(data, tmp)
	if err != nil {
		return fmt.Errorf("decode user: %w", err)
	}
	meta, err := liftLegacyMeta(data, u.Meta)
	if err != nil {
		return err
	}
	u.Meta = meta
	return nil
}

// UnmarshalJSON mirrors User.UnmarshalJSON for invite records.
func (i *Invite) UnmarshalJSON(data []byte) error {
	type alias Invite
	tmp := (*alias)(i)
	err := json.Unmarshal(data, tmp)
	if err != nil {
		return fmt.Errorf("decode invite: %w", err)
	}
	meta, err := liftLegacyMeta(data, i.Meta)
	if err != nil {
		return err
	}
	i.Meta = meta
	return nil
}

// Invite is a one-shot token a super admin issues to onboard a new user.
// Persisted at _auth/invites/{Token}.json; the file's presence is its
// validity. Consumed (UsedBy set) records are kept briefly for audit but
// can't be reused.
type Invite struct {
	Token string `json:"token"`
	Email string `json:"email"`
	Role  Role   `json:"role"`
	// Meta rides along to the User record this invite creates; see User.Meta.
	Meta    json.RawMessage `json:"meta,omitempty"`
	Created time.Time       `json:"created"`
	Expires time.Time       `json:"expires"`
	UsedBy  string          `json:"used_by,omitempty"`
}

// User is the persistent identity for a single human in the system. Email
// is the canonical identifier (also the S3 key) — renames are unsupported
// by design. Credentials are stored inline; the passkey library appends to
// this slice via PutCredential after a successful registration.
//
// Implements both webauthn.User (required for the WebAuthn ceremony) and
// passkey.User (the egregors wrapper, which adds PutCredential).
type User struct {
	Email string `json:"email"`
	Name  string `json:"name,omitempty"`
	Role  Role   `json:"role"`
	// Meta is application-defined data carried verbatim on the record. This
	// package never looks inside it. It exists so a consumer can attach its
	// own policy — resource caps, feature flags, billing tier — without the
	// identity domain growing a concept of what any of that means, which is
	// exactly the coupling that kept this package pinned to one application.
	Meta        json.RawMessage       `json:"meta,omitempty"`
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
		if bytes.Equal(existing.ID, c.ID) {
			u.Credentials[i] = c
			return
		}
	}
	u.Credentials = append(u.Credentials, c)
}

// RemoveCredential drops the credential whose ID matches, reporting whether one
// was removed. Mirror of PutCredential, used by the self-service "remove
// passkey" action. It does NOT guard against removing the user's last
// credential — callers own that policy, so the method stays reusable.
func (u *User) RemoveCredential(id []byte) bool {
	for i, existing := range u.Credentials {
		if bytes.Equal(existing.ID, id) {
			u.Credentials = append(u.Credentials[:i], u.Credentials[i+1:]...)
			return true
		}
	}
	return false
}

// NormalizeEmail lowercases and trims whitespace so the same address keyed
// inconsistently by the user (mixed-case, leading space) always resolves
// to the same S3 record.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
