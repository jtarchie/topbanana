package agent

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// TestFunctionsPromptMatchesBindings catches the kind of silent drift that
// happened during Vibe Backend's Phase 2: a new sandbox binding shipped, the
// prompt didn't follow, and the LLM had to learn the API by reading skeleton
// examples. This test parses the actual `kv.Set(...)` / `resp.Set(...)`
// registrations out of the sandbox source and asserts every name appears in
// functions_prompt.md — and vice versa, so the prompt can't reference a method
// that doesn't exist.
//
// When you add a new binding in internal/sandbox/, this test will fail until
// you update functions_prompt.md to document it.
func TestFunctionsPromptMatchesBindings(t *testing.T) {
	prompt, err := os.ReadFile("functions_prompt.md")
	if err != nil {
		t.Fatalf("read functions_prompt.md: %v", err)
	}
	promptText := string(prompt)

	cases := []struct {
		label     string // human-readable name for the failure message
		jsObject  string // the global name the JS handler uses ("kv", "response")
		sourceGo  string // path to the Go file that registers the bindings
		goVarName string // the local variable in Go that gets .Set("name", ...) called on it
	}{
		{
			label:     "kv",
			jsObject:  "kv",
			sourceGo:  "../sandbox/kv.go",
			goVarName: "kv",
		},
		{
			label:     "response",
			jsObject:  "response",
			sourceGo:  "../sandbox/sandbox.go",
			goVarName: "resp",
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			registered := extractGoBindings(t, tc.sourceGo, tc.goVarName)
			documented := extractPromptMentions(promptText, tc.jsObject)

			regSet := toSet(registered)
			docSet := toSet(documented)

			var missingFromPrompt []string
			for _, name := range registered {
				if !docSet[name] {
					missingFromPrompt = append(missingFromPrompt, name)
				}
			}
			sort.Strings(missingFromPrompt)
			if len(missingFromPrompt) > 0 {
				t.Errorf("%s bindings registered in sandbox but NOT documented in functions_prompt.md: %v\nAdd them to the prompt or the LLM won't know they exist.", tc.label, missingFromPrompt)
			}

			var extraInPrompt []string
			for _, name := range documented {
				if !regSet[name] {
					extraInPrompt = append(extraInPrompt, name)
				}
			}
			sort.Strings(extraInPrompt)
			if len(extraInPrompt) > 0 {
				t.Errorf("%s methods mentioned in functions_prompt.md but NOT registered in sandbox: %v\nEither add the binding or remove the doc.", tc.label, extraInPrompt)
			}
		})
	}
}

// extractGoBindings scans path for occurrences of `varName.Set("name", ...)`
// and returns the deduplicated list of names. This mirrors how installKV and
// installResponseBuilder register their methods on the goja object.
func extractGoBindings(t *testing.T, path, varName string) []string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	// e.g. `kv.Set("get", ...)` or `resp.Set("redirect", ...)`. Names are
	// lowercase ASCII; we use [a-z]+ to avoid matching internal helpers that
	// happen to have a Set call on something unrelated.
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(varName) + `\.Set\("([a-z]+)"`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	seen := map[string]bool{}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// extractPromptMentions finds every `<object>.<method>(` reference in the
// prompt text. Documenting a method without showing a call is a code smell —
// the LLM picks up the surface from the example, not the prose.
func extractPromptMentions(text, jsObject string) []string {
	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(jsObject) + `\.([a-z]+)\(`)
	matches := re.FindAllStringSubmatch(text, -1)
	seen := map[string]bool{}
	out := []string{}
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func toSet(xs []string) map[string]bool {
	out := make(map[string]bool, len(xs))
	for _, x := range xs {
		out[x] = true
	}
	return out
}
