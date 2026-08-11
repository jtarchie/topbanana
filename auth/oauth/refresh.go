package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jtarchie/topbanana/auth/blob"
)

// Refresh tokens exist because the access token is a self-contained JWT that
// nothing can revoke, so it has to be short-lived — and without a way to renew
// it, "short-lived" means the user reconnects by hand every twelve hours. That
// is the friction this file removes.
//
// They are opaque and stored, not signed and stateless, precisely so they can
// be revoked and their reuse detected. That is the trade: the access token is
// cheap to verify and impossible to withdraw; the refresh token costs a read
// and can be taken away.
//
// # Rotation and reuse detection
//
// OAuth 2.1 requires refresh tokens issued to public clients to rotate, and a
// rotated token to be treated as compromised if it shows up again. Every
// refresh mints a new token and consumes the old one; a chain of them shares a
// Family. Presenting an already-consumed token means either a replay or a leak
// — in both cases the holder is not the only one with it — so the entire
// family is revoked rather than just that token. The legitimate client loses
// its session and re-authorizes, which is the correct outcome when a
// credential is known to have escaped.

// refreshRecord is one link in a rotation chain.
type refreshRecord struct {
	Email    string `json:"email"`
	ClientID string `json:"client_id"`
	// Family is stable across a chain of rotations and is what reuse
	// detection revokes: an attacker replaying any link invalidates all of it.
	Family  string    `json:"family"`
	Expires time.Time `json:"expires"`
	// Consumed marks a token that has already been rotated away. The record
	// deliberately outlives its usefulness — a deleted token is
	// indistinguishable from one that never existed, and that difference is
	// the entire signal reuse detection runs on.
	Consumed bool `json:"consumed,omitempty"`
}

func (st *mcpOAuthState) refreshPrefix() string { return st.prefix + "refresh/" }

func (st *mcpOAuthState) refreshKey(token string) string {
	return st.refreshPrefix() + token + ".json"
}

// issueRefresh mints a token in the given family, or starts a new family when
// family is empty.
func (st *mcpOAuthState) issueRefresh(ctx context.Context, email, clientID, family string) (string, error) {
	if family == "" {
		family = mcpRandomToken()
	}
	token := mcpRandomToken()
	body, err := json.Marshal(refreshRecord{
		Email:    email,
		ClientID: clientID,
		Family:   family,
		Expires:  time.Now().Add(refreshTTL),
	})
	if err != nil {
		return "", fmt.Errorf("oauth: marshal refresh token: %w", err)
	}
	err = st.store.Put(ctx, st.refreshKey(token), string(body))
	if err != nil {
		return "", fmt.Errorf("oauth: write refresh token: %w", err)
	}
	return token, nil
}

// errRefreshReused reports that a consumed token was presented again. Kept
// distinct from a plain miss so the caller can revoke the family and log it —
// this is the one refresh failure that means something is wrong rather than
// merely expired.
var errRefreshReused = errors.New("oauth: refresh token reused")

// redeemRefresh consumes a refresh token and returns the identity it carries,
// claiming it with a compare-and-set so two concurrent refreshes cannot both
// win. ok=false with a nil error means "no such token, or expired, or the
// wrong client" — all invalid_grant, none of them distinguishable to a caller
// on purpose.
func (st *mcpOAuthState) redeemRefresh(ctx context.Context, token, clientID string) (refreshRecord, bool, error) {
	if !mcpTokenPattern.MatchString(token) {
		return refreshRecord{}, false, nil
	}
	obj, err := st.store.Get(ctx, st.refreshKey(token))
	if err != nil {
		return refreshRecord{}, false, fmt.Errorf("oauth: read refresh token: %w", err)
	}
	if obj.Content == "" {
		return refreshRecord{}, false, nil
	}
	rec := refreshRecord{}
	err = json.Unmarshal([]byte(obj.Content), &rec)
	if err != nil {
		slog.Warn("mcp.oauth.refresh_parse_failed", "err", err)
		return refreshRecord{}, false, nil
	}

	// Reuse: this token was already rotated away, so whoever just presented it
	// is working from a copy. Revoke the whole chain — including the link the
	// legitimate client currently holds, which is the point.
	if rec.Consumed {
		return rec, false, errRefreshReused
	}
	if rec.ClientID != clientID || time.Now().After(rec.Expires) {
		return refreshRecord{}, false, nil
	}

	rec.Consumed = true
	consumed, err := json.Marshal(rec)
	if err != nil {
		return refreshRecord{}, false, fmt.Errorf("oauth: marshal consumed refresh token: %w", err)
	}
	err = st.store.PutIfMatch(ctx, st.refreshKey(token), string(consumed), obj.ETag)
	if errors.Is(err, blob.ErrPrecondition) {
		// Another refresh claimed it first. Refusing is right: exactly one
		// caller may rotate a given link.
		return refreshRecord{}, false, nil
	}
	if err != nil {
		return refreshRecord{}, false, fmt.Errorf("oauth: claim refresh token: %w", err)
	}
	return rec, true, nil
}

// revokeFamily deletes every token in a rotation chain. Called on reuse
// detection, where the chain is known to have leaked.
//
// Returns an error rather than swallowing one: this is a security operation,
// and a caller that reports "revoked" after a failed store call has told the
// operator something untrue about who still has access.
func (st *mcpOAuthState) revokeFamily(ctx context.Context, family string) error {
	if family == "" {
		// Every record issueRefresh writes carries a family, so an empty one
		// can only come from a malformed record — and matching on it would
		// delete every other malformed record along with it. Refuse rather
		// than turn one bad blob into a mass delete.
		return errors.New("oauth: refusing to revoke an empty refresh family")
	}
	return st.deleteMatching(ctx, func(rec refreshRecord) bool { return rec.Family == family })
}

// revokeClientRefresh deletes every refresh token issued to a client. Without
// this, revoking a registration would not actually cut off access: the client
// keeps a live rotation chain and renews indefinitely, which would make the
// admin console's "revoked" a lie.
func (st *mcpOAuthState) revokeClientRefresh(ctx context.Context, clientID string) error {
	if clientID == "" {
		return errors.New("oauth: refusing to revoke refresh tokens for an empty client_id")
	}
	return st.deleteMatching(ctx, func(rec refreshRecord) bool { return rec.ClientID == clientID })
}

// sweepRefresh drops expired and consumed-but-stale records so the prefix
// doesn't grow without bound. Consumed records are kept for a grace window
// past expiry because they are what reuse detection reads — deleting one
// immediately would turn a detectable replay into a silent miss.
func (st *mcpOAuthState) sweepRefresh(ctx context.Context) {
	cutoff := time.Now().Add(-refreshTombstoneGrace)
	err := st.deleteMatching(ctx, func(rec refreshRecord) bool { return cutoff.After(rec.Expires) })
	if err != nil {
		slog.Warn("mcp.oauth.refresh_sweep_failed", "err", err)
	}
}

// maybeSweepRefresh runs the sweep at most once per refreshSweepInterval.
//
// The throttle is load-bearing, not politeness. The sweep is a LIST plus a GET
// per record, and refresh records are long-lived: every rotation writes a new
// one and keeps the consumed one as a replay tombstone, so a client refreshing
// twice a day accumulates ~70 records over their lifetime. Sweeping on every
// grant would make each refresh cost a read of every record every other client
// has ever held — work that grows with usage on the exact path this feature
// exists to keep cheap.
func (st *mcpOAuthState) maybeSweepRefresh(ctx context.Context) {
	now := time.Now().UnixNano()
	last := st.lastRefreshSweep.Load()
	if now-last < int64(refreshSweepInterval) {
		return
	}
	// CAS so concurrent grants don't all sweep; the loser simply skips.
	if !st.lastRefreshSweep.CompareAndSwap(last, now) {
		return
	}
	st.sweepRefresh(ctx)
}

// deleteMatching removes every refresh record satisfying match. Unparseable
// records are removed too: they can never be honoured, so keeping them only
// slows the sweep.
func (st *mcpOAuthState) deleteMatching(ctx context.Context, match func(refreshRecord) bool) error {
	keys, err := st.store.List(ctx, st.refreshPrefix())
	if err != nil {
		return fmt.Errorf("oauth: list refresh tokens: %w", err)
	}
	var failed error
	for _, key := range keys {
		obj, err := st.store.Get(ctx, key)
		if err != nil || obj.Content == "" {
			continue
		}
		rec := refreshRecord{}
		parseErr := json.Unmarshal([]byte(obj.Content), &rec)
		if parseErr == nil && !match(rec) {
			continue
		}
		err = st.store.Delete(ctx, key)
		if err != nil {
			// Keep going: a caller revoking a chain wants every link it can
			// remove gone, not an early return that leaves the rest live.
			// The first failure is reported once the pass completes.
			slog.Warn("mcp.oauth.refresh_delete_failed", "key", key, "err", err)
			if failed == nil {
				failed = fmt.Errorf("oauth: delete refresh token %s: %w", key, err)
			}
		}
	}
	return failed
}
