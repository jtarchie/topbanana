package lint

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// tinytoolsCSS is the relevant slice of the real stylesheet from the incident:
// a fixed size at the top level and a smaller one inside a breakpoint, both of
// which outranked the width/height attributes the agent kept editing.
const tinytoolsCSS = `
.tool{display:flex;gap:18px}
.tool .shot{
  flex:none;width:84px;height:84px;border-radius:9px;display:block;
  object-fit:cover;
}
@media(max-width:520px){
  .tool .shot{width:64px;height:64px;border-radius:7px}
}
`

func parsePage(t *testing.T, name, content string) pageInfo {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return collectPageInfo(name, doc)
}

func TestCascadeConflict_CatchesTheIncident(t *testing.T) {
	t.Parallel()

	rules := collectSizingRules("site.css", tinytoolsCSS)
	page := parsePage(t, "index.html", `<html><body>
<a class="tool"><img class="shot" src="/assets/getflexy.png" width="360" height="360" alt="x"></a>
</body></html>`)

	errs := checkCascadeConflicts([]pageInfo{page}, rules)
	if len(errs) == 0 {
		t.Fatal("the attribute-vs-stylesheet conflict that shipped twice was not reported")
	}
	got := errs[0].Message
	for _, want := range []string{"site.css", "84px", "360"} {
		if !strings.Contains(got, want) {
			t.Errorf("message missing %q: %s", want, got)
		}
	}
	if errs[0].Kind != KindCascadeConflict {
		t.Errorf("Kind = %q, want %q", errs[0].Kind, KindCascadeConflict)
	}
}

// The narrowness matters more than the coverage: these errors gate a build, so
// every case here must stay silent.
func TestCascadeConflict_StaysQuiet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		css  string
		html string
	}{{
		// The canonical responsive pairing: attributes supply the aspect
		// ratio, CSS makes the element fluid. Flagging this would be wrong.
		name: "fluid rule alongside sizing attributes",
		css:  `img{max-width:100%;height:auto}`,
		html: `<img src="a.png" width="600" height="400" alt="a">`,
	}, {
		name: "stylesheet merely restates the attribute",
		css:  `.shot{width:360px}`,
		html: `<img class="shot" src="a.png" width="360" alt="a">`,
	}, {
		name: "percentage width",
		css:  `.shot{width:50%}`,
		html: `<img class="shot" src="a.png" width="360" alt="a">`,
	}, {
		name: "min()/clamp() sizing",
		css:  `.shot{width:min(42%,220px)}`,
		html: `<img class="shot" src="a.png" width="360" alt="a">`,
	}, {
		name: "rule targets a different class",
		css:  `.hero{width:84px}`,
		html: `<img class="shot" src="a.png" width="360" alt="a">`,
	}, {
		name: "rule targets a different element type",
		css:  `video{width:84px}`,
		html: `<img class="shot" src="a.png" width="360" alt="a">`,
	}, {
		name: "state-dependent rule",
		css:  `.shot:hover{width:84px}`,
		html: `<img class="shot" src="a.png" width="360" alt="a">`,
	}, {
		name: "element carries no sizing attribute",
		css:  `.shot{width:84px}`,
		html: `<img class="shot" src="a.png" alt="a">`,
	}, {
		name: "commented-out rule",
		css:  `/* .shot{width:84px} */`,
		html: `<img class="shot" src="a.png" width="360" alt="a">`,
	}, {
		name: "keyframes percentages are not selectors",
		css:  `@keyframes grow{from{width:84px}to{width:200px}}`,
		html: `<img class="shot" src="a.png" width="360" alt="a">`,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rules := collectSizingRules("site.css", c.css)
			page := parsePage(t, "index.html", "<html><body>"+c.html+"</body></html>")
			if errs := checkCascadeConflicts([]pageInfo{page}, rules); len(errs) > 0 {
				t.Errorf("false positive: %s", errs[0].Message)
			}
		})
	}
}

func TestCollectSizingRules_ParsesBreakpointsAndSelectorLists(t *testing.T) {
	t.Parallel()

	rules := collectSizingRules("site.css", tinytoolsCSS)
	// .tool .shot at 84px (width + height) and again at 64px inside the
	// breakpoint: four pinned declarations in total.
	if len(rules) != 4 {
		t.Fatalf("got %d rules, want 4: %+v", len(rules), rules)
	}
	for _, r := range rules {
		if len(r.classes) != 1 || r.classes[0] != "shot" {
			t.Errorf("key compound should be .shot, got %+v", r.classes)
		}
	}

	list := collectSizingRules("site.css", `.a,.b{width:10px}`)
	if len(list) != 2 {
		t.Errorf("a selector list should yield one rule per selector, got %d", len(list))
	}
}
