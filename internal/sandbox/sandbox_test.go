package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func mustInvoke(t *testing.T, src string, req Request) (Response, []string) {
	t.Helper()
	m := New(Config{CPUTimeout: 500 * time.Millisecond})
	var logs []string
	resp, err := m.Invoke(context.Background(), "slug", InvokeRequest{Name: "fn", Source: src, Request: req, Log: func(level, line string) {
		logs = append(logs, level+": "+line)
	}})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	return resp, logs
}

func TestSandbox_RedirectResponse(t *testing.T) {
	src := `module.exports = function (req) { return response.redirect("/thanks.html"); };`
	resp, _ := mustInvoke(t, src, Request{Method: "POST", Path: "/api/submit"})
	if resp.Status != 303 {
		t.Fatalf("status: %d", resp.Status)
	}
	if resp.Headers["Location"] != "/thanks.html" {
		t.Fatalf("location: %q", resp.Headers["Location"])
	}
}

func TestSandbox_JSONResponse(t *testing.T) {
	src := `module.exports = function (req) { return response.json({ ok: true, n: 3 }); };`
	resp, _ := mustInvoke(t, src, Request{Method: "GET"})
	if resp.Status != 200 {
		t.Fatalf("status: %d", resp.Status)
	}
	if resp.ContentType != "application/json" {
		t.Fatalf("ct: %q", resp.ContentType)
	}
	if !strings.Contains(string(resp.Body), `"ok":true`) {
		t.Fatalf("body: %s", resp.Body)
	}
}

// Regression: response.json({...}, 400) is the documented error shape (used
// by the contact-form skeleton and the functions prompt), but the builder
// dropped the second arg and hardcoded 200 — so every validation failure went
// out as a success status.
func TestSandbox_JSONResponseWithStatus(t *testing.T) {
	src := `module.exports = function (req) { return response.json({ errors: [{field: "email"}] }, 400); };`
	resp, _ := mustInvoke(t, src, Request{Method: "POST"})
	if resp.Status != 400 {
		t.Fatalf("status: %d want 400 — response.json must honor its optional status arg", resp.Status)
	}
	if resp.ContentType != "application/json" {
		t.Fatalf("ct: %q", resp.ContentType)
	}
	if !strings.Contains(string(resp.Body), `"errors"`) {
		t.Fatalf("body: %s", resp.Body)
	}
}

func TestSandbox_RequestFieldsExposed(t *testing.T) {
	src := `module.exports = function (req) {
		return response.json({ method: req.method, q: req.query.x, h: req.headers["x-test"], body: req.body });
	};`
	resp, _ := mustInvoke(t, src, Request{
		Method:  "POST",
		Query:   map[string]string{"x": "yes"},
		Headers: map[string]string{"x-test": "ping"},
		Body:    "raw",
	})
	if !strings.Contains(string(resp.Body), `"method":"POST"`) {
		t.Fatalf("body missing method: %s", resp.Body)
	}
	if !strings.Contains(string(resp.Body), `"q":"yes"`) {
		t.Fatalf("body missing query: %s", resp.Body)
	}
	if !strings.Contains(string(resp.Body), `"h":"ping"`) {
		t.Fatalf("body missing header: %s", resp.Body)
	}
}

func TestSandbox_ConsoleLogStreamed(t *testing.T) {
	src := `module.exports = function () { console.log("hello", "world"); console.warn("oops"); return response.text("ok"); };`
	_, logs := mustInvoke(t, src, Request{})
	if len(logs) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %v", len(logs), logs)
	}
	if !strings.Contains(logs[0], "hello world") {
		t.Fatalf("log[0]: %q", logs[0])
	}
	if !strings.HasPrefix(logs[1], "warn:") {
		t.Fatalf("log[1]: %q", logs[1])
	}
}

func TestSandbox_TimeoutInfiniteLoop(t *testing.T) {
	src := `module.exports = function () { while(true){} };`
	m := New(Config{CPUTimeout: 50 * time.Millisecond})
	_, err := m.Invoke(context.Background(), "slug", InvokeRequest{Name: "fn", Source: src})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got: %v", err)
	}
}

func TestSandbox_NoHandlerExportedRejected(t *testing.T) {
	src := `var x = 1;` // no module.exports assignment
	m := New(Config{})
	_, err := m.Invoke(context.Background(), "slug", InvokeRequest{Name: "fn", Source: src})
	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("expected ErrNoHandler, got: %v", err)
	}
}

func TestSandbox_HandlerExceptionBecomes500(t *testing.T) {
	src := `module.exports = function () { throw new Error("boom"); };`
	resp, _ := mustInvoke(t, src, Request{})
	if resp.Status != 500 {
		t.Fatalf("status: %d", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "boom") {
		t.Fatalf("body: %s", resp.Body)
	}
}

func TestSandbox_EvalStripped(t *testing.T) {
	src := `module.exports = function () { return typeof eval === "undefined" ? response.text("safe") : response.text("unsafe"); };`
	resp, _ := mustInvoke(t, src, Request{})
	if string(resp.Body) != "safe" {
		t.Fatalf("eval was not stripped: body=%q", resp.Body)
	}
}

func TestSandbox_RateLimitedEventually(t *testing.T) {
	src := `module.exports = function () { return response.text("ok"); };`
	m := New(Config{RPS: 1, RPSBurst: 1})
	ctx := context.Background()
	// First call should succeed.
	_, err := m.Invoke(ctx, "s", InvokeRequest{Name: "f", Source: src})
	if err != nil {
		t.Fatalf("first invoke: %v", err)
	}
	// Spam until we see a rate-limit error.
	var saw bool
	for range 5 {
		_, err = m.Invoke(ctx, "s", InvokeRequest{Name: "f", Source: src})
		if errors.Is(err, ErrRateLimit) {
			saw = true
			break
		}
	}
	if !saw {
		t.Fatal("expected rate limit error after burst exhausted")
	}
}

func TestSandbox_CompileErrorWrapped(t *testing.T) {
	src := `module.exports = function ( {` // syntax error
	m := New(Config{})
	_, err := m.Invoke(context.Background(), "s", InvokeRequest{Name: "f", Source: src})
	if !errors.Is(err, ErrCompile) {
		t.Fatalf("expected ErrCompile, got: %v", err)
	}
}

func TestSandbox_UndefinedReturnIs204(t *testing.T) {
	src := `module.exports = function () { /* nothing */ };`
	resp, _ := mustInvoke(t, src, Request{})
	if resp.Status != 204 {
		t.Fatalf("status: %d", resp.Status)
	}
}

func TestSandbox_StringReturnIsHTML(t *testing.T) {
	src := `module.exports = function () { return "<h1>hi</h1>"; };`
	resp, _ := mustInvoke(t, src, Request{})
	if resp.Status != 200 || resp.ContentType != "text/html; charset=utf-8" {
		t.Fatalf("status=%d ct=%q", resp.Status, resp.ContentType)
	}
	if string(resp.Body) != "<h1>hi</h1>" {
		t.Fatalf("body: %s", resp.Body)
	}
}
