package build

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/linkcheck"
)

// LinkChecker is the seam to internal/linkcheck, so builds in tests never touch the network.
type LinkChecker interface {
	Check(ctx context.Context, slug string) (linkcheck.Report, error)
}

// fixDeadLinks spends one editor turn on links that are certainly dead, which is what an invented URL looks like; advisory, so it never fails the build.
func (svc *Service) fixDeadLinks(ctx context.Context, editor Runner, p Params, rec *editrec.Recorder) {
	if svc.linkChecker == nil {
		return
	}
	rep, err := svc.linkChecker.Check(ctx, p.Slug)
	if err != nil {
		slog.Warn(p.LogKey+".linkcheck_failed", "slug", p.Slug, "err", err)
		return
	}
	dead := rep.Dead()
	if len(dead) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, svc.buildTimeout)
	defer cancel()
	emit := func(e events.Event) { svc.events.Emit(p.Slug, e) }
	if rec != nil {
		emit = rec.Wrap(ctx, svc.store, p.Slug, emit)
	}
	emit(events.Event{Type: events.TypeStatus, Status: events.StatusRetry, Message: fmt.Sprintf("fixing %d dead link(s)", len(dead))})

	prompt := DeadLinkPrompt(dead)
	usage, err := editor.Run(ctx, svc.store, RunRequest{
		Slug:        p.Slug,
		Prompt:      prompt,
		Template:    p.Template,
		Attachments: p.Attachments,
		Seeds:       svc.EditSeeds(ctx, p.Slug, prompt),
		BuildStart:  time.Now(),
		IsEdit:      !p.SeedSkeleton,
	}, emit, svc.events)
	recordUsage(rec, usage)
	if err != nil {
		slog.Warn(p.LogKey+".dead_link_fix_failed", "slug", p.Slug, "err", err)
	}
}

// DeadLinkPrompt forbids guessing a replacement, since a second invented URL is the very failure being repaired.
func DeadLinkPrompt(dead []linkcheck.Result) string {
	var b strings.Builder
	b.WriteString("You are fixing dead links on an existing site. Use read_file to see each affected file's current content first, edit it in place, and do not rewrite pages from scratch or delete content unrelated to the links listed below.")
	b.WriteString("\n\nThese links to other websites are dead (checked from the server just now):\n")
	for _, r := range dead {
		fmt.Fprintf(&b, "- %s: %s. Used on %s\n", r.URL, r.Reason, strings.Join(r.Pages, ", "))
	}
	b.WriteString("\nFor each one: if you are certain of the correct address (a page on this site, or an official URL you know exists), point the href/src there. Otherwise do not guess another address: remove the link but keep its visible text, and remove an <img>, <iframe>, or media element whose source is dead. Change nothing else.")
	return b.String()
}
