// Package model resolves a provider/model string to an ADK LLM.
package model

import (
	"fmt"
	"strings"

	genaianthropic "github.com/achetronic/adk-utils-go/genai/anthropic"
	genaiopenai "github.com/achetronic/adk-utils-go/genai/openai"
	adkmodel "google.golang.org/adk/model"
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

// Resolve constructs an ADK LLM for the given provider + model + API key.
// If baseURL is non-empty it overrides the provider default (useful for local
// OpenAI-compatible servers like LM Studio / llama.cpp / vLLM).
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
		return genaiopenai.New(genaiopenai.Config{
			APIKey:    apiKey,
			BaseURL:   baseURL,
			ModelName: name,
		}), nil
	}
}
