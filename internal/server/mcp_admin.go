package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/model"
	"github.com/jtarchie/topbanana/internal/quotas"
)

// The super-admin half of the MCP surface: issuing, listing, and revoking the
// invites that are the only way onto this instance. Everything else in the MCP
// tools is scoped to sites the caller owns; these three are scoped to the
// caller's *role*, so they live in their own file with their own gate rather
// than sharing mcpUserAndAuthorize's slug-shaped one.

// mcpMaxInviteTTL bounds ttl_hours. An invite is a bearer credential for an
// account on this instance, so "never expires" is not an option a tool call
// gets to pick; 30 days is past any plausible "I'll send it next week".
const mcpMaxInviteTTL = 30 * 24 * time.Hour

// mcpSuperAdmin resolves the caller from the bearer token and requires the
// super-admin role, mirroring requireSuperAdmin on the web surface. The error
// is deliberately flat — a regular admin learns the tool exists (it's in the
// tool list either way) but nothing about who does have the role.
//
// ponytail: the gate is the *caller's* role, which means every MCP client a
// super admin authorizes inherits invite-issuing power along with site
// editing. Upgrade path when that's too coarse: mint a distinct `mcp:admin`
// scope, have the client request it at /oauth/authorize (auth/oauth), check it
// off TokenInfoFromContext(ctx).Scopes here, and approve it per client on the
// consent page — so a coding agent can hold the editing surface without this
// one.
func (s *Server) mcpSuperAdmin(ctx context.Context) (*auth.User, error) {
	user, err := s.mcpUserAndAuthorize(ctx, "")
	if err != nil {
		return nil, err
	}
	if user.Role != auth.RoleSuperAdmin {
		return nil, errors.New("not permitted: this tool requires the super-admin role")
	}
	return user, nil
}

// mcpInviteURL is the redeemable link for a token. Built from mcpBaseURL
// rather than the web surface's adminURL, which derives scheme and port from
// an *echo.Context an MCP tool call doesn't have. The token is
// base64-RawURL-encoded, so it needs no query escaping — and skipping it keeps
// this byte-identical to the URL the /admin/users table renders.
func (s *Server) mcpInviteURL(token string) string {
	return s.mcpBaseURL() + "/register?invite=" + token
}

// mcpInviteRow is one invite as the tools report it. Quotas are flattened out
// of the opaque Meta blob so a caller can see what an invite will provision
// without decoding it themselves.
type mcpInviteRow struct {
	Token   string        `json:"token"`
	Email   string        `json:"email"`
	Role    string        `json:"role"`
	URL     string        `json:"url"`
	Created time.Time     `json:"created"`
	Expires time.Time     `json:"expires"`
	MaxApps int           `json:"max_apps,omitempty"`
	Models  model.TierMap `json:"models,omitempty"`
}

func (s *Server) mcpInviteRowOf(inv *auth.Invite) mcpInviteRow {
	q := quotas.OfInvite(inv)
	return mcpInviteRow{
		Token:   inv.Token,
		Email:   inv.Email,
		Role:    string(inv.Role),
		URL:     s.mcpInviteURL(inv.Token),
		Created: inv.Created,
		Expires: inv.Expires,
		MaxApps: q.MaxApps,
		Models:  q.AllowedModels,
	}
}

// mcpInviteRole validates the requested role. Empty defaults to admin, and
// anything outside the two real roles is rejected rather than silently
// downgraded — an agent that fat-fingers "superadmin" should hear about it,
// not quietly hand out a weaker invite than the operator asked for.
func mcpInviteRole(raw string) (auth.Role, error) {
	switch strings.TrimSpace(raw) {
	case "":
		return auth.RoleAdmin, nil
	case string(auth.RoleAdmin):
		return auth.RoleAdmin, nil
	case string(auth.RoleSuperAdmin):
		return auth.RoleSuperAdmin, nil
	default:
		return "", fmt.Errorf("invalid role %q (expected %q or %q)", raw, auth.RoleAdmin, auth.RoleSuperAdmin)
	}
}

// mcpInviteTiers validates a per-tier model override map, rejecting unknown
// tier keys. An empty result stays an empty map rather than nil: quotas.Encode
// keys off len(), so either leaves no field behind on the invite record.
func mcpInviteTiers(in map[string]string) (model.TierMap, error) {
	if len(in) == 0 {
		return model.TierMap{}, nil
	}
	known := make(map[string]bool, len(model.AllTiers))
	for _, t := range model.AllTiers {
		known[string(t)] = true
	}
	out := model.TierMap{}
	for k, v := range in {
		if !known[k] {
			return nil, fmt.Errorf("unknown model tier %q (expected one of %s)", k, mcpTierNames())
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out[model.Tier(k)] = v
	}
	return out, nil
}

func mcpTierNames() string {
	names := make([]string, 0, len(model.AllTiers))
	for _, t := range model.AllTiers {
		names = append(names, string(t))
	}
	return strings.Join(names, ", ")
}

type issueInviteInput struct {
	Email    string            `json:"email"               jsonschema:"Email address the invite is for. Normalized before storage."`
	Role     string            `json:"role,omitempty"      jsonschema:"admin (default) or super_admin. super_admin can manage every site and every user on the instance."`
	TTLHours int               `json:"ttl_hours,omitempty" jsonschema:"How long the invite stays redeemable, in hours. Defaults to 168 (7 days); capped at 720 (30 days)."`
	MaxApps  int               `json:"max_apps,omitempty"  jsonschema:"Cap on how many sites the invited user may own. Omit to use the platform default."`
	Models   map[string]string `json:"models,omitempty"    jsonschema:"Per-tier LLM model overrides for the invited user, keyed by tier: author, editor, utility, vision. Omit to inherit the platform defaults."`
}

// registerIssueInvite exposes invite creation. Note it can provision quotas
// the /admin/users form cannot: that form passes nil meta, so a pre-quota'd
// invite has until now required issuing one and then editing the user record
// after they redeem it.
func (s *Server) registerIssueInvite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "issue_invite",
		Description: "Super-admin only. Issue a one-time invite to join this Top Banana instance, optionally pre-provisioning the new user's site cap and per-tier models. Returns a redeemable registration URL. " +
			"That URL is a live credential: whoever holds it can bind a passkey and claim the account, so hand it to the intended recipient and nobody else.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in issueInviteInput) (*mcp.CallToolResult, any, error) {
		caller, err := s.mcpSuperAdmin(ctx)
		if err != nil {
			return nil, nil, err
		}
		email := auth.NormalizeEmail(in.Email)
		if email == "" {
			return nil, nil, errors.New("email is required")
		}
		role, err := mcpInviteRole(in.Role)
		if err != nil {
			return nil, nil, err
		}
		tiers, err := mcpInviteTiers(in.Models)
		if err != nil {
			return nil, nil, err
		}
		if in.MaxApps < 0 {
			return nil, nil, errors.New("max_apps must be zero (platform default) or positive")
		}
		meta, err := quotas.Encode(quotas.Quotas{MaxApps: in.MaxApps, AllowedModels: tiers})
		if err != nil {
			return nil, nil, fmt.Errorf("encode quotas: %w", err)
		}

		// Clamp the hours before multiplying: time.Duration is an int64 of
		// nanoseconds, so a large ttl_hours overflows in the multiply and wraps
		// negative — which slips past a post-multiply ceiling check and mints
		// an invite that expired before it was returned.
		ttl := auth.DefaultInviteTTL
		if in.TTLHours > 0 {
			hours := min(in.TTLHours, int(mcpMaxInviteTTL/time.Hour))
			ttl = time.Duration(hours) * time.Hour
		}

		inv, err := s.auth.Invites.Issue(ctx, email, role, meta, ttl)
		if err != nil {
			return nil, nil, fmt.Errorf("issue invite: %w", err)
		}
		slog.Info("invite.issue", "email", inv.Email, "role", inv.Role, "by", caller.Email, "via", "mcp")

		row := s.mcpInviteRowOf(inv)
		return mcpJSON(map[string]any{
			"ok": true, "invite": row,
			"next": "send this url only to " + inv.Email + " — anyone holding it can claim the account until it expires or you revoke_invite it",
		})
	})
}

type listInvitesInput struct{}

func (s *Server) registerListInvites(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_invites",
		Description: "Super-admin only. List the invites that are still redeemable — consumed and expired ones are omitted, matching the pending-invites table on /admin/users. Each row carries its registration URL, which is a live credential.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listInvitesInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpSuperAdmin(ctx)
		if err != nil {
			return nil, nil, err
		}
		invites, err := s.auth.Invites.List(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list invites: %w", err)
		}
		now := time.Now()
		rows := make([]mcpInviteRow, 0, len(invites))
		for _, inv := range invites {
			if inv.UsedBy != "" || now.After(inv.Expires) {
				continue
			}
			rows = append(rows, s.mcpInviteRowOf(inv))
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Email < rows[j].Email })
		return mcpJSON(map[string]any{"invites": rows})
	})
}

type revokeInviteInput struct {
	Token string `json:"token" jsonschema:"The invite token, as returned by issue_invite or list_invites."`
}

func (s *Server) registerRevokeInvite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "revoke_invite",
		Description: "Super-admin only. Delete an invite so its registration URL stops working. Use this when a link went to the wrong address. Irreversible; issue a fresh invite instead of trying to restore one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in revokeInviteInput) (*mcp.CallToolResult, any, error) {
		caller, err := s.mcpSuperAdmin(ctx)
		if err != nil {
			return nil, nil, err
		}
		token := strings.TrimSpace(in.Token)
		if token == "" {
			return nil, nil, errors.New("token is required")
		}
		// Confirm the token names something before deleting. Revoke is a blob
		// delete, which S3 answers 204 for a key that was never there — so
		// without this a mistyped token reports "revoked" and the operator
		// believes a still-live registration link is dead. An expired invite is
		// still worth deleting (cleanup), so only a genuine miss is an error.
		inv, err := s.auth.Invites.Get(ctx, token)
		switch {
		case errors.Is(err, auth.ErrInviteNotFound):
			return nil, nil, errors.New("no pending invite with that token — it was already used or revoked, or the token is wrong (list_invites shows what is outstanding)")
		case err != nil && !errors.Is(err, auth.ErrInviteExpired):
			return nil, nil, fmt.Errorf("look up invite: %w", err)
		}
		err = s.auth.Invites.Revoke(ctx, token)
		if err != nil {
			return nil, nil, fmt.Errorf("revoke invite: %w", err)
		}
		// inv is nil when Get reported the invite expired; the token is all
		// that's needed for the log line in that case.
		email := ""
		if inv != nil {
			email = inv.Email
		}
		slog.Info("invite.revoke", "email", email, "by", caller.Email, "via", "mcp")
		return mcpJSON(map[string]any{"ok": true, "token": token, "email": email})
	})
}
