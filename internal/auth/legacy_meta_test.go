package auth

import (
	"encoding/json"
	"testing"
)

// Records written before application policy moved out of this package stored it
// in a `quotas` object. Those records are already in the bucket, so decoding
// must keep working without a migration pass — the same
// intentional-legacy-compat-read approach the rest of the repo uses for renamed
// sidecars. These pin that, and pin that a record carrying the current field
// is never overwritten by the old one.

func TestUser_LiftsLegacyQuotasIntoMeta(t *testing.T) {
	t.Parallel()

	legacy := []byte(`{"email":"a@example.com","role":"admin","quotas":{"max_apps":7}}`)

	u := &User{}
	err := json.Unmarshal(legacy, u)
	if err != nil {
		t.Fatalf("decode legacy user: %v", err)
	}
	if u.Email != "a@example.com" || u.Role != RoleAdmin {
		t.Fatalf("ordinary fields lost: %+v", u)
	}
	if string(u.Meta) != `{"max_apps":7}` {
		t.Fatalf("Meta = %s, want the legacy quotas object lifted verbatim", u.Meta)
	}
}

func TestInvite_LiftsLegacyQuotasIntoMeta(t *testing.T) {
	t.Parallel()

	legacy := []byte(`{"token":"t","email":"a@example.com","role":"admin","quotas":{"max_apps":3}}`)

	inv := &Invite{}
	err := json.Unmarshal(legacy, inv)
	if err != nil {
		t.Fatalf("decode legacy invite: %v", err)
	}
	if string(inv.Meta) != `{"max_apps":3}` {
		t.Fatalf("Meta = %s, want the legacy quotas object lifted verbatim", inv.Meta)
	}
}

// A record carrying both fields is one written after the rename by a consumer
// that also kept a stale `quotas` key around. The current field wins — the
// legacy read is a fallback, never an override.
func TestUser_CurrentMetaBeatsLegacyQuotas(t *testing.T) {
	t.Parallel()

	both := []byte(`{"email":"a@example.com","meta":{"max_apps":1},"quotas":{"max_apps":99}}`)

	u := &User{}
	err := json.Unmarshal(both, u)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(u.Meta) != `{"max_apps":1}` {
		t.Fatalf("Meta = %s, want the current field to win", u.Meta)
	}
}

// A record with neither field decodes to nil Meta rather than an error: no
// policy attached is an ordinary state, not a fault.
func TestUser_NoMetaIsNotAnError(t *testing.T) {
	t.Parallel()

	u := &User{}
	err := json.Unmarshal([]byte(`{"email":"a@example.com","role":"admin"}`), u)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if u.Meta != nil {
		t.Fatalf("Meta = %s, want nil", u.Meta)
	}
}

// Round trip: a record written today decodes to the same Meta bytes.
func TestUser_MetaRoundTrips(t *testing.T) {
	t.Parallel()

	in := &User{Email: "a@example.com", Role: RoleAdmin, Meta: json.RawMessage(`{"anything":["at","all"]}`)}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	out := &User{}
	err = json.Unmarshal(body, out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(out.Meta) != string(in.Meta) {
		t.Fatalf("Meta = %s, want %s", out.Meta, in.Meta)
	}
}
