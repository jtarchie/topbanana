package model

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestWithSessionID_RoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		id   string
		want string
	}{
		{"empty id is ignored", "", ""},
		{"normal id round-trips", "20260614T014352.791215216Z-edit.json", "20260614T014352.791215216Z-edit.json"},
		{"over-long id is dropped", strings.Repeat("x", maxSessionIDLen+1), ""},
		{"max-length id is kept", strings.Repeat("x", maxSessionIDLen), strings.Repeat("x", maxSessionIDLen)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := sessionIDFromContext(WithSessionID(context.Background(), tc.id))
			if got != tc.want {
				t.Errorf("sessionIDFromContext after WithSessionID(%q) = %q, want %q", tc.id, got, tc.want)
			}
		})
	}
}

func TestIsAnthropicModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		want bool
	}{
		{"anthropic/claude-haiku-latest", true},
		{"~anthropic/claude-sonnet-latest", true},
		{"openai/gpt-4o", false},
		{"~openai/gpt-4o", false},
		{"google/gemini-2.5-pro", false},
		{"deepseek/deepseek-v3", false},
		{"", false},
		{"anthropic", false}, // missing slash — guard against prefix collision with future provider names
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isAnthropicModel(tc.name); got != tc.want {
				t.Errorf("isAnthropicModel(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsOpenRouterChatCompletion(t *testing.T) {
	t.Parallel()

	cases := []struct {
		url  string
		want bool
	}{
		{"https://openrouter.ai/api/v1/chat/completions", true},
		{"https://api.openrouter.ai/v1/chat/completions", true},
		{"https://openrouter.ai/api/v1/models", false},
		{"https://api.anthropic.com/v1/messages", false},
		{"http://localhost:1234/v1/chat/completions", false},
		{"https://api.openai.com/v1/chat/completions", false},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			t.Parallel()

			u, err := url.Parse(tc.url)
			if err != nil {
				t.Fatalf("parse %q: %v", tc.url, err)
			}

			req := &http.Request{URL: u}
			if got := isOpenRouterChatCompletion(req); got != tc.want {
				t.Errorf("isOpenRouterChatCompletion(%q) = %v, want %v", tc.url, got, tc.want)
			}
		})
	}
}

func TestMaybeInjectCacheControl(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		body     map[string]any
		wantCC   bool
		wantKeep []string // top-level keys that must survive
	}{
		{
			name:     "anthropic gets cache_control injected",
			body:     map[string]any{"model": "anthropic/claude-haiku-latest", "messages": []any{}},
			wantCC:   true,
			wantKeep: []string{"model", "messages"},
		},
		{
			name:     "anthropic with tilde prefix also gets cache_control",
			body:     map[string]any{"model": "~anthropic/claude-sonnet-latest", "stream": true},
			wantCC:   true,
			wantKeep: []string{"model", "stream"},
		},
		{
			name:     "openai does not get cache_control",
			body:     map[string]any{"model": "openai/gpt-4o", "messages": []any{}},
			wantCC:   false,
			wantKeep: []string{"model", "messages"},
		},
		{
			name:     "deepseek does not get cache_control",
			body:     map[string]any{"model": "deepseek/deepseek-v3", "stream": false},
			wantCC:   false,
			wantKeep: []string{"model", "stream"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runMaybeInjectCase(t, tc.body, tc.wantCC, tc.wantKeep)
		})
	}
}

func runMaybeInjectCase(t *testing.T, in map[string]any, wantCC bool, wantKeep []string) {
	t.Helper()

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("build req: %v", err)
	}

	err = maybeInjectCacheControl(req)
	if err != nil {
		t.Fatalf("maybeInjectCacheControl: %v", err)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read rewritten body: %v", err)
	}

	if int64(len(body)) != req.ContentLength {
		t.Errorf("ContentLength=%d, want %d", req.ContentLength, len(body))
	}

	out := mustUnmarshal(t, body)
	assertCacheControl(t, out, body, wantCC)
	assertKeysSurvive(t, out, body, wantKeep)
	assertGetBodyRoundTrip(t, req, body)
}

func mustUnmarshal(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var out map[string]any

	err := json.Unmarshal(body, &out)
	if err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}

	return out
}

func assertCacheControl(t *testing.T, out map[string]any, body []byte, want bool) {
	t.Helper()

	cc, has := out["cache_control"]
	if !want {
		if has {
			t.Errorf("non-Anthropic model got cache_control: %s", body)
		}

		return
	}

	if !has {
		t.Fatalf("rewritten body missing cache_control: %s", body)
	}

	obj, ok := cc.(map[string]any)
	if !ok || obj["type"] != "ephemeral" {
		t.Errorf("cache_control = %v, want {type: ephemeral}", cc)
	}
}

func assertKeysSurvive(t *testing.T, out map[string]any, body []byte, keys []string) {
	t.Helper()

	for _, k := range keys {
		if _, ok := out[k]; !ok {
			t.Errorf("rewritten body dropped key %q: %s", k, body)
		}
	}
}

func assertGetBodyRoundTrip(t *testing.T, req *http.Request, want []byte) {
	t.Helper()

	rb, err := req.GetBody()
	if err != nil {
		t.Fatalf("GetBody err: %v", err)
	}

	again, err := io.ReadAll(rb)
	if err != nil {
		t.Fatalf("GetBody read: %v", err)
	}

	if !bytes.Equal(want, again) {
		t.Errorf("GetBody returned different bytes")
	}
}

func TestCaptureSlot_ParseNonStreaming(t *testing.T) {
	t.Parallel()

	body := []byte(`{"id":"x","usage":{"prompt_tokens":10339,"completion_tokens":60,"total_tokens":10399,"prompt_tokens_details":{"cached_tokens":10318,"cache_write_tokens":0}},"cache_discount":0.0421}`)
	slot := &captureSlot{}
	slot.buf.Write(body)
	slot.parse()

	if slot.cachedTokens != 10318 {
		t.Errorf("cachedTokens=%d, want 10318", slot.cachedTokens)
	}

	if slot.cacheDiscount != 0.0421 {
		t.Errorf("cacheDiscount=%v, want 0.0421", slot.cacheDiscount)
	}

	// Re-parse must be a no-op (idempotent).
	prev := slot.cachedTokens
	slot.parse()

	if slot.cachedTokens != prev {
		t.Errorf("re-parse changed cachedTokens: %d -> %d", prev, slot.cachedTokens)
	}
}

func TestCaptureSlot_ParseSSE(t *testing.T) {
	t.Parallel()

	// Realistic OpenAI-compatible SSE stream: many chunks of partial content
	// then a final chunk carrying usage. Mid-stream chunks do not have usage.
	body := []byte(`data: {"id":"x","choices":[{"delta":{"content":"hello"}}]}

data: {"id":"x","choices":[{"delta":{"content":" world"}}]}

data: {"id":"x","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":1500,"completion_tokens":12,"total_tokens":1512,"prompt_tokens_details":{"cached_tokens":1450}}}

data: [DONE]
`)
	slot := &captureSlot{}
	slot.buf.Write(body)
	slot.parse()

	if slot.cachedTokens != 1450 {
		t.Errorf("cachedTokens=%d, want 1450 (SSE final usage chunk)", slot.cachedTokens)
	}
}

func TestCaptureSlot_ParseEmpty(t *testing.T) {
	t.Parallel()

	slot := &captureSlot{}
	slot.parse()

	if slot.cachedTokens != 0 || slot.cacheDiscount != 0 {
		t.Errorf("empty slot parse should produce zero values, got cached=%d discount=%v", slot.cachedTokens, slot.cacheDiscount)
	}
}

func TestCaptureSlot_NoCachedTokensField(t *testing.T) {
	t.Parallel()

	// Response without the cached_tokens detail — common on first turn before
	// the cache exists. extractCacheFields returns ok=true but cached=0; the
	// slot still seals so we don't re-parse on every iteration.
	body := []byte(`{"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11}}`)
	slot := &captureSlot{}
	slot.buf.Write(body)
	slot.parse()

	if slot.cachedTokens != 0 {
		t.Errorf("cachedTokens=%d, want 0 when field absent", slot.cachedTokens)
	}

	if !slot.parsed {
		t.Errorf("slot not sealed after a successful parse with zero cached")
	}
}

func TestRoundTrip_PassThroughForNonOpenRouterHost(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	tr := &openRouterTransport{base: http.DefaultTransport}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/anything", nil)
	if err != nil {
		t.Fatalf("build req: %v", err)
	}

	// Set a header we should NOT have touched.
	req.Header.Set("x-original", "kept")

	// Put a session-id in ctx that we should NOT have emitted (URL is not openrouter.ai).
	req = req.WithContext(WithSessionID(req.Context(), "should-not-appear"))

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}

	if got := req.Header.Get("x-session-id"); got != "" {
		t.Errorf("non-OpenRouter request got x-session-id=%q, want empty", got)
	}
}

func TestRoundTrip_OpenRouter_InjectsSessionAndCacheControl(t *testing.T) {
	t.Parallel()

	// We assert the mutations openRouterTransport applies to the outgoing
	// request via a base transport that records the request + body and
	// returns a canned response. No real network.
	capture := &captureTransport{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"id":"x","usage":{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":80}}}`))),
			Header:     http.Header{},
		},
	}
	tr := &openRouterTransport{base: capture}

	bodyJSON := []byte(`{"model":"anthropic/claude-haiku-latest","messages":[{"role":"user","content":"hi"}]}`)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("build req: %v", err)
	}

	slot := &captureSlot{}
	ctx := context.WithValue(WithSessionID(req.Context(), "session-abc"), captureSlotKey{}, slot)
	req = req.WithContext(ctx)

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := capture.req.Header.Get("x-session-id"); got != "session-abc" {
		t.Errorf("x-session-id=%q, want session-abc", got)
	}

	var out map[string]any

	uerr := json.Unmarshal(capture.body, &out)
	if uerr != nil {
		t.Fatalf("unmarshal rewritten request body: %v", uerr)
	}

	if _, ok := out["cache_control"]; !ok {
		t.Errorf("rewritten request body missing cache_control: %s", capture.body)
	}

	// Read the response body to drive the TeeReader, then assert the slot is populated.
	_, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("read resp body: %v", rerr)
	}

	slot.parse()

	if slot.cachedTokens != 80 {
		t.Errorf("slot.cachedTokens=%d, want 80", slot.cachedTokens)
	}
}

func TestRoundTrip_OpenRouter_OmitsCacheControlForNonAnthropic(t *testing.T) {
	t.Parallel()

	capture := &captureTransport{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"usage":{}}`))),
			Header:     http.Header{},
		},
	}
	tr := &openRouterTransport{base: capture}

	bodyJSON := []byte(`{"model":"openai/gpt-4o","messages":[{"role":"user","content":"hi"}]}`)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", bytes.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("build req: %v", err)
	}

	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var out map[string]any

	uerr := json.Unmarshal(capture.body, &out)
	if uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}

	if _, has := out["cache_control"]; has {
		t.Errorf("non-Anthropic model got cache_control: %s", capture.body)
	}
}

// captureTransport records the request and body it receives, then returns a
// pre-canned response. Used to assert openRouterTransport mutations.
type captureTransport struct {
	req  *http.Request
	body []byte
	resp *http.Response
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.req = req

	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err //nolint:wrapcheck // test fixture, surface err verbatim
		}

		c.body = b
	}

	return c.resp, nil
}
