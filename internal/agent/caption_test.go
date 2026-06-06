package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// fakeLLM is a minimal adkmodel.LLM that yields a single fixed text response.
// The adk LLM interface is just Name() + GenerateContent, so the fake is cheap.
type fakeLLM struct {
	name string
	text string
	err  error
}

func (f fakeLLM) Name() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}

func (f fakeLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if f.err != nil {
			yield(nil, f.err)
			return
		}
		resp := &adkmodel.LLMResponse{
			Content: &genai.Content{
				Parts: []*genai.Part{{Text: f.text}},
			},
		}
		yield(resp, nil)
	}
}

// TestCaptionAssetNilLLM is the regression guard for the production panic: a nil
// LLM must return an error, not deref. The test simply not panicking is the
// assertion that matters.
func TestCaptionAssetNilLLM(t *testing.T) {
	c, err := CaptionAsset(context.Background(), nil, []byte("not really a png"), "image/png")
	if err == nil {
		t.Fatalf("expected an error for a nil LLM, got caption %+v", c)
	}
	if c.Alt != "" || c.Description != "" {
		t.Fatalf("expected zero caption on error, got %+v", c)
	}
}

func TestCaptionAssetSVGSkipped(t *testing.T) {
	_, err := CaptionAsset(context.Background(), fakeLLM{}, []byte("<svg/>"), "image/svg+xml")
	if err == nil {
		t.Fatal("expected svg captioning to be unsupported")
	}
}

func TestCaptionAssetHappyPath(t *testing.T) {
	llm := fakeLLM{text: `{"alt":"a tabby cat","description":"a tabby cat sleeping on a couch"}`}
	c, err := CaptionAsset(context.Background(), llm, []byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Alt != "a tabby cat" {
		t.Errorf("alt = %q, want %q", c.Alt, "a tabby cat")
	}
	if c.Description != "a tabby cat sleeping on a couch" {
		t.Errorf("description = %q", c.Description)
	}
}

func TestCaptionAssetTruncatesAlt(t *testing.T) {
	long := strings.Repeat("x", altMaxLen+50)
	llm := fakeLLM{text: `{"alt":"` + long + `","description":"d"}`}
	c, err := CaptionAsset(context.Background(), llm, []byte("png"), "image/png")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(c.Alt) != altMaxLen {
		t.Errorf("alt length = %d, want %d", len(c.Alt), altMaxLen)
	}
}

func TestCaptionAssetWrapsResponseInProse(t *testing.T) {
	// Models often ignore the JSON-only instruction; extractJSON should still
	// pull the object out of chatty prose / code fences.
	llm := fakeLLM{text: "Sure! Here you go:\n```json\n{\"alt\":\"a dog\",\"description\":\"a dog\"}\n```"}
	c, err := CaptionAsset(context.Background(), llm, []byte("png"), "image/png")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Alt != "a dog" {
		t.Errorf("alt = %q, want %q", c.Alt, "a dog")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare", `{"a":1}`, `{"a":1}`},
		{"fenced", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"prose", `here: {"a":1} thanks`, `{"a":1}`},
		{"none", `no json here`, ``},
		{"empty", ``, ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractJSON(tc.in)
			if got != tc.want {
				t.Errorf("extractJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
