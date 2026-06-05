package server

import (
	"bytes"
	"html/template"
	"strings"
	"testing"
)

// renderAccount parses the embedded layout + account templates the same way
// New does (layout first so its partials resolve, then the named "account"
// body) and executes "account" against data. Deterministic — no Minio or HTTP
// server needed, which is the signal CI actually runs.
func renderAccount(t *testing.T, data accountData) string {
	t.Helper()
	tpl := template.New("")
	template.Must(tpl.Parse(layoutTemplate))
	template.Must(tpl.New("account").Parse(accountTemplate))
	var buf bytes.Buffer
	err := tpl.ExecuteTemplate(&buf, "account", data)
	if err != nil {
		t.Fatalf("execute account template: %v", err)
	}
	return buf.String()
}

const testMCPCommand = "claude mcp add --transport http topbanana http://localhost:8080/mcp"

// TestAccount_MCPEnabled: when MCP is on, the card shows the copy-paste connect
// command and Copy button, not the disabled-state hint.
func TestAccount_MCPEnabled(t *testing.T) {
	t.Parallel()

	html := renderAccount(t, accountData{
		Email:      "user@example.com",
		Role:       "admin",
		MCPEnabled: true,
		MCPCommand: testMCPCommand,
	})

	if !strings.Contains(html, testMCPCommand) {
		t.Errorf("enabled card missing connect command %q", testMCPCommand)
	}
	if !strings.Contains(html, ">Copy<") {
		t.Errorf("enabled card missing Copy button")
	}
	if strings.Contains(html, "MCP_SECRET") {
		t.Errorf("enabled card should not show the MCP_SECRET enable hint")
	}
}

// TestAccount_DisabledSuperAdmin: when MCP is off and the viewer is a super
// admin, the card explains it's not enabled and how to enable it — but never
// renders a (nonexistent) connect command.
func TestAccount_DisabledSuperAdmin(t *testing.T) {
	t.Parallel()

	html := renderAccount(t, accountData{
		Email:        "boss@example.com",
		Role:         "super_admin",
		MCPEnabled:   false,
		IsSuperAdmin: true,
	})

	if !strings.Contains(html, "MCP_SECRET") {
		t.Errorf("disabled super-admin card missing the MCP_SECRET enable hint")
	}
	if !strings.Contains(html, "isn't enabled") {
		t.Errorf("disabled card missing the 'not enabled' note")
	}
	if strings.Contains(html, "claude mcp add") {
		t.Errorf("disabled card should not show a connect command")
	}
}

// TestAccount_DisabledRegularUser: when MCP is off and the viewer is not a
// super admin, they see the "not enabled" note but no operator-only enable
// instructions (they can't set server config).
func TestAccount_DisabledRegularUser(t *testing.T) {
	t.Parallel()

	html := renderAccount(t, accountData{
		Email:        "user@example.com",
		Role:         "admin",
		MCPEnabled:   false,
		IsSuperAdmin: false,
	})

	if !strings.Contains(html, "isn't enabled") {
		t.Errorf("disabled card missing the 'not enabled' note")
	}
	if strings.Contains(html, "MCP_SECRET") {
		t.Errorf("regular user should not see the MCP_SECRET enable hint")
	}
}
