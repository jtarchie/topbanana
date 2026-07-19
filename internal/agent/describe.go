package agent

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/topbanana/internal/store"
)

// describeInstruction asks the LLM for a tiny structured response. Strict
// JSON keeps us tolerant of local models that ignore ResponseMIMEType — we'd
// rather fail closed than store garbage in the sidecar. Body lives in
// describe_prompt.md.
//
//go:embed describe_prompt.md
var describeInstruction string

const (
	titleMaxLen          = 80
	descriptionMaxLen    = 280
	indexHTMLTruncateLen = 8000
)

// SiteDescription is the bit we persist into SiteMeta so the Available Apps
// page can show something more useful than a random slug.
type SiteDescription struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// DescribeSite asks the configured LLM to summarise a built site. Inputs are
// the user's build prompt, the index.html (truncated), and a short list of
// the other pages — enough context for a one-line summary without paying for
// the whole site on every build.
func DescribeSite(ctx context.Context, llm adkmodel.LLM, s *store.Store, slug, userPrompt string) (SiteDescription, error) {
	prompt, err := buildDescribePrompt(ctx, s, slug, userPrompt)
	if err != nil {
		return SiteDescription{}, err
	}

	raw, err := runDescribeLLM(ctx, llm, prompt)
	if err != nil {
		return SiteDescription{}, err
	}

	var d SiteDescription
	err = json.Unmarshal([]byte(raw), &d)
	if err != nil {
		return SiteDescription{}, fmt.Errorf("parse describe JSON %q: %w", raw, err)
	}
	d.Title = clamp(strings.TrimSpace(d.Title), titleMaxLen)
	d.Description = clamp(strings.TrimSpace(d.Description), descriptionMaxLen)
	if d.Title == "" && d.Description == "" {
		return SiteDescription{}, errors.New("empty describe response")
	}
	return d, nil
}

// buildDescribePrompt assembles the single-shot user message: build prompt,
// other page names, and index.html (truncated to keep the call cheap).
func buildDescribePrompt(ctx context.Context, s *store.Store, slug, userPrompt string) (string, error) {
	index, err := s.Read(ctx, slug, "index.html")
	if err != nil {
		return "", fmt.Errorf("read index.html: %w", err)
	}
	if index.Content == "" {
		return "", fmt.Errorf("site %q has no index.html", slug)
	}

	body := index.Content
	if len(body) > indexHTMLTruncateLen {
		body = body[:indexHTMLTruncateLen]
	}

	files, err := s.List(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("list files: %w", err)
	}
	otherPages := otherHTMLPages(files)

	var b strings.Builder
	b.WriteString("User's build prompt:\n")
	b.WriteString(strings.TrimSpace(userPrompt))
	b.WriteString("\n\n")
	if len(otherPages) > 0 {
		b.WriteString("Other pages on the site: ")
		b.WriteString(strings.Join(otherPages, ", "))
		b.WriteString("\n\n")
	}
	b.WriteString("index.html:\n")
	b.WriteString(body)
	b.WriteString("\n\n")
	b.WriteString(describeInstruction)
	return b.String(), nil
}

// runDescribeLLM does the LLM call and extracts the first JSON object from
// the response. Returns the raw JSON string ready for unmarshal.
func runDescribeLLM(ctx context.Context, llm adkmodel.LLM, prompt string) (string, error) {
	req := &adkmodel.LLMRequest{
		Model: llm.Name(),
		Contents: []*genai.Content{
			{
				Role: "user",
				Parts: []*genai.Part{
					genai.NewPartFromText(prompt),
				},
			},
		},
	}

	var text strings.Builder
	for resp, err := range llm.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", fmt.Errorf("describe call: %w", err)
		}
		appendResponseText(&text, resp)
	}

	raw := extractJSON(text.String())
	if raw == "" {
		return "", fmt.Errorf("no JSON in describe output: %q", text.String())
	}
	return raw, nil
}

func appendResponseText(dst *strings.Builder, resp *adkmodel.LLMResponse) {
	if resp == nil || resp.Content == nil {
		return
	}
	for _, p := range resp.Content.Parts {
		if p.Text != "" {
			dst.WriteString(p.Text)
		}
	}
}

func otherHTMLPages(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if f == "index.html" {
			continue
		}
		if !strings.HasSuffix(f, ".html") {
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

func clamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
