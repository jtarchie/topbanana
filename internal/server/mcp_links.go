package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/linkcheck"
)

type checkLinksInput struct {
	Slug string `json:"slug" jsonschema:"The site slug to check"`
}

// registerCheckLinks stays out of lint_site because a third-party outage must never read as the author's error.
func (s *Server) registerCheckLinks(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "check_links",
		Description: "Check every link and embedded image/video/iframe on a site the caller owns that points at another website, by requesting each URL from the server. Links between the site's own pages are lint_site's job; this covers the rest. Each problem is dead (the domain does not exist, 404/410, or a private/local address: fix or remove it, and never guess a replacement URL), suspect (timeout, server error, bad certificate: may be temporary), unverified (the host blocks automated checks, as LinkedIn does: usually fine), or unchecked (the time budget ran out; run again). Answers are cached for up to a week, so re-running is cheap. Advisory: nothing here blocks publishing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in checkLinksInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		if s.linkChecker == nil {
			return nil, nil, errors.New("link checking is not enabled on this server")
		}
		rep, err := s.linkChecker.Check(ctx, in.Slug)
		if err != nil {
			return nil, nil, fmt.Errorf("check links: %w", err)
		}
		problems := make([]map[string]any, 0)
		for _, r := range rep.Results {
			if r.Status == linkcheck.StatusOK {
				continue
			}
			problems = append(problems, map[string]any{
				"url": r.URL, "status": r.Status, "reason": r.Reason, "code": r.Code, "pages": r.Pages,
			})
		}
		return mcpJSON(map[string]any{
			"slug":       in.Slug,
			"total":      len(rep.Results),
			"ok":         rep.Count(linkcheck.StatusOK),
			"dead":       rep.Count(linkcheck.StatusDead),
			"suspect":    rep.Count(linkcheck.StatusSuspect),
			"unverified": rep.Count(linkcheck.StatusUnverified),
			"unchecked":  rep.Count(linkcheck.StatusUnchecked),
			"problems":   problems,
		})
	})
}
