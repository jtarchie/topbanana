package model_test

import (
	"testing"

	"google.golang.org/genai"

	"github.com/jtarchie/topbanana/internal/model"
)

func TestSplitModel(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in           string
		wantProvider string
		wantName     string
	}{
		{"", "", ""},
		{"lmstudio", "lmstudio", "lmstudio"},  // no slash: whole string for both
		{"openai/gpt-4o", "openai", "gpt-4o"}, // one slash
		{"lmstudio/google/gemma-4-26b-a4b", "lmstudio", "google/gemma-4-26b-a4b"}, // splits on the FIRST slash only
		{"anthropic/claude-opus-4-8", "anthropic", "claude-opus-4-8"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			gotProvider, gotName := model.SplitModel(tc.in)
			if gotProvider != tc.wantProvider || gotName != tc.wantName {
				t.Errorf("SplitModel(%q) = (%q, %q), want (%q, %q)", tc.in, gotProvider, gotName, tc.wantProvider, tc.wantName)
			}
		})
	}
}

func TestParseReasoningEffort(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		want    genai.ThinkingLevel
		wantErr bool
	}{
		{"", "", false},
		{"none", "", false},
		{"NONE", "", false},     // case-insensitive
		{"  none  ", "", false}, // trimmed
		{"minimal", genai.ThinkingLevelMinimal, false},
		{"low", genai.ThinkingLevelLow, false},
		{"Medium", genai.ThinkingLevelMedium, false},
		{"HIGH", genai.ThinkingLevelHigh, false},
		{"bogus", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := model.ParseReasoningEffort(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseReasoningEffort(%q) = nil error, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReasoningEffort(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseReasoningEffort(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()

	t.Run("anthropic returns a client", func(t *testing.T) {
		t.Parallel()
		llm, err := model.Resolve("anthropic", "claude-opus-4-8", "key", "")
		if err != nil {
			t.Fatalf("Resolve(anthropic) error: %v", err)
		}
		if llm == nil {
			t.Fatal("Resolve(anthropic) returned nil LLM")
		}
	})

	t.Run("known openai-compatible provider uses its default base URL", func(t *testing.T) {
		t.Parallel()
		for provider := range model.DefaultBaseURLs {
			llm, err := model.Resolve(provider, "some-model", "key", "")
			if err != nil {
				t.Errorf("Resolve(%q) error: %v", provider, err)
				continue
			}
			if llm == nil {
				t.Errorf("Resolve(%q) returned nil LLM", provider)
			}
		}
	})

	t.Run("unknown provider with no base URL errors", func(t *testing.T) {
		t.Parallel()
		_, err := model.Resolve("madeup", "model", "key", "")
		if err == nil {
			t.Fatal("Resolve(unknown provider, no baseURL) = nil error, want error")
		}
	})

	t.Run("explicit base URL overrides an unknown provider", func(t *testing.T) {
		t.Parallel()
		llm, err := model.Resolve("madeup", "model", "key", "http://localhost:9999/v1")
		if err != nil {
			t.Fatalf("Resolve(unknown provider, explicit baseURL) error: %v", err)
		}
		if llm == nil {
			t.Fatal("Resolve with explicit baseURL returned nil LLM")
		}
	})
}
