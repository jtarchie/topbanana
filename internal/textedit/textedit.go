// Package textedit holds the pure string transforms and path/name validation
// shared by the in-process build agent (internal/agent) and the external MCP
// editing surface (internal/server). Everything here operates on plain strings
// with no store, locking, or LLM dependency, so the exact same editing
// semantics — byte-exact find/replace with a whitespace-tolerant fallback,
// line splicing, cat -n numbering, and HTML/function path validation — back
// both the server-side agent's tools and the tools an external agent (Claude
// Code) drives over MCP. Different callers, identical behaviour.
package textedit

import (
	"errors"
	"fmt"
	"path"
	"strings"
)

// maxHTMLPathLen bounds an authored HTML path. Mirrors the agent's own cap so
// the two surfaces reject the same oversized paths.
const maxHTMLPathLen = 200

// ApplyEdit performs the find/replace at the heart of edit_file and
// edit_function. Returns the updated content, the replacement count, a note
// surfaced to the caller (empty when there's nothing to flag), and an error.
// Returning errors as values (not Go errors) lets the caller surface them in
// the tool's Error field so the agent can recover.
//
// When exact-string matching fails, ApplyEdit attempts a whitespace-tolerant
// search: if the file contains exactly one byte range whose whitespace-
// collapsed form equals the whitespace-collapsed old_text, that range is
// replaced and a note advises the model to copy whitespace verbatim next
// time. Zero or multiple tolerant matches still fall through to the original
// diagnostic so the model has actionable feedback.
func ApplyEdit(content, oldText, newText string, replaceAll bool) (string, int, string, error) {
	count := strings.Count(content, oldText)
	if count == 0 {
		updated, ok := applyTolerantEdit(content, oldText, newText)
		if ok {
			return updated, 1, "applied a whitespace-tolerant match — the file's whitespace at the match site differed from old_text. Re-read the file (use read_file with start_line/end_line) to copy whitespace verbatim for predictable edits next time.", nil
		}
		return "", 0, "", DiagnoseNotFound(content, oldText)
	}
	if count > 1 && !replaceAll {
		return "", 0, "", fmt.Errorf("old_text matches %d locations; include more surrounding context to make it unique, or set replace_all=true", count)
	}
	if replaceAll {
		return strings.ReplaceAll(content, oldText, newText), count, "", nil
	}
	return strings.Replace(content, oldText, newText, 1), 1, "", nil
}

// applyTolerantEdit looks for exactly one substring of content whose
// whitespace-collapsed form equals CollapseWS(oldText). When that uniquely
// identifies a region, it's safe to replace — the alternative is forcing the
// agent to re-read and retry. When zero or >1 candidates exist, returns
// ok=false so the caller falls through to the existing error path.
func applyTolerantEdit(content, oldText, newText string) (string, bool) {
	target := CollapseWS(oldText)
	if target == "" {
		return "", false
	}
	type span struct{ start, end int }
	var found []span
	for i := 0; i <= len(content); i++ {
		// Find the smallest j > i such that CollapseWS(content[i:j]) == target.
		// Once equal we record it and resume the outer loop past the match;
		// once it exceeds target length we abandon this start.
		for j := i; j <= len(content); j++ {
			collapsed := CollapseWS(content[i:j])
			if collapsed == target {
				found = append(found, span{i, j})
				if len(found) > 1 {
					return "", false // ambiguous — bail
				}
				i = j - 1 // -1 because outer loop's i++ will bump it
				break
			}
			if len(collapsed) > len(target) {
				break
			}
		}
	}
	if len(found) != 1 {
		return "", false
	}
	m := found[0]
	return content[:m.start] + newText + content[m.end:], true
}

// CollapseWS collapses every run of ASCII whitespace to a single space.
func CollapseWS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inWS := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			if !inWS {
				b.WriteByte(' ')
				inWS = true
			}
		default:
			b.WriteRune(r)
			inWS = false
		}
	}
	return b.String()
}

// ContainsCollapsedWS reports whether needle appears in haystack after every
// run of whitespace is collapsed to a single space in both. Used only for
// diagnostics — the actual replace still requires a byte-exact match.
func ContainsCollapsedWS(haystack, needle string) bool {
	return strings.Contains(CollapseWS(haystack), CollapseWS(needle))
}

// DiagnoseNotFound returns the most actionable error message for a failed
// edit lookup. The common failure mode is that the model copied old_text with
// slightly-wrong whitespace (tabs vs spaces, missing indent, extra trailing
// newline), so we check for those first and tell the model exactly what to
// fix. Falling back to a generic message would just trigger another blind
// retry.
func DiagnoseNotFound(content, oldText string) error {
	trimmed := strings.TrimSpace(oldText)
	if trimmed != "" && trimmed != oldText && strings.Contains(content, trimmed) {
		return errors.New("old_text has extra leading or trailing whitespace that the file does not contain; trim it and retry")
	}
	if ContainsCollapsedWS(content, oldText) {
		return errors.New("old_text matches only when whitespace is normalized — the file uses different indentation, tabs, or line breaks than your old_text. Re-read the file (use start_line/end_line to zoom in) and copy the whitespace verbatim")
	}
	return errors.New("old_text not found in file. Re-read the file to confirm the exact text (whitespace included), or use grep_files to locate a unique substring before retrying")
}

// SpliceLines replaces lines start..end (1-indexed, inclusive) of content with
// newText. Validates the range; returns a descriptive error if the range is
// out-of-bounds or inverted so the agent can correct itself. Empty newText
// deletes the range.
func SpliceLines(content string, start, end int, newText string) (string, error) {
	if start < 1 {
		return "", fmt.Errorf("start_line must be >= 1 (got %d)", start)
	}
	if end < start {
		return "", fmt.Errorf("end_line (%d) must be >= start_line (%d)", end, start)
	}
	lines := strings.Split(content, "\n")
	if end > len(lines) {
		return "", fmt.Errorf("end_line %d exceeds file length %d", end, len(lines))
	}
	var head, mid, tail []string
	head = lines[:start-1]
	tail = lines[end:]
	if newText != "" {
		mid = strings.Split(newText, "\n")
	}
	out := make([]string, 0, len(head)+len(mid)+len(tail))
	out = append(out, head...)
	out = append(out, mid...)
	out = append(out, tail...)
	return strings.Join(out, "\n"), nil
}

// InsertAfterLine returns content with insertContent spliced in after line
// `after` (1-indexed). after=0 prepends; after=total_lines appends.
func InsertAfterLine(content string, after int, insertContent string) (string, error) {
	lines := strings.Split(content, "\n")
	if after < 0 {
		return "", fmt.Errorf("after_line must be >= 0 (got %d)", after)
	}
	if after > len(lines) {
		return "", fmt.Errorf("after_line %d exceeds file length %d", after, len(lines))
	}
	insertLines := strings.Split(insertContent, "\n")
	out := make([]string, 0, len(lines)+len(insertLines))
	out = append(out, lines[:after]...)
	out = append(out, insertLines...)
	out = append(out, lines[after:]...)
	return strings.Join(out, "\n"), nil
}

// SliceLines returns a 1-indexed-inclusive slice of content delimited by \n,
// plus the total line count of the full content. start/end of 0 mean "from
// line 1" and "through last line" respectively, so the zero-value (both 0)
// returns the whole content unchanged. start past the end returns an empty
// slice with no error (lets the agent self-correct using total_lines).
func SliceLines(content string, start, end int) (string, int, error) {
	var total int
	if content == "" {
		total = 0
	} else {
		total = strings.Count(content, "\n") + 1
	}
	if start <= 0 && end <= 0 {
		return content, total, nil
	}
	if start > 0 && end > 0 && start > end {
		return "", total, errors.New("start_line must be <= end_line")
	}
	lines := strings.Split(content, "\n")
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return "", total, nil
	}
	return strings.Join(lines[start-1:end], "\n"), total, nil
}

// NumberLines prefixes every line in content with its 1-indexed line number
// (right-aligned to 6 columns, followed by a tab), matching the cat -n
// convention LLMs recognize from training. startOffset is the number to
// assign to the first line — pass 1 for a full-file read or the requested
// start_line for a slice, so numbers always refer to positions in the
// original file rather than the slice's local index. The empty string
// returns the empty string.
func NumberLines(content string, startOffset int) string {
	if content == "" {
		return ""
	}
	if startOffset < 1 {
		startOffset = 1
	}
	lines := strings.Split(content, "\n")
	var out strings.Builder
	out.Grow(len(content) + len(lines)*8)
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "%6d\t%s", startOffset+i, line)
	}
	return out.String()
}

// reservedWritePrefixes are paths managed by other tools (functions/, assets/)
// that the HTML write tools must not clobber.
var reservedWritePrefixes = []string{"functions/", "assets/"}

// reservedWritePaths are exact paths the HTML write tools must not touch
// (e.g. the per-site sidecar persisted by the build service). Both the current
// and legacy sidecar names are reserved so an agent on a legacy site can't
// accidentally clobber pre-rebrand metadata.
var reservedWritePaths = map[string]bool{
	".topbanana.json":   true,
	".bloomhollow.json": true,
	".buildabear.json":  true,
}

// ValidateHTMLPath gates every tool that writes/edits HTML. Rejects anything
// that could escape the slug, smuggle non-HTML into HTML paths, or clobber
// files managed by other tools.
func ValidateHTMLPath(p string) error {
	for _, check := range htmlPathChecks {
		err := check(p)
		if err != nil {
			return err
		}
	}
	return nil
}

var htmlPathChecks = []func(string) error{
	checkHTMLPathShape,
	checkHTMLPathCharset,
	checkHTMLPathSegments,
	checkHTMLPathExtension,
	checkHTMLPathReserved,
}

func checkHTMLPathShape(p string) error {
	switch {
	case p == "":
		return errors.New("path is required")
	case len(p) > maxHTMLPathLen:
		return fmt.Errorf("path too long (max %d chars)", maxHTMLPathLen)
	case strings.HasPrefix(p, "/"):
		return errors.New("path must be relative (no leading /)")
	case strings.Contains(p, `\`):
		return errors.New("path must use forward slashes")
	}
	return nil
}

func checkHTMLPathCharset(p string) error {
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '/' || r == '.':
		default:
			return fmt.Errorf("path must match [a-z0-9_/.-] (got %q)", p)
		}
	}
	return nil
}

func checkHTMLPathSegments(p string) error {
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q contains an empty or relative segment", p)
		}
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q is not canonical", p)
	}
	return nil
}

func checkHTMLPathExtension(p string) error {
	if !strings.HasSuffix(p, ".html") {
		return fmt.Errorf("path %q must end with .html", p)
	}
	return nil
}

func checkHTMLPathReserved(p string) error {
	if reservedWritePaths[p] {
		return fmt.Errorf("path %q is reserved", p)
	}
	for _, pfx := range reservedWritePrefixes {
		if strings.HasPrefix(p, pfx) {
			return fmt.Errorf("path %q is under reserved prefix %q", p, pfx)
		}
	}
	return nil
}

// ValidateFunctionName accepts the bare handler name (no path, no extension)
// supplied to write_function/read_function. Rejects anything that could escape
// the slug's functions/ prefix or smuggle JS into a non-function path. Names
// match [a-z0-9-_]{1,40}.
func ValidateFunctionName(name string) error {
	if name == "" {
		return errors.New("function name is required")
	}
	if len(name) > 40 {
		return errors.New("function name too long")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return errors.New("function name must match [a-z0-9-_]")
		}
	}
	return nil
}

// GrepEligible decides whether a stored path is worth grepping. Assets are
// binary-ish, the sidecar is internal metadata, and other extensions don't
// belong in a search aimed at HTML + function source.
func GrepEligible(path string) bool {
	if strings.HasPrefix(path, "assets/") {
		return false
	}
	if path == ".topbanana.json" || path == ".bloomhollow.json" || path == ".buildabear.json" {
		return false
	}
	return strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".js")
}

// GrepMatch is one literal-substring hit: the file, its 1-indexed line, and a
// length-capped snippet of that line.
type GrepMatch struct {
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	Snippet    string `json:"snippet"`
}

// MatchLines returns every line in content that literally contains pattern, as
// 1-indexed GrepMatch entries with snippets capped to snippetMax bytes. Path
// labels the matches. An empty pattern returns no matches.
func MatchLines(p, content, pattern string, snippetMax int) []GrepMatch {
	if pattern == "" || !strings.Contains(content, pattern) {
		return nil
	}
	var out []GrepMatch
	for i, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, pattern) {
			continue
		}
		out = append(out, GrepMatch{Path: p, LineNumber: i + 1, Snippet: TruncateSnippet(line, snippetMax)})
	}
	return out
}

// TruncateSnippet caps line to limit bytes, appending an ellipsis when it had
// to cut. The cut is on a byte boundary, matching the agent's existing
// snippets.
func TruncateSnippet(line string, limit int) string {
	if len(line) <= limit {
		return line
	}
	return line[:limit] + "…"
}
