// Package build orchestrates the per-slug build lifecycle: seed the
// template skeleton, run the agent, lint and retry on failures, and persist
// the per-site metadata sidecar.
package build

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/buildabear/internal/agent"
	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/lint"
	"github.com/jtarchie/buildabear/internal/store"
	"github.com/jtarchie/buildabear/internal/templates"
)

const (
	maxLintRetries = 3

	// MetaFile holds the per-site sidecar (template id, creation time, basic
	// auth). Stored alongside the HTML files in the same S3 prefix so it
	// travels with the site.
	MetaFile = ".buildabear.json"
)

// SiteMeta is the per-site sidecar persisted at MetaFile.
type SiteMeta struct {
	Template     string    `json:"template"`
	Created      time.Time `json:"created"`
	Username     string    `json:"username,omitempty"`
	PasswordHash string    `json:"password_hash,omitempty"`
}

// Service runs builds against a Store using a configured LLM, reporting
// progress through an events Tracker.
type Service struct {
	store  *store.Store
	llm    adkmodel.LLM
	events *events.Tracker
}

func New(s *store.Store, llm adkmodel.LLM, t *events.Tracker) *Service {
	return &Service{store: s, llm: llm, events: t}
}

// Params describes one invocation of Start. LogKey distinguishes build vs.
// edit in slog output. SeedSkeleton (initial builds only) writes the
// template's skeleton files and metadata sidecar before the agent runs.
type Params struct {
	Slug         string
	Prompt       string
	LogKey       string
	Template     *templates.SiteTemplate
	SeedSkeleton bool
	Seeds        []agent.SeedToolCall
	Username     string
	PasswordHash string
}

// Start records the build as in-flight and runs it asynchronously. The
// goroutine emits status events through the tracker; callers render the
// progress page and subscribe via the events handler.
func (svc *Service) Start(p Params) {
	svc.events.Start(p.Slug)

	go func() {
		ctx := context.Background()
		if p.SeedSkeleton {
			err := svc.seedTemplate(ctx, p.Slug, p.Template, p.Username, p.PasswordHash)
			if err != nil {
				slog.Error(p.LogKey+".seed_failed", "slug", p.Slug, "template", p.Template.ID, "err", err)
				svc.events.Fail(p.Slug, err)
				return
			}
		}
		err := svc.buildAndLint(ctx, p.Slug, p.Prompt, p.Template, p.Seeds)
		if err != nil {
			slog.Error(p.LogKey+".failed", "slug", p.Slug, "err", err)
			svc.events.Fail(p.Slug, err)
			return
		}
		slog.Info(p.LogKey+".done", "slug", p.Slug)
		svc.events.Complete(p.Slug)
	}()
}

// buildAndLint runs the agent then lints with up to maxLintRetries fix-up
// passes when issues are found.
func (svc *Service) buildAndLint(ctx context.Context, slug, prompt string, tmpl *templates.SiteTemplate, seeds []agent.SeedToolCall) error {
	emit := func(e events.Event) { svc.events.Emit(slug, e) }

	err := agent.Run(ctx, svc.llm, svc.store, slug, prompt, tmpl, seeds, emit)
	if err != nil {
		return fmt.Errorf("agent run: %w", err)
	}

	for attempt := 0; attempt <= maxLintRetries; attempt++ {
		emit(events.Event{Type: events.TypeStatus, Status: events.StatusLinting})
		lintErrs := lint.App(ctx, svc.store, slug, tmpl)
		if len(lintErrs) == 0 {
			return nil
		}

		msgs := make([]string, 0, len(lintErrs))
		for _, e := range lintErrs {
			msgs = append(msgs, e.Error())
		}

		if attempt == maxLintRetries {
			return fmt.Errorf("lint errors after %d retries: %s", maxLintRetries, strings.Join(msgs, "; "))
		}

		slog.Info("build.lint_retry", "slug", slug, "attempt", attempt+1, "issues", len(lintErrs))
		emit(events.Event{Type: events.TypeStatus, Status: events.StatusRetry, Message: fmt.Sprintf("fixing %d issue(s)", len(lintErrs))})
		fixPrompt := "Fix these issues in the site:\n" + strings.Join(msgs, "\n")
		err := agent.Run(ctx, svc.llm, svc.store, slug, fixPrompt, tmpl, nil, emit)
		if err != nil {
			return fmt.Errorf("agent retry: %w", err)
		}
	}

	return nil
}

// seedTemplate writes the template's skeleton files (if any) and the
// .buildabear.json sidecar recording the template id. The sidecar lets later
// edits re-apply the same template addendum.
func (svc *Service) seedTemplate(ctx context.Context, slug string, tmpl *templates.SiteTemplate, username, passwordHash string) error {
	if tmpl == nil {
		return nil
	}
	for path, content := range tmpl.Skeleton {
		err := svc.store.Write(ctx, slug, path, content, "text/html; charset=utf-8", nil)
		if err != nil {
			return fmt.Errorf("seed %s: %w", path, err)
		}
		slog.Info("template.seed", "slug", slug, "template", tmpl.ID, "path", path)
	}

	return svc.WriteMeta(ctx, slug, SiteMeta{
		Template:     tmpl.ID,
		Created:      time.Now().UTC(),
		Username:     username,
		PasswordHash: passwordHash,
	})
}

// WriteMeta persists the per-site sidecar.
func (svc *Service) WriteMeta(ctx context.Context, slug string, meta SiteMeta) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode site meta: %w", err)
	}
	err = svc.store.Write(ctx, slug, MetaFile, string(body), "application/json", nil)
	if err != nil {
		return fmt.Errorf("write site meta: %w", err)
	}
	return nil
}

// ReadMeta returns the recorded sidecar for an existing site, or a zero value
// if the sidecar is missing (older sites pre-date templates).
func (svc *Service) ReadMeta(ctx context.Context, slug string) SiteMeta {
	obj, err := svc.store.Read(ctx, slug, MetaFile)
	if err != nil {
		slog.Warn("site_meta.read_failed", "slug", slug, "err", err)
		return SiteMeta{}
	}
	if obj.Content == "" {
		return SiteMeta{}
	}
	var m SiteMeta
	err = json.Unmarshal([]byte(obj.Content), &m)
	if err != nil {
		slog.Warn("site_meta.decode_failed", "slug", slug, "err", err)
		return SiteMeta{}
	}
	return m
}
