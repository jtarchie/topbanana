# internal/model

`internal/model/openrouter_cache.go` wraps the adk-utils-go OpenAI adapter for the `openrouter` provider with a process-wide `http.DefaultClient.Transport` that scope-checks `openrouter.ai` and per-request does three things lifted straight from OpenRouter's documented prompt-caching API:

1. **`x-session-id` header** — read from ctx via `model.WithSessionID(ctx, logKey)` (called once in `build.Service.Start` with `p.LogKey`). Activates OpenRouter's *provider sticky routing* on the very first request of a build/edit, keeping subsequent turns on the same provider endpoint so the prompt cache stays warm. Universal — every provider OpenRouter routes to honors it.
2. **Top-level `cache_control: {type: "ephemeral"}` body field** — injected only when the request's `model` starts with `anthropic/` or `~anthropic/`. Enables Anthropic's auto-advancing rolling-tail cache (one breakpoint that covers the growing message history). Other providers (OpenAI / DeepSeek / Grok / Groq / Moonshot auto-cache; Gemini 2.5 implicit; Qwen / Anthropic explicit) get nothing — auto-cache providers handle it themselves and forcing Anthropic-direct routing would break non-Anthropic calls.
3. **Tees response body into a per-call captureSlot** held in ctx. The adapter's `convertUsageMetadata` drops `usage.prompt_tokens_details.cached_tokens`; we parse it from the raw body (or the final SSE `data:` event when streaming) and stitch it onto `genai.UsageMetadata.CachedContentTokenCount`, which `agent.Usage.add` already plumbs into `editrec.Usage.Cached` and the `/debug/{slug}/edit` cache-hit ratio.

`provider.order` is intentionally **never** set in the request body — the docs note sticky routing falls back to inactive when manual provider order is supplied.
