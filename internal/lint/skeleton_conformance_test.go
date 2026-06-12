package lint

import (
	"context"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/storetest"
	"github.com/jtarchie/topbanana/internal/templates"
)

// TestSkeletonsLintClean seeds every shipped template skeleton into a store
// and asserts lint passes with zero errors. The skeletons are the platform's
// own starting points — if a lint check would flag one, either the skeleton
// has a real bug or the check is too noisy to ship; both must be resolved
// before the check lands. Content types mirror build.seedTemplate.
func TestSkeletonsLintClean(t *testing.T) {
	t.Parallel()

	for _, tmpl := range templates.All() {
		if len(tmpl.Skeleton) == 0 {
			continue
		}
		t.Run(tmpl.ID, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			s := storetest.New(t, 0)
			slug := storetest.FreshSlug(t, "skel-"+tmpl.ID)

			for path, content := range tmpl.Skeleton {
				ct := "text/html; charset=utf-8"
				if strings.HasSuffix(path, ".js") {
					ct = "application/javascript; charset=utf-8"
				}
				err := s.Write(ctx, slug, path, content, ct, nil)
				if err != nil {
					t.Fatalf("seed %s: %v", path, err)
				}
			}

			for _, e := range App(ctx, s, slug, tmpl) {
				t.Errorf("lint: %s", e.Error())
			}
		})
	}
}
