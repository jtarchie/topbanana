package build

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jtarchie/topbanana/internal/editrec"
)

// This file assembles the short "what happened here before" block that rides
// along with a user-initiated edit. It exists because of a specific failure:
// a user asked to make images bigger, the agent changed the width attributes
// on <img> and reported success, nothing rendered differently, and two minutes
// later the user asked again in near-identical words. The agent — which had no
// idea it had just been asked the same thing — made the identical change with
// bigger numbers.
//
// The restatement was the highest-signal fact available about that request,
// and it was the one fact the agent could not see.

const (
	// historyRuns is how many prior runs to summarise. Three covers a short
	// back-and-forth without turning the user turn into a changelog.
	historyRuns = 3

	// historyMaxPromptChars truncates a long prior prompt. The opening of a
	// request carries the intent; the rest is elaboration.
	historyMaxPromptChars = 240

	// historyMaxFiles caps the files listed per run, so one sweeping edit
	// can't crowd out the runs around it.
	historyMaxFiles = 6
)

// PriorRun is the distilled record of one earlier user-initiated run: enough
// to recognise a repeat, not enough to re-litigate it. Deliberately omits the
// tool calls, the diffs, and the system prompt — /debug is where those belong.
type PriorRun struct {
	At     time.Time
	Prompt string
	Files  []string
	Status string
}

// userInitiatedLogKeys are the runs a person asked for. Lint-fix retries and
// the polish pass are machinery, not requests: surfacing them as history would
// read to the agent like the user asked for something they never asked for.
var userInitiatedLogKeys = map[string]bool{"build": true, "edit": true}

// RecentRuns returns up to limit prior user-initiated runs for a slug, newest
// first. Best-effort in the same spirit as EditSeeds: any read failure yields
// less history, never a failed edit.
//
// ponytail: reads whole transcripts — each carries the rendered system prompt
// and the before/after content of every file it touched — to project four
// fields out of each. Server-side I/O, not tokens, and the store compresses at
// rest, so it stays cheap until a site accumulates very large pages. The
// upgrade is a slim projection written alongside the transcript in
// editrec.Finish, read from here instead.
func (svc *Service) RecentRuns(ctx context.Context, slug string, limit int) []PriorRun {
	rows, err := editrec.List(ctx, svc.store, slug)
	if err != nil {
		slog.Warn("history.list_failed", "slug", slug, "err", err)
		return nil
	}

	out := make([]PriorRun, 0, limit)
	for _, row := range rows {
		if len(out) >= limit {
			break
		}
		if !userInitiatedLogKeys[row.LogKey] {
			continue
		}
		tr, err := editrec.Read(ctx, svc.store, row.Key)
		if err != nil {
			slog.Warn("history.read_failed", "slug", slug, "key", row.Key, "err", err)
			continue
		}
		if strings.TrimSpace(tr.UserPrompt) == "" {
			continue
		}
		out = append(out, PriorRun{
			At:     tr.StartedAt,
			Prompt: tr.UserPrompt,
			Files:  changedFiles(tr),
			Status: tr.FinalStatus,
		})
	}
	return out
}

// changedFiles returns the distinct paths a run actually modified, in stable
// order. Reads FileChanges rather than ToolCalls so a tool that errored, or
// one whose edit was a no-op, doesn't show up as a change that never happened.
func changedFiles(tr editrec.Transcript) []string {
	seen := make(map[string]bool, len(tr.FileChanges))
	paths := make([]string, 0, len(tr.FileChanges))
	for _, fc := range tr.FileChanges {
		if fc.Path == "" || seen[fc.Path] || fc.BeforeSHA256 == fc.AfterSHA256 {
			continue
		}
		seen[fc.Path] = true
		paths = append(paths, fc.Path)
	}
	sort.Strings(paths)
	if len(paths) > historyMaxFiles {
		paths = paths[:historyMaxFiles]
	}
	return paths
}

// FormatRunHistory renders the history block appended to a user-initiated edit
// prompt. Returns "" when there is nothing worth saying.
//
// This goes in the user turn, never the system instruction. History changes on
// every single run, so in the cached prefix it would invalidate the whole
// instruction on every edit — the exact cost the cache-stable layering in
// internal/agent/instruction.go exists to avoid.
func FormatRunHistory(runs []PriorRun, prompt string, now time.Time) string {
	if len(runs) == 0 {
		return ""
	}

	lines := []string{"Earlier requests on this site, most recent first:"}
	for _, run := range runs {
		lines = append(lines, "- "+formatRun(run, now))
	}
	lines = append(lines,
		"",
		"If the request below restates one of those, the earlier attempt did not have the effect the user wanted — otherwise they would not be asking again. Work out why it failed before you edit anything, and fix that cause. Re-applying the same change with different values will fail the same way.")

	if repeat := repeatedRun(runs, prompt, now); repeat != nil {
		lines = append(lines, "", strings.TrimSpace(fmt.Sprintf(
			"This request closely restates the one from %s. That run changed %s and evidently did not satisfy the user. Read the files that decide how the page actually renders — including any stylesheet the site carries, which outranks markup — and change the thing that is really controlling the result.",
			humanizeAge(now.Sub(repeat.At)), formatFiles(repeat.Files))))
	}

	return strings.Join(lines, "\n")
}

func formatRun(run PriorRun, now time.Time) string {
	parts := []string{humanizeAge(now.Sub(run.At)), fmt.Sprintf("%q", truncatePrompt(run.Prompt))}
	if len(run.Files) > 0 {
		parts = append(parts, "changed "+formatFiles(run.Files))
	} else {
		parts = append(parts, "changed nothing")
	}
	if run.Status != "" && run.Status != "completed" {
		parts = append(parts, run.Status)
	}
	return strings.Join(parts, " — ")
}

func formatFiles(files []string) string {
	if len(files) == 0 {
		return "no files"
	}
	return strings.Join(files, ", ")
}

func truncatePrompt(p string) string {
	p = strings.Join(strings.Fields(p), " ")
	if len(p) <= historyMaxPromptChars {
		return p
	}
	return p[:historyMaxPromptChars] + "…"
}

// humanizeAge renders a duration the way a person would say it. Exact
// timestamps would be worse here: "3 minutes ago" is what makes a restatement
// read as a restatement.
func humanizeAge(d time.Duration) string {
	switch {
	case d < 90*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 36*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

const (
	// repeatWindow bounds how recent a prior run must be for a restatement to
	// mean "that didn't work". Ask the same thing next week and you probably
	// mean it afresh; ask it again in minutes and the first attempt missed.
	repeatWindow = 30 * time.Minute

	// repeatSimilarity is the Jaccard overlap two prompts need before this is
	// called a restatement. Tuned against the incident pair, which scores just
	// over 0.5 — the shared skeleton ("they should be 1/3 to 1/2 the width of
	// the …") is what makes a restatement recognisable, so stopwords are
	// deliberately kept rather than stripped.
	repeatSimilarity = 0.5

	// repeatMinTokens keeps short prompts out of it. "make the hero bigger"
	// and "make the images bigger" are two unrelated requests that share three
	// of five words; only a prompt with real substance can be judged this way.
	repeatMinTokens = 8
)

// repeatedRun returns the most recent prior run whose prompt this one
// restates, or nil. Deliberately narrow: a false positive costs the agent some
// wasted reading, but firing on every superficially-similar pair would teach
// it to distrust its own prior work.
func repeatedRun(runs []PriorRun, prompt string, now time.Time) *PriorRun {
	current := promptTokens(prompt)
	if len(current) < repeatMinTokens {
		return nil
	}
	for i := range runs {
		run := runs[i]
		if now.Sub(run.At) > repeatWindow {
			// runs are newest-first, so everything after this is older too.
			return nil
		}
		prior := promptTokens(run.Prompt)
		if len(prior) < repeatMinTokens {
			continue
		}
		if jaccard(current, prior) >= repeatSimilarity {
			return &run
		}
	}
	return nil
}

// promptTokens lowercases and splits a prompt into a set of alphanumeric
// tokens. Punctuation is dropped, so "1/3" becomes "1" and "3" — which is
// what makes two phrasings of the same fraction look alike.
func promptTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, field := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		out[field] = true
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for tok := range a {
		if b[tok] {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}
