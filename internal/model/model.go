// Package model resolves a provider/model string to an ADK LLM.
package model

import (
	"fmt"
	"strings"

	genaianthropic "github.com/achetronic/adk-utils-go/genai/anthropic"
	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// DefaultBaseURLs maps OpenAI-compatible provider names to their API endpoints.
var DefaultBaseURLs = map[string]string{
	"openai":     "https://api.openai.com/v1",
	"openrouter": "https://openrouter.ai/api/v1",
	"ollama":     "http://localhost:11434/v1",
	"lmstudio":   "http://localhost:1234/v1",
}

// SplitModel parses "provider/model-name" into (provider, model-name).
// "lmstudio/google/gemma-4-26b-a4b" -> ("lmstudio", "google/gemma-4-26b-a4b").
func SplitModel(s string) (provider, name string) {
	idx := strings.Index(s, "/")
	if idx < 0 {
		return s, s
	}
	return s[:idx], s[idx+1:]
}

// ParseReasoningEffort converts a CLI string into a genai.ThinkingLevel.
// Empty input + "none" both map to the unspecified zero value (no thinking).
// Returned errors are user-facing — wire them straight to a startup failure
// so a typo never silently disables reasoning.
//
// On OpenRouter, the adk-utils-go OpenAI adapter passes ThinkingLevel through
// as the request's reasoning effort, which OpenRouter then maps to whichever
// per-model API expects it (Google's thinkingLevel for Gemini 3, thinkingBudget
// for Gemini 2.5, Anthropic's thinking blocks, Qwen's enable_thinking, etc).
func ParseReasoningEffort(s string) (genai.ThinkingLevel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "none":
		return "", nil
	case "minimal":
		return genai.ThinkingLevelMinimal, nil
	case "low":
		return genai.ThinkingLevelLow, nil
	case "medium":
		return genai.ThinkingLevelMedium, nil
	case "high":
		return genai.ThinkingLevelHigh, nil
	default:
		return "", fmt.Errorf("unknown reasoning effort %q (use minimal|low|medium|high or none)", s)
	}
}

// Resolve constructs an ADK LLM for the given provider + model + API key.
// If baseURL is non-empty it overrides the provider default (useful for local
// OpenAI-compatible servers like LM Studio / llama.cpp / vLLM).
//
// OpenRouter is wrapped by newCachingOpenRouter so every request carries an
// x-session-id header read from ctx via WithSessionID (sticky routing —
// universal across providers) and Anthropic-routed models get a top-level
// cache_control: {type: ephemeral} marker (rolling-tail prompt caching).
// Both lift directly from OpenRouter's documented prompt-caching API; see
// internal/model/openrouter_cache.go for the wiring.
func Resolve(provider, name, apiKey, baseURL string) (adkmodel.LLM, error) {
	switch provider {
	case "anthropic":
		return genaianthropic.New(genaianthropic.Config{
			APIKey:    apiKey,
			ModelName: name,
		}), nil
	default:
		if baseURL == "" {
			def, ok := DefaultBaseURLs[provider]
			if !ok {
				return nil, fmt.Errorf("unknown provider %q: set LLM_BASE_URL, or use anthropic/openai/openrouter/ollama/lmstudio", provider)
			}
			baseURL = def
		}

		cfg := genaiopenai.Config{
			APIKey:    apiKey,
			BaseURL:   baseURL,
			ModelName: name,
		}

		if provider == "openrouter" {
			return newCachingOpenRouter(cfg), nil
		}

		return genaiopenai.New(cfg), nil
	}
}
