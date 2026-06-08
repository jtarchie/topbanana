package agent

import (
	"strings"
	"testing"
)

// TestGuideAccessors confirms the exported accessors surface the embedded
// prompts the MCP server serves as resources, with their load-bearing tokens
// intact: the authoring contract names the /app.css substrate, and the
// functions contract documents the CommonJS handler shape.
func TestGuideAccessors(t *testing.T) {
	t.Parallel()
	if !strings.Contains(AuthoringGuide(), "/app.css") {
		t.Error("AuthoringGuide() should mention the /app.css substrate")
	}
	if !strings.Contains(FunctionsGuide(), "module.exports") {
		t.Error("FunctionsGuide() should document module.exports")
	}
}

// TestEmbeddedPromptsNonEmpty guards against a sibling *_prompt.md file being
// emptied or accidentally truncated. //go:embed errors at compile time if a
// file is missing, but a zero-byte file would slip through and the LLM would
// silently get an empty instruction.
func TestEmbeddedPromptsNonEmpty(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"systemPrompt":        systemPrompt,
		"functionsPrompt":     functionsPrompt,
		"describeInstruction": describeInstruction,
		"captionInstruction":  captionInstruction,
	} {
		if body == "" {
			t.Errorf("%s embedded prompt is empty — was the .md file emptied?", name)
		}
	}
}
