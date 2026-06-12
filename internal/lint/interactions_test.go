package lint

import (
	"strings"
	"testing"
)

func interactionErrs(t *testing.T, body string) []Error {
	t.Helper()
	pi := pageOf(t, "index.html", `<!DOCTYPE html><html><body>`+body+`</body></html>`)
	facts := collectJSFacts("index.html", pi.scripts)
	return checkDeadInteractions(pi, facts)
}

func kindsOf(errs []Error) map[Kind]int {
	out := map[Kind]int{}
	for _, e := range errs {
		out[e.Kind]++
	}
	return out
}

func TestCheckLabels(t *testing.T) {
	t.Parallel()

	t.Run("matched label passes", func(t *testing.T) {
		t.Parallel()
		errs := interactionErrs(t, `<label for="email">Email</label><input id="email" name="email">`)
		if kindsOf(errs)[KindOrphanLabel] != 0 {
			t.Fatalf("matched label flagged: %+v", errs)
		}
	})

	t.Run("orphan label flags with id inventory", func(t *testing.T) {
		t.Parallel()
		errs := interactionErrs(t, `<label for="emial">Email</label><input id="email" name="email">`)
		var found *Error
		for i := range errs {
			if errs[i].Kind == KindOrphanLabel {
				found = &errs[i]
			}
		}
		if found == nil {
			t.Fatalf("orphan label not flagged: %+v", errs)
		}
		for _, want := range []string{`for="emial"`, "Existing ids: email."} {
			if !strings.Contains(found.Message, want) {
				t.Errorf("message missing %q:\n%s", want, found.Message)
			}
		}
	})
}

func TestCheckDuplicateIDs(t *testing.T) {
	t.Parallel()

	errs := interactionErrs(t, `<div id="hero"></div><section id="hero"></section><p id="solo"></p>`)
	dups := kindsOf(errs)[KindDuplicateID]
	if dups != 1 {
		t.Fatalf("expected 1 duplicate-id error, got %d: %+v", dups, errs)
	}
	for _, e := range errs {
		if e.Kind == KindDuplicateID && !strings.Contains(e.Message, "2 elements") {
			t.Errorf("message must carry the occurrence count: %s", e.Message)
		}
	}
}

func TestCheckContactHrefs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		href    string
		wantErr bool
	}{
		{"valid mailto passes", "mailto:owner@example.com", false},
		{"mailto with subject query passes", "mailto:replace@example.com?subject=RSVP", false},
		{"mailto with plus alias passes", "mailto:owner+site@example.com", false},
		{"uppercase MAILTO passes validation path", "MAILTO:owner@example.com", false},
		{"mailto multiple recipients passes", "mailto:a@example.com,b@example.com", false},
		{"mailto without @ flags", "mailto:ownerexample.com", true},
		{"mailto without tld flags", "mailto:owner@example", true},
		{"empty mailto flags", "mailto:", true},
		{"mailto with space flags", "mailto:owner @example.com", true},
		{"valid tel passes", "tel:+15551234567", false},
		{"formatted tel passes", "tel:+1 (555) 123-4567", false},
		{"dotted tel passes", "tel:555.123.4567", false},
		{"short tel flags", "tel:123456", true},
		{"lettered tel flags", "tel:CALL-NOW", true},
		{"empty tel flags", "tel:", true},
		{"plain links ignored", "about.html", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := interactionErrs(t, `<a href="`+tc.href+`">x</a>`)
			got := kindsOf(errs)[KindBrokenContactHref]
			if tc.wantErr != (got == 1) {
				t.Fatalf("href %q: wantErr=%v, got %+v", tc.href, tc.wantErr, errs)
			}
		})
	}
}

func TestCheckHandlers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "defined function passes",
			body:    `<button onclick="doThing()">x</button><script>function doThing() {}</script>`,
			wantErr: false,
		},
		{
			name:    "undefined bare call flags",
			body:    `<button onclick="ghost()">x</button><script>function doThing() {}</script>`,
			wantErr: true,
		},
		{
			name:    "undefined with zero scripts flags",
			body:    `<button onclick="ghost()">x</button>`,
			wantErr: true,
		},
		{
			name:    "return-call shape flags",
			body:    `<form onsubmit="return ghost(event)"></form>`,
			wantErr: true,
		},
		{
			name:    "call then return-false shape flags",
			body:    `<form onsubmit="ghost(event); return false"></form>`,
			wantErr: true,
		},
		{
			name:    "const arrow definition passes",
			body:    `<button onclick="go()">x</button><script>const go = () => {};</script>`,
			wantErr: false,
		},
		{
			name:    "window assignment passes",
			body:    `<button onclick="go()">x</button><script>(function(){ window.go = function(){}; })();</script>`,
			wantErr: false,
		},
		{
			name:    "browser global passes",
			body:    `<button onclick="alert('hi')">x</button>`,
			wantErr: false,
		},
		{
			name: "member-expression handler skipped (waitlist verbatim)",
			body: `<form id="waitlist-form" onsubmit="document.getElementById('thanks').style.display='block'; this.style.display='none'; return false;"><input type="email" name="email"></form>
<p id="thanks" style="display:none;">Thanks</p>`,
			wantErr: false,
		},
		{
			name:    "multi-statement handler skipped",
			body:    `<button onclick="let x = 1; ghost(x)">x</button>`,
			wantErr: false,
		},
		{
			name:    "unparseable handler skipped",
			body:    `<button onclick="this is not js {{{">x</button>`,
			wantErr: false,
		},
		{
			name:    "page with parse-failed script stands down",
			body:    `<button onclick="ghost()">x</button><script>broken {{{</script>`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := interactionErrs(t, tc.body)
			got := kindsOf(errs)[KindUndefinedHandler]
			if tc.wantErr != (got == 1) {
				t.Fatalf("wantErr=%v, got %+v", tc.wantErr, errs)
			}
		})
	}
}

func TestCheckDOMQueries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name:    "existing id passes",
			body:    `<div id="wall"></div><script>document.getElementById('wall').textContent = 'x';</script>`,
			wantErr: false,
		},
		{
			name:    "missing id flags",
			body:    `<div id="wall"></div><script>document.getElementById('wal').textContent = 'x';</script>`,
			wantErr: true,
		},
		{
			name:    "querySelector id selector flags",
			body:    `<script>document.querySelector('#ghost').remove();</script>`,
			wantErr: true,
		},
		{
			name:    "dynamic DOM gate stands down",
			body:    `<script>document.getElementById('ghost').innerHTML = '<p>x</p>';</script>`,
			wantErr: false,
		},
		{
			name:    "parse failure stands down",
			body:    `<script>document.getElementById('ghost')</script><script>broken {{{</script>`,
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := interactionErrs(t, tc.body)
			got := kindsOf(errs)[KindBrokenDOMQuery]
			if tc.wantErr != (got == 1) {
				t.Fatalf("wantErr=%v, got %+v", tc.wantErr, errs)
			}
		})
	}
}
