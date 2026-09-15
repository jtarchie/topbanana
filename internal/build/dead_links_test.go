package build

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/linkcheck"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
)

type fakeLinkChecker struct {
	rep linkcheck.Report
	err error
}

func (f fakeLinkChecker) Check(context.Context, string) (linkcheck.Report, error) {
	return f.rep, f.err
}

type promptRecorder struct {
	mu      sync.Mutex
	prompts []string
}

func (r *promptRecorder) Run(_ context.Context, _ *store.Store, req RunRequest, _ func(events.Event), _ *events.Tracker) (agent.Usage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prompts = append(r.prompts, req.Prompt)
	return agent.Usage{}, nil
}

func (r *promptRecorder) Describe(context.Context, *store.Store, string, string) (agent.SiteDescription, error) {
	return agent.SiteDescription{}, nil
}

func TestFixDeadLinks(t *testing.T) {
	t.Parallel()

	rep := linkcheck.Report{Results: []linkcheck.Result{
		{URL: "https://invented.example/menu", Status: linkcheck.StatusDead, Reason: "the domain does not exist", Pages: []string{"index.html"}},
		{URL: "https://slow.example/", Status: linkcheck.StatusSuspect, Reason: "the site did not answer in time", Pages: []string{"index.html"}},
		{URL: "https://linkedin.example/in/x", Status: linkcheck.StatusUnverified, Pages: []string{"about.html"}},
	}}

	cases := []struct {
		name    string
		checker LinkChecker
		want    int
	}{
		{"no checker configured", nil, 0},
		{"check failed", fakeLinkChecker{err: errors.New("store down")}, 0},
		{"nothing certainly dead", fakeLinkChecker{rep: linkcheck.Report{Results: rep.Results[1:]}}, 0},
		{"dead link gets one turn", fakeLinkChecker{rep: rep}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tracker := events.NewTracker()
			t.Cleanup(tracker.Close)
			svc := NewWithConfig(Config{Store: storetest.New(t, 0), Events: tracker, LinkChecker: tc.checker})
			runner := &promptRecorder{}

			svc.fixDeadLinks(context.Background(), runner, Params{Slug: "s", LogKey: "build"}, nil)

			if len(runner.prompts) != tc.want {
				t.Fatalf("editor turns = %d, want %d", len(runner.prompts), tc.want)
			}
			if tc.want == 0 {
				return
			}
			p := runner.prompts[0]
			if !strings.Contains(p, "https://invented.example/menu: the domain does not exist. Used on index.html") {
				t.Errorf("prompt must name the dead URL, why, and where:\n%s", p)
			}
			if strings.Contains(p, "slow.example") || strings.Contains(p, "linkedin.example") {
				t.Errorf("only certainly-dead links may reach the agent:\n%s", p)
			}
		})
	}
}
