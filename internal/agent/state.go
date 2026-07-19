package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/jtarchie/topbanana/internal/textedit"
)

// This file owns per-run agent state and the small utilities the runner loop
// and tool closures share: the size/iteration caps, buildState, the anti-loop
// toolGuard + signatures, the budget hint, and the path/name validation shims
// that delegate to internal/textedit. Extracted from agent.go.

const (
	functionsDir = "functions/"
	jsExt        = ".js"
)

const (
	maxHTMLFileBytes   = 256 * 1024
	maxHTMLFiles       = 25
	maxAgentIterations = 60
	toolGuardRingLen   = 3

	maxQuestionsPerBuild = 3
	askQuestionTimeout   = 5 * time.Minute
)

// buildState bundles per-run state that the runner loop and tool closures
// both touch. iters is incremented from the ADK event loop in Run; tool
// closures read it via Load() to surface a "iterations remaining" hint to
// the model so it can self-pace.
//
// writeMu serializes every mutating tool against the store. ADK dispatches
// each function call in a separate goroutine (base_flow.handleFunctionCalls),
// and OpenAI/Anthropic both default to parallel tool calls — so two
// edit_file/replace_lines/write_file calls on the same path would otherwise
// race at S3, where there's no per-key locking and last-write-wins silently
// drops work. Reads stay lock-free so the model still gets fan-out wins on
// list_files / read_file / grep_files batches.
type buildState struct {
	guard          *toolGuard
	iters          atomic.Int64
	writeMu        sync.Mutex
	questionsAsked atomic.Int32
}

func newBuildState() *buildState {
	return &buildState{guard: &toolGuard{}}
}

// countFunctionCalls returns the number of function-call parts on an ADK
// event. ADK dispatches each call in its own goroutine
// (base_flow.handleFunctionCalls), so a return value > 1 means parallel tool
// execution actually happened on this turn — useful for confirming the model
// is batching and that buildState.writeMu is doing real work.
func countFunctionCalls(ev *session.Event) int {
	if ev == nil || ev.Content == nil {
		return 0
	}
	n := 0
	for _, part := range ev.Content.Parts {
		if part != nil && part.FunctionCall != nil {
			n++
		}
	}
	return n
}

// toolGuard rejects the same write/edit being issued twice in a short
// window. Catches the most common failure mode beyond ADK's iteration cap:
// the model loops on the same fix idea, burning iterations without making
// progress. The existing OldText == NewText no-op short-circuit in edit_file
// fires before the guard, so the loop doesn't waste a slot on a legitimate
// reasoning artefact.
type toolGuard struct {
	mu     sync.Mutex
	recent [toolGuardRingLen]string
	next   int
}

// Allow returns nil when signature is new; otherwise an error naming the
// tool the model just repeated. Signatures are opaque to Allow — the caller
// builds them with toolSignature so the encoding stays consistent.
func (g *toolGuard) Allow(signature string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.recent {
		if r != "" && r == signature {
			toolName, _, _ := strings.Cut(signature, "|")
			return fmt.Errorf("identical %s call repeated — read the current file and pick a different change", toolName)
		}
	}
	g.recent[g.next] = signature
	g.next = (g.next + 1) % len(g.recent)
	return nil
}

// toolSignature builds a stable key for the anti-loop guard. The tool name
// lives in the leading segment so Allow can surface it in the error;
// remaining parts get sha256'd to keep memory bounded regardless of payload
// size.
func toolSignature(toolName string, parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return toolName + "|" + hex.EncodeToString(h.Sum(nil))
}

// budgetHint composes the "X of Y HTML files used; ~N iterations remaining"
// string that goes back to the model in tool results. htmlCount < 0 omits
// the file-count clause (callers that don't already have a List in hand
// can pass -1 to avoid an extra round-trip).
func budgetHint(htmlCount int, state *buildState) string {
	remaining := maxAgentIterations - int(state.iters.Load())
	if remaining < 0 {
		remaining = 0
	}
	if htmlCount < 0 {
		return fmt.Sprintf("~%d iterations remaining", remaining)
	}
	return fmt.Sprintf("%d of %d HTML files used; ~%d iterations remaining", htmlCount, maxHTMLFiles, remaining)
}

// validateHTMLPath gates every tool that writes/edits HTML: reject anything
// that could escape the slug, smuggle non-HTML into HTML paths, or clobber
// files managed by other tools. Implemented in internal/textedit so the MCP
// write/edit tools enforce the identical rule.
func validateHTMLPath(p string) error {
	return textedit.ValidateHTMLPath(p)
}

// validateFunctionName accepts the bare handler name (no path, no extension)
// supplied to write_function/read_function. Delegates to internal/textedit.
func validateFunctionName(name string) error {
	return textedit.ValidateFunctionName(name)
}
