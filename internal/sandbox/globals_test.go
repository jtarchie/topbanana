package sandbox

import (
	"strings"
	"testing"
)

// TestSandbox_EscapeHelper drives the escape() global end-to-end through Invoke
// — the XSS-prevention primitive handlers use before concatenating user input
// into HTML. The direct Go test of html-escaping can't see the goja value
// round-trip; this can.
func TestSandbox_EscapeHelper(t *testing.T) {
	src := `module.exports = function (req) { return response.text(escape('<b>&"\'')); };`
	resp, _ := mustInvoke(t, src, Request{Method: "GET"})
	got := string(resp.Body)
	// template.HTMLEscapeString: < > & " ' -> &lt; &gt; &amp; &#34; &#39;
	want := "&lt;b&gt;&amp;&#34;&#39;"
	if got != want {
		t.Fatalf("escape output = %q, want %q", got, want)
	}
}

// TestSandbox_ValidateHelper_OK exercises validate() through the goja boundary:
// a valid input returns ok:true with the cleaned data, and unknown fields are
// dropped (strong-parameters posture).
func TestSandbox_ValidateHelper_OK(t *testing.T) {
	src := `module.exports = function (req) {
		var r = validate({ email: 'a@b.com', extra: 'nope' }, { email: { type: 'email', required: true } });
		return response.json(r);
	};`
	resp, _ := mustInvoke(t, src, Request{Method: "POST"})
	body := string(resp.Body)
	if !strings.Contains(body, `"ok":true`) {
		t.Fatalf("validate result not ok: %s", body)
	}
	if !strings.Contains(body, "a@b.com") {
		t.Fatalf("validate dropped the valid field: %s", body)
	}
	if strings.Contains(body, "nope") {
		t.Fatalf("validate kept an unknown field (strong-params violated): %s", body)
	}
}

// TestSandbox_ValidateHelper_Errors confirms the ok:false / errors[] shape comes
// back through goja for a failing field.
func TestSandbox_ValidateHelper_Errors(t *testing.T) {
	src := `module.exports = function (req) {
		var r = validate({ email: 'not-an-email' }, { email: { type: 'email', required: true } });
		return response.json(r);
	};`
	resp, _ := mustInvoke(t, src, Request{Method: "POST"})
	body := string(resp.Body)
	if !strings.Contains(body, `"ok":false`) {
		t.Fatalf("expected ok:false for bad email: %s", body)
	}
	if !strings.Contains(body, "email") {
		t.Fatalf("expected the failing field name in errors: %s", body)
	}
}

// TestSandbox_ValidateHelper_URLScheme locks in the url-type scheme restriction:
// javascript:/data: URLs are rejected (stored-XSS / open-redirect guard) while
// https URLs pass — exercised through the real goja validate() path.
func TestSandbox_ValidateHelper_URLScheme(t *testing.T) {
	run := func(value string) string {
		t.Helper()
		src := `module.exports = function (req) {
			var r = validate({ site: '` + value + `' }, { site: { type: 'url', required: true } });
			return response.json(r);
		};`
		resp, _ := mustInvoke(t, src, Request{Method: "POST"})
		return string(resp.Body)
	}

	for _, bad := range []string{"javascript://alert(1)", "data://text/html,x"} {
		body := run(bad)
		if !strings.Contains(body, `"ok":false`) {
			t.Errorf("url validate accepted %q (should reject non-http scheme): %s", bad, body)
		}
	}

	good := run("https://example.com/page")
	if !strings.Contains(good, `"ok":true`) {
		t.Fatalf("url validate rejected a valid https URL: %s", good)
	}
}
