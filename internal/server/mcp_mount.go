package server

import (
	"fmt"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/auth/oauth"
)

// The OAuth authorization server lives in auth/oauth. What stays here is the
// part that is actually this application: the MCP tool surface behind the
// bearer gate, the upload endpoint, and the two things the library asks the
// host to supply — where a signed-in browser comes from, and what origin to
// advertise.

// mcpBaseURL is the externally-reachable origin the OAuth metadata advertises.
// Derived from the configured domain/port so it matches what the bearer
// middleware pins as the resource metadata URL. Local-dev (loopback) domains
// get http; everything else https.
func (s *Server) mcpBaseURL() string {
	host := stripPort(s.domain)
	if fallThroughHosts[host] {
		base := "http://" + s.domain
		if s.port != "" && s.port != "80" {
			base += ":" + s.port
		}
		return base
	}
	return "https://" + s.domain
}

// newOAuthServer builds the authorization server from this app's configuration.
// CurrentUser is the whole integration: the library never learns what a passkey
// is, it just asks whether this browser is signed in.
func (s *Server) newOAuthServer() (*oauth.Server, error) {
	oa, err := oauth.New(oauth.Config{
		Blobs:       s.blobs,
		BaseURL:     s.mcpBaseURL(),
		Secret:      s.mcpSecret,
		CurrentUser: s.currentSessionEmail,
	})
	if err != nil {
		return nil, fmt.Errorf("server: build oauth server: %w", err)
	}
	return oa, nil
}

// mountMCP publishes the OAuth endpoints, then the bearer-protected MCP
// endpoint the tokens are for. Called from New only when an MCP secret is set.
func (s *Server) mountMCP(e *echo.Echo) {
	s.mcpOAuth.Mount(e)

	verifier := auth.MCPTokenVerifier(s.mcpSecret)
	protected := mcpauth.RequireBearerToken(verifier, &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.mcpOAuth.ResourceMetadataURL(),
		Scopes:              []string{auth.MCPScope},
	})(s.newMCPHandler())
	e.Any("/mcp", echo.WrapHandler(protected))
	e.Any("/mcp/*", echo.WrapHandler(protected))

	// Binary upload endpoint for create_upload_ticket. Auth lives in the signed
	// ticket carried in the path (the agent can't read its MCP bearer token to
	// set a header), so this route is outside the bearer middleware; BodyLimit
	// is a first-line cap before the handler re-checks the size.
	e.POST("/upload/ticket/:token", s.uploadTicketHandler, middleware.BodyLimit(maxUploadBytes+1024))
}
