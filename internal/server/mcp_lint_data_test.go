package server

import (
	"testing"

	"github.com/jtarchie/topbanana/internal/lint"
)

// TestMCPLintProblems pins the structured lint_site contract: each problem
// carries file/message/kind and an autofixable flag that is true only for the
// design-substrate kind (the one the build loop mechanically repairs), plus the
// flat strings older clients read.
func TestMCPLintProblems(t *testing.T) {
	t.Parallel()
	errs := []lint.Error{
		{File: "index.html", Message: "missing /app.css", Kind: lint.KindDesignSubstrate},
		{File: "about.html", Message: "broken link to x.html"}, // empty kind => agent must fix
	}
	problems, msgs := mcpLintProblems(errs)
	if len(problems) != 2 || len(msgs) != 2 {
		t.Fatalf("want 2 problems and 2 msgs, got %d/%d", len(problems), len(msgs))
	}
	if problems[0]["autofixable"] != true {
		t.Error("design-substrate problem should be autofixable")
	}
	if problems[1]["autofixable"] != false {
		t.Error("unclassified problem must not be autofixable")
	}
	if problems[0]["file"] != "index.html" || problems[0]["message"] != "missing /app.css" {
		t.Errorf("unexpected problem fields: %+v", problems[0])
	}
	if msgs[0] != "index.html: missing /app.css" {
		t.Errorf("flat message = %q", msgs[0])
	}

	empty, emptyMsgs := mcpLintProblems(nil)
	if len(empty) != 0 || len(emptyMsgs) != 0 {
		t.Error("nil errors should yield empty (non-nil) slices")
	}
}

func TestMCPSubmissionRows(t *testing.T) {
	t.Parallel()
	cols := []string{"name", "email"}
	rows := []dataRow{
		{Key: "submission:0002", Values: []string{"Bo", "bo@x.com"}},
		{Key: "submission:0001", Values: []string{"Al"}}, // short row: missing email
	}

	out, truncated := mcpSubmissionRows(cols, rows, 100)
	if truncated {
		t.Error("should not truncate under the cap")
	}
	if out[0]["_key"] != "submission:0002" || out[0]["name"] != "Bo" || out[0]["email"] != "bo@x.com" {
		t.Errorf("row 0 = %+v", out[0])
	}
	// A row shorter than cols must omit the missing column rather than panic.
	if _, ok := out[1]["email"]; ok {
		t.Errorf("short row must omit missing column, got %+v", out[1])
	}
	if out[1]["name"] != "Al" {
		t.Errorf("row 1 name = %v", out[1]["name"])
	}

	capped, truncated := mcpSubmissionRows(cols, rows, 1)
	if !truncated || len(capped) != 1 {
		t.Errorf("cap=1 should truncate to 1 row, got %d truncated=%v", len(capped), truncated)
	}
}
