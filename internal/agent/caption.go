package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// captionInstruction asks the LLM for a tiny structured response. We keep it
// strict because local models (LM Studio / Ollama) often ignore
// ResponseMIMEType, and we'd rather fail closed and let the caller fall back
// to a filename-based alt than store garbage in S3 metadata.
const captionInstruction = `Look at the image and respond with ONLY a single-line JSON object — no prose, no code fences:
{"alt":"<accessible alt text, under 125 chars>","description":"<one short sentence about what the image shows and what it could be used for on a website>"}`

const altMaxLen = 125

// Caption is what the upload handler stores on the S3 object as
// x-amz-meta-alt and x-amz-meta-description, and what list_assets returns to
// the agent so it can place images intelligently.
type Caption struct {
	Alt         string `json:"alt"`
	Description string `json:"description"`
}

// CaptionAsset asks the configured LLM to describe an image. SVGs are skipped
// because most multimodal models render rasters, not vectors — captioning
// them produces low-quality results and we'd rather have no caption than a
// misleading one.
func CaptionAsset(ctx context.Context, llm adkmodel.LLM, content []byte, mimeType string) (Caption, error) {
	if mimeType == "image/svg+xml" {
		return Caption{}, errors.New("svg captioning not supported")
	}

	req := &adkmodel.LLMRequest{
		Model: llm.Name(),
		Contents: []*genai.Content{
			{
				Role: "user",
				Parts: []*genai.Part{
					genai.NewPartFromBytes(content, mimeType),
					genai.NewPartFromText(captionInstruction),
				},
			},
		},
	}

	var text strings.Builder
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return Caption{}, fmt.Errorf("vision call: %w", err)
		}
		if resp == nil || resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p.Text != "" {
				text.WriteString(p.Text)
			}
		}
	}

	raw := extractJSON(text.String())
	if raw == "" {
		return Caption{}, fmt.Errorf("no JSON in vision output: %q", text.String())
	}

	var c Caption
	err := json.Unmarshal([]byte(raw), &c)
	if err != nil {
		return Caption{}, fmt.Errorf("parse caption JSON %q: %w", raw, err)
	}
	c.Alt = strings.TrimSpace(c.Alt)
	c.Description = strings.TrimSpace(c.Description)
	if len(c.Alt) > altMaxLen {
		c.Alt = c.Alt[:altMaxLen]
	}
	return c, nil
}

// extractJSON pulls the first {...} block out of a model response, which lets
// us cope with a model that wraps its output in code fences or chatty prose
// despite ResponseMIMEType=application/json.
func extractJSON(s string) string {
	start := strings.IndexByte(s, '{')
	end := strings.LastIndexByte(s, '}')
	if start == -1 || end == -1 || end <= start {
		return ""
	}
	return s[start : end+1]
}
