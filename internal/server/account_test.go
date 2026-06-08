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

// TestAccount_DangerZoneRegularUser: a regular user gets working danger-zone
// forms — sign-out-everywhere and a typed-email delete bound to their address —
// and no leftover mailto: placeholders.
func TestAccount_DangerZoneRegularUser(t *testing.T) {
	t.Parallel()

	html := renderAccount(t, accountData{
		Email:        "user@example.com",
		Role:         "admin",
		IsSuperAdmin: false,
	})

	for _, want := range []string{
		`action="/account/sessions"`,
		`action="/account/delete"`,
		`data-confirm-slug="user@example.com"`,
		`name="confirm"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("danger zone missing %q", want)
		}
	}
	if strings.Contains(html, "mailto:") {
		t.Errorf("danger zone still has a mailto: placeholder")
	}
	if strings.Contains(html, "Coming soon") {
		t.Errorf("danger zone still says 'Coming soon'")
	}
}

// TestAccount_DangerZoneSuperAdmin: an operator can still sign out everywhere,
// but the self-delete form is replaced by a note — they can't delete their own
// account, so the form must not render.
func TestAccount_DangerZoneSuperAdmin(t *testing.T) {
	t.Parallel()

	html := renderAccount(t, accountData{
		Email:        "boss@example.com",
		Role:         "super_admin",
		IsSuperAdmin: true,
	})

	if !strings.Contains(html, `action="/account/sessions"`) {
		t.Errorf("super admin should still have sign-out-everywhere")
	}
	if strings.Contains(html, `action="/account/delete"`) {
		t.Errorf("super admin must not get a self-delete form")
	}
	if !strings.Contains(html, "can't be self-deleted") {
		t.Errorf("super admin missing the no-self-delete note")
	}
}

// TestAccount_PasskeyRemoveButton: the per-passkey Remove form renders only
// when there's more than one passkey — you can't remove your only sign-in key.
func TestAccount_PasskeyRemoveButton(t *testing.T) {
	t.Parallel()

	two := renderAccount(t, accountData{
		Email: "user@example.com",
		Role:  "admin",
		Credentials: []accountCredential{
			{ID: "aaa", RawID: "cred-aaa"},
			{ID: "bbb", RawID: "cred-bbb"},
		},
	})
	if !strings.Contains(two, `action="/account/passkeys/delete"`) {
		t.Errorf("two-passkey account missing the Remove form")
	}
	if !strings.Contains(two, `value="cred-bbb"`) {
		t.Errorf("Remove form missing the credential RawID")
	}

	one := renderAccount(t, accountData{
		Email:       "user@example.com",
		Role:        "admin",
		Credentials: []accountCredential{{ID: "aaa", RawID: "cred-aaa"}},
	})
	if strings.Contains(one, `action="/account/passkeys/delete"`) {
		t.Errorf("single-passkey account must not offer Remove")
	}
}

// TestAccount_FlashAndError: the danger-zone POSTs redirect back with a flash
// or error query param, which the page must surface.
func TestAccount_FlashAndError(t *testing.T) {
	t.Parallel()

	html := renderAccount(t, accountData{
		Email: "user@example.com",
		Role:  "admin",
		Flash: "Passkey removed",
		Error: "something broke",
	})
	if !strings.Contains(html, "Passkey removed") {
		t.Errorf("flash not rendered")
	}
	if !strings.Contains(html, "something broke") {
		t.Errorf("error not rendered")
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
