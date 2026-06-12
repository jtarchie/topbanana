package build

import "strings"

// HumanizeFailure turns a raw build-failure error string into plain language a
// non-technical user can act on. It returns a friendly headline, an actionable
// hint, and the raw detail — which is always preserved verbatim so the UI can
// tuck it behind a "Technical details" disclosure for debugging.
//
// The build's user-facing failures are a small, finite set composed in
// build.go (timeout, lint-retry exhaustion, wrapped agent errors). Lint
// exhaustion joins one or more lint.Error messages, so we match on the stable
// substrings those messages carry (see internal/lint/lint.go) rather than on
// whole strings, which embed file paths and counts. friendly_test.go runs real
// lint output through this function so a reworded lint message fails loudly
// instead of silently degrading to the generic bucket.
func HumanizeFailure(raw string) (headline, hint, detail string) {
	for _, r := range friendlyRules {
		if strings.Contains(raw, r.match) {
			return r.headline, r.hint, raw
		}
	}
	return "Something went wrong while building your site.",
		"Try again — and if it keeps happening, try simplifying your request.",
		raw
}

// friendlyRule maps a stable substring of a raw failure to user-facing copy.
type friendlyRule struct {
	match    string
	headline string
	hint     string
}

// friendlyRules is ordered most-specific first. The lint substrings come before
// the generic "lint errors after" catch-all so a recognized cause wins over the
// vague wrapper. "missing stylesheet" precedes "closing quote" because the
// design-substrate lint message mentions a missing closing quote in its own
// explanatory text — the more specific cause should match first.
var friendlyRules = []friendlyRule{
	{
		match:    "timed out",
		headline: "This is taking longer than expected.",
		hint:     "Try again, or break your request into one change at a time.",
	},
	{
		match:    "missing stylesheet",
		headline: "A page was missing its styling.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "missing responsive viewport",
		headline: "A page wasn't set up to look right on phones.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "closing quote",
		headline: "A page had a formatting glitch we couldn't fix automatically.",
		hint:     "Try again, or describe the change in simpler terms.",
	},
	{
		match:    "broken link",
		headline: "A link pointed to a page that doesn't exist yet.",
		hint:     "Try again — describe all the pages you'd like and we'll connect them.",
	},
	{
		match:    "non-empty index.html",
		headline: "Your site didn't end up with a home page.",
		hint:     "Try again and describe what the main page should show.",
	},
	{
		match:    "required by",
		headline: "A required part of the chosen template was missing.",
		hint:     "Try again, or pick a different template to start from.",
	},
	{
		match:    "broken anchor",
		headline: "A button or link pointed at a section that doesn't exist.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "missing character encoding",
		headline: "A page was missing a basic browser setting.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "missing language",
		headline: "A page didn't say what language it's written in.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "missing <title>",
		headline: "A page was missing its name.",
		hint:     "Try again and mention what each page should be called.",
	},
	{
		match:    "duplicate <title>",
		headline: "Several pages ended up with the same name.",
		hint:     "Try again — page names usually sort themselves out on a second pass.",
	},
	{
		match:    "missing meta description",
		headline: "A page was missing its search-result summary.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "form control will not submit",
		headline: "A form field wasn't wired up to send its answer.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "form posts nowhere",
		headline: "A form had nowhere to send what visitors type.",
		hint:     "Try again and describe what should happen when someone submits the form.",
	},
	{
		match:    "file upload won't reach",
		headline: "A form tried to collect a file, which form handlers here can't receive.",
		hint:     "Ask visitors for a link or a text answer instead of a file.",
	},
	{
		match:    "broken fetch",
		headline: "The page tried to load something that doesn't exist yet.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "broken label",
		headline: "A form label wasn't connected to its field.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "duplicate id",
		headline: "Two parts of a page accidentally shared the same internal name.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "broken mailto link",
		headline: "An email link didn't contain a real email address.",
		hint:     "Double-check the contact email address and try again.",
	},
	{
		match:    "broken tel link",
		headline: "A phone link didn't contain a real phone number.",
		hint:     "Double-check the phone number and try again.",
	},
	{
		match:    "undefined handler",
		headline: "A button was wired to an action that doesn't exist.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "broken DOM lookup",
		headline: "A script pointed at a part of the page that doesn't exist.",
		hint:     "Try again — this usually clears up on a second attempt.",
	},
	{
		match:    "external script",
		headline: "A page tried to load code from another website.",
		hint:     "Sites here are self-contained — try again and the code will be built in.",
	},
	{
		match:    "external stylesheet",
		headline: "A page tried to load styling from another website.",
		hint:     "Sites here are self-contained — try again and the styling will be built in.",
	},
	{
		match:    "insecure http:// URL",
		headline: "A page pointed at content over an insecure connection.",
		hint:     "Try again — secure links usually sort themselves out on a second pass.",
	},
	{
		match:    "unreachable page",
		headline: "A page was created but nothing linked to it.",
		hint:     "Try again and mention where the page should appear in the menu.",
	},
	{
		match:    "lint errors after",
		headline: "We built your site, but a few things didn't pass our checks.",
		hint:     "Try again — small changes usually clear it up.",
	},
}
