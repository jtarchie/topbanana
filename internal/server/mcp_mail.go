package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jtarchie/mailcatch/mailparse"
	"github.com/jtarchie/mailcatch/mailstore"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type listMailInput struct {
	Slug  string `json:"slug"            jsonschema:"The site slug"`
	Inbox string `json:"inbox,omitempty" jsonschema:"Only this local part (before the @); empty lists every address"`
	Limit int    `json:"limit,omitempty" jsonschema:"Newest N messages, default 50, max 200"`
}

type mailRefInput struct {
	Slug  string `json:"slug"  jsonschema:"The site slug"`
	Inbox string `json:"inbox" jsonschema:"The message's inbox, as returned by list_mail"`
	ID    string `json:"id"    jsonschema:"The message id, as returned by list_mail"`
}

var errMailOff = errors.New("mail is not enabled on this server")

// registerMailTools lets an agent verify a signup or contact flow end to end; enabling mail stays a super-admin web action.
func (s *Server) registerMailTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_mail",
		Description: "List email caught for a site the caller can access (any address at its subdomain or attached custom domains, when a super admin has enabled mail for it). Newest first; no bodies. Use read_mail for one message.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listMailInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		if s.mail == nil {
			return nil, nil, errMailOff
		}
		limit := in.Limit
		if limit <= 0 || limit > mailListLimit {
			limit = 50
		}
		inbox := ""
		if in.Inbox != "" {
			norm, ok := mailstore.NormalizeInbox(in.Inbox)
			if !ok {
				return nil, nil, fmt.Errorf("invalid inbox %q", in.Inbox)
			}
			inbox = norm
		}
		msgs, err := s.mail.List(ctx, in.Slug, inbox, limit)
		if err != nil {
			return nil, nil, fmt.Errorf("list mail: %w", err)
		}
		_, enabled := s.registry.mailTTL(in.Slug)
		return mcpJSON(map[string]any{"slug": in.Slug, "enabled": enabled, "messages": msgs})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_mail",
		Description: "Read one caught email: headers, decoded text and HTML bodies, and the attachment list (names and sizes only; attachment bytes are never exposed).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mailRefInput) (*mcp.CallToolResult, any, error) {
		m, err := s.mcpMail(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		body := mailparse.ParseBody(m.Raw)
		return mcpJSON(map[string]any{"message": m, "text": body.Text, "html": body.HTML, "attachments": body.Attachments, "truncated": body.Truncated})
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_mail",
		Description: "Delete one caught email, e.g. a test message you triggered while verifying a form. Irreversible.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mailRefInput) (*mcp.CallToolResult, any, error) {
		m, err := s.mcpMail(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		err = s.mail.Delete(ctx, in.Slug, m.Inbox, m.ID)
		if err != nil && !errors.Is(err, mailstore.ErrNotFound) {
			return nil, nil, fmt.Errorf("delete mail: %w", err)
		}
		return mcpJSON(map[string]any{"deleted": m.ID})
	})
}

func (s *Server) mcpMail(ctx context.Context, in mailRefInput) (mailstore.Message, error) {
	_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
	if err != nil {
		return mailstore.Message{}, err
	}
	if s.mail == nil {
		return mailstore.Message{}, errMailOff
	}
	inbox, ok := mailstore.NormalizeInbox(in.Inbox)
	if !ok {
		return mailstore.Message{}, mailstore.ErrNotFound
	}
	m, err := s.mail.Get(ctx, in.Slug, inbox, in.ID)
	if err != nil {
		return mailstore.Message{}, fmt.Errorf("read mail: %w", err)
	}
	return m, nil
}
