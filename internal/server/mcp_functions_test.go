package server

import (
	"encoding/json"
	"testing"

	"github.com/jtarchie/topbanana/internal/agent"
)

func TestMCPFunctionPath(t *testing.T) {
	t.Parallel()
	if got := mcpFunctionPath("submit"); got != "functions/submit.js" {
		t.Errorf("mcpFunctionPath(submit) = %q", got)
	}
}

// TestConfigureSiteInputOptional pins the contract configure_site relies on:
// pointer fields the agent omits stay nil (left untouched), while a field it
// sends — even to a zero value like private:false — is decoded as set.
func TestConfigureSiteInputOptional(t *testing.T) {
	t.Parallel()

	var partial configureSiteInput
	err := json.Unmarshal([]byte(`{"slug":"x","private":true}`), &partial)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if partial.Title != nil || partial.Description != nil || partial.EnableFunctions != nil || partial.EnablePublicAPI != nil {
		t.Error("omitted fields must decode to nil so they're left untouched")
	}
	if partial.Private == nil || !*partial.Private {
		t.Error("private:true must decode to a set pointer")
	}

	var zero configureSiteInput
	err = json.Unmarshal([]byte(`{"slug":"x","private":false}`), &zero)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if zero.Private == nil || *zero.Private {
		t.Error("private:false must decode to a set pointer holding false (distinct from omitted)")
	}
}

// TestFunctionsGuideExposed confirms the embedded functions contract is reachable
// through the accessor the topbanana://guide/functions resource serves.
func TestFunctionsGuideExposed(t *testing.T) {
	t.Parallel()
	if agent.FunctionsGuide() == "" {
		t.Fatal("FunctionsGuide() is empty")
	}
	if agent.FunctionsGuide() == agent.AuthoringGuide() {
		t.Error("functions and authoring guides should differ")
	}
}
