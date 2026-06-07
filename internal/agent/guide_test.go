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
