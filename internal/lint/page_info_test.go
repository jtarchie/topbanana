package lint

import (
	"strings"
	"testing"
)

func TestCollectPageInfo(t *testing.T) {
	t.Parallel()

	pi := pageOf(t, "docs/page.html", `<!DOCTYPE html><html><head><title>x</title></head><body>
<section id="hero"></section>
<div id="hero"></div>
<a name="legacy">x</a>
<script></script>
<script>let a = 1;</script>
<script type="application/json">{"not":"js"}</script>
<script>this is not valid js {{{</script>
</body></html>`)

	if pi.dir != "docs" {
		t.Errorf("dir = %q, want %q", pi.dir, "docs")
	}
	if pi.ids["hero"] != 2 {
		t.Errorf(`ids["hero"] = %d, want 2 (duplicate counted)`, pi.ids["hero"])
	}
	if !pi.targets["hero"] || !pi.targets["legacy"] {
		t.Errorf("targets missing hero/legacy: %v", pi.targets)
	}
	if len(pi.elements) == 0 {
		t.Fatal("elements not collected")
	}

	// Scripts: the empty one is counted in ordinals but not kept; the JSON
	// one is not lintable; the broken one is kept with parseErr set. So we
	// keep #2 (valid) and #3 (broken) — the JSON script never increments.
	if len(pi.scripts) != 2 {
		t.Fatalf("scripts = %d, want 2: %+v", len(pi.scripts), pi.scripts)
	}
	if pi.scripts[0].ordinal != 2 || pi.scripts[0].program == nil || pi.scripts[0].parseErr != nil {
		t.Errorf("first kept script should be ordinal 2 and parsed: %+v", pi.scripts[0])
	}
	if pi.scripts[1].ordinal != 3 || pi.scripts[1].program != nil || pi.scripts[1].parseErr == nil {
		t.Errorf("second kept script should be ordinal 3 with a parse error: %+v", pi.scripts[1])
	}

	// checkInlineJS numbering must match the ordinals.
	errs := checkInlineJS("docs/page.html", pi.scripts)
	if len(errs) != 1 {
		t.Fatalf("checkInlineJS = %+v, want 1 error", errs)
	}
	if !strings.Contains(errs[0].Message, "inline <script> #3 parse error") {
		t.Errorf("parse error must carry the visible script ordinal: %s", errs[0].Message)
	}
}
