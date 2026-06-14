// openrouter_cache.go wires OpenRouter's prompt-caching surface into the
// adk-utils-go OpenAI adapter without forking it. The adapter exposes only a
// HTTPOptions.Headers seam — too coarse for per-request behaviour — so we
// install a process-wide http.Transport on http.DefaultClient that scope-
// checks the host (openrouter.ai only) and per-request decides:
//
//  1. x-session-id header. Read from ctx via WithSessionID. Enables OpenRouter
//     provider sticky routing from the very first request of a session — the
//     "universal" lever per the docs: works with implicit caching (OpenAI,
//     DeepSeek, Grok, Groq, Moonshot, Gemini 2.5) and explicit caching
//     (Anthropic, Qwen). Without it sticky routing only activates after a
//     cache hit is observed, which is too late for short builds.
//
//  2. Top-level cache_control: {type: ephemeral} body field. Injected only
//     when the request's model field routes to Anthropic (model starts with
//     "anthropic/" or "~anthropic/"). Enables auto-advancing rolling-tail
//     caching across multi-turn conversations. The marker forces Anthropic-
//     direct routing (Bedrock and Vertex ignore it), which is fine because we
//     only emit it when the caller has already chosen Anthropic.
//
//  3. Response body tee into a per-call captureSlot held in ctx. The adapter
//     drops usage.prompt_tokens_details.cached_tokens during its genai
//     conversion; we parse it from the raw body and stitch it back onto the
//     genai UsageMetadata.CachedContentTokenCount that agent.Usage.add already
//     reads. Handles both non-streaming JSON and SSE chunked responses.
//
// All other traffic (anthropic-direct, lmstudio, openai-direct, etc.) flows
// through the wrapped base transport untouched.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"sync"

	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	adkmodel "google.golang.org/adk/model"
)

// Max length per OpenRouter docs for the session_id field.
const maxSessionIDLen = 256

type sessionIDKey struct{}
type captureSlotKey struct{}

// WithSessionID returns a derived context that carries an OpenRouter session
// identifier. The OpenRouter http.Transport (installed on http.DefaultClient
// when an openrouter-backed LLM is resolved) reads it via sessionIDFromContext
// and sets it as the x-session-id request header. Empty or over-long values
// leave ctx unchanged — the OpenRouter docs cap session_id at 256 chars.
//
// Call this once at the top of a build/edit run with the editrec LogKey (or
// any stable per-run identifier) so every LLM call in that run lands on the
// same provider endpoint, keeping the prompt cache warm.
func WithSessionID(ctx context.Context, id string) context.Context {
	if id == "" || len(id) > maxSessionIDLen {
		return ctx
	}

	return context.WithValue(ctx, sessionIDKey{}, id)
}

func sessionIDFromContext(ctx context.Context) string {
	s, _ := ctx.Value(sessionIDKey{}).(string)

	return s
}

// captureSlot accumulates response body bytes (whatever openai-go consumes
// from the upstream HTTP body) and parses cached-token metrics on demand. One
// slot per LLM call, threaded via ctx so the http.Transport — which only sees
// raw bytes — can populate it for the LLM wrapper, which only sees genai
// shapes, to read back. Guarded by a mutex because the TeeReader writes
// concurrently with the wrapper's read loop on the very last response chunk.
type captureSlot struct {
	mu            sync.Mutex
	buf           bytes.Buffer
	cachedTokens  int64
	cacheDiscount float64
	parsed        bool
}

func (s *captureSlot) write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.buf.Write(p) //nolint:wrapcheck // bytes.Buffer.Write never errors; surface signature verbatim
}

// parse extracts usage.prompt_tokens_details.cached_tokens (and the top-level
// cache_discount field) from the buffered response body. Handles two shapes:
//
//  1. Non-streaming: the whole body is one JSON document, json.Unmarshal hits.
//  2. SSE streaming: the body is a sequence of `data: {...}` lines, the last
//     of which carries usage. We walk lines from the end looking for the most
//     recent payload that decodes with a usage block.
//
// Idempotent: once parsed (or once a parse fails) the slot is sealed.
func (s *captureSlot) parse() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.parsed {
		return
	}
	defer func() { s.parsed = true }()

	body := s.buf.Bytes()
	if len(body) == 0 {
		return
	}

	if cached, discount, ok := extractCacheFields(body); ok {
		s.cachedTokens, s.cacheDiscount = cached, discount

		return
	}
	// SSE fallback: walk newline-separated lines from the tail.
	for end := len(body); end > 0; {
		start := bytes.LastIndexByte(body[:end], '\n') + 1
		line := bytes.TrimSpace(body[start:end])
		end = start - 1

		if !bytes.HasPrefix(line, []byte("data:")) {
			if end <= 0 {
				return
			}

			continue
		}

		payload := bytes.TrimSpace(line[len("data:"):])
		if cached, discount, ok := extractCacheFields(payload); ok {
			s.cachedTokens, s.cacheDiscount = cached, discount

			return
		}

		if end <= 0 {
			return
		}
	}
}

func extractCacheFields(body []byte) (cached int64, discount float64, ok bool) {
	var doc struct {
		Usage struct {
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		CacheDiscount float64 `json:"cache_discount"`
	}

	err := json.Unmarshal(body, &doc)
	if err != nil {
		return 0, 0, false
	}

	return doc.Usage.PromptTokensDetails.CachedTokens, doc.CacheDiscount, true
}

// openRouterTransport scopes its interception to api.openrouter.ai's chat-
// completions path; everything else passes through unchanged. Within scope it
// injects x-session-id + cache_control and tees the response body.
type openRouterTransport struct {
	base http.RoundTripper
}

func (t *openRouterTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isOpenRouterChatCompletion(req) {
		return t.base.RoundTrip(req) //nolint:wrapcheck // pass through verbatim
	}

	if sid := sessionIDFromContext(req.Context()); sid != "" {
		req.Header.Set("x-session-id", sid)
	}

	err := maybeInjectCacheControl(req)
	if err != nil {
		return nil, fmt.Errorf("inject cache_control: %w", err)
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err //nolint:wrapcheck // surface base RoundTripper errors verbatim
	}

	if slot, ok := req.Context().Value(captureSlotKey{}).(*captureSlot); ok && slot != nil {
		resp.Body = teeBody(resp.Body, slot)
	}

	return resp, nil
}

func isOpenRouterChatCompletion(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}

	if !strings.HasSuffix(req.URL.Host, "openrouter.ai") {
		return false
	}

	return strings.HasSuffix(req.URL.Path, "/chat/completions")
}

// maybeInjectCacheControl reads the request body once, parses the JSON, and
// for Anthropic-routed models stamps `cache_control: {type: "ephemeral"}` at
// the top level. The body is always re-wrapped (cache_control or not) because
// ReadAll consumed the original io.ReadCloser.
//
// Anthropic detection is by model-string prefix because OpenRouter accepts
// both `anthropic/<model>` and the auto-routing `~anthropic/<model>` form; the
// caller's model selection is the source of truth, not request URL.
func maybeInjectCacheControl(req *http.Request) error {
	if req.Body == nil {
		return nil
	}

	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()

	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	var doc map[string]any

	jerr := json.Unmarshal(body, &doc)
	if jerr == nil {
		if name, _ := doc["model"].(string); isAnthropicModel(name) {
			doc["cache_control"] = map[string]string{"type": "ephemeral"}

			rewritten, merr := json.Marshal(doc)
			if merr == nil {
				body = rewritten
			}
		}
	}

	req.ContentLength = int64(len(body))
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }

	return nil
}

func isAnthropicModel(name string) bool {
	return strings.HasPrefix(name, "anthropic/") || strings.HasPrefix(name, "~anthropic/")
}

func teeBody(body io.ReadCloser, slot *captureSlot) io.ReadCloser {
	return &teeReadCloser{r: io.TeeReader(body, writerFunc(slot.write)), c: body}
}

type teeReadCloser struct {
	r io.Reader
	c io.Closer
}

func (t *teeReadCloser) Read(p []byte) (int, error) { return t.r.Read(p) } //nolint:wrapcheck
func (t *teeReadCloser) Close() error               { return t.c.Close() } //nolint:wrapcheck

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// installOnce wraps http.DefaultClient.Transport with our interceptor exactly
// once per process. Idempotent so test setups and repeated Resolve calls don't
// stack interceptors. We use http.DefaultClient because adk-utils-go's
// genaiopenai adapter constructs openai.NewClient without an explicit
// option.WithHTTPClient, which falls back to the SDK's default — i.e.
// http.DefaultClient. If adk-utils-go ever exposes a per-Config http.Client
// seam, switch to that and drop the global.
var installOnce sync.Once

func installOpenRouterTransport() {
	installOnce.Do(func() {
		base := http.DefaultClient.Transport
		if base == nil {
			base = http.DefaultTransport
		}

		http.DefaultClient.Transport = &openRouterTransport{base: base}
	})
}

// cachingLLM wraps genaiopenai.Model so each LLM call gets a fresh captureSlot
// in its ctx and any non-zero cached_tokens parsed from the response body is
// stitched back onto the genai UsageMetadata that the adapter would otherwise
// leave at zero. Session-ID injection and cache_control body rewriting happen
// in the underlying http.Transport — they don't need a per-call hook here.
type cachingLLM struct {
	underlying adkmodel.LLM
	modelName  string
}

func newCachingOpenRouter(cfg genaiopenai.Config) adkmodel.LLM {
	installOpenRouterTransport()

	return &cachingLLM{
		underlying: genaiopenai.New(cfg),
		modelName:  cfg.ModelName,
	}
}

func (m *cachingLLM) Name() string { return m.modelName }

func (m *cachingLLM) GenerateContent(
	ctx context.Context,
	req *adkmodel.LLMRequest,
	stream bool,
) iter.Seq2[*adkmodel.LLMResponse, error] {
	slot := &captureSlot{}
	ctx = context.WithValue(ctx, captureSlotKey{}, slot)
	base := m.underlying.GenerateContent(ctx, req, stream)

	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for resp, err := range base {
			if resp != nil && resp.UsageMetadata != nil {
				slot.parse()

				if slot.cachedTokens > 0 && slot.cachedTokens < int64(1<<31) {
					resp.UsageMetadata.CachedContentTokenCount = int32(slot.cachedTokens)
				}
			}

			if !yield(resp, err) {
				return
			}
		}
	}
}
