package lint

import (
	"strings"
	"testing"
)

func TestCheckForms_UnnamedControls(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr int
	}{
		{
			name:    "all controls named passes",
			body:    `<form action="/api/submit" method="post"><input type="text" name="name"><textarea name="message"></textarea><button type="submit">Go</button></form>`,
			wantErr: 0,
		},
		{
			name:    "unnamed input in submitting form flags",
			body:    `<form action="/api/submit" method="post"><input type="text" name="name"><input type="text"></form>`,
			wantErr: 1,
		},
		{
			name:    "unnamed select and textarea both flag",
			body:    `<form action="/api/submit"><select><option>a</option></select><textarea></textarea></form>`,
			wantErr: 2,
		},
		{
			name:    "submit and button inputs are exempt",
			body:    `<form action="/api/submit"><input type="text" name="q"><input type="submit" value="Go"><input type="button" value="x"><input type="reset"><input type="image" src="x.png"></form>`,
			wantErr: 0,
		},
		{
			name:    "disabled control is exempt",
			body:    `<form action="/api/submit"><input type="text" name="q"><input type="text" disabled></form>`,
			wantErr: 0,
		},
		{
			name:    "form without action ignores unnamed controls",
			body:    `<form onsubmit="return false"><input type="text"></form>`,
			wantErr: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pi := pageOf(t, "index.html", `<!DOCTYPE html><html><body>`+tc.body+`</body></html>`)
			var got []Error
			for _, e := range checkForms(pi, linkCheckContext{}) {
				if e.Kind == KindFormControlUnnamed {
					got = append(got, e)
				}
			}
			if len(got) != tc.wantErr {
				t.Fatalf("got %d unnamed-control errors, want %d: %+v", len(got), tc.wantErr, got)
			}
		})
	}
}

func TestCheckForms_UnnamedMessageIsActionable(t *testing.T) {
	t.Parallel()

	pi := pageOf(t, "index.html", `<!DOCTYPE html><html><body>
<form action="/api/submit" method="post">
  <input type="text" name="name">
  <input type="email" name="email">
  <input type="tel">
</form></body></html>`)
	errs := checkForms(pi, linkCheckContext{})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %+v", errs)
	}
	msg := errs[0].Message
	for _, want := range []string{`<input type="tel">`, `"/api/submit"`, "email, name", "Add a name attribute"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestCheckForms_PostWithoutAction(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"post without action flags", `<form method="post"><input name="email"></form>`, true},
		{"POST case-insensitive", `<form method="POST"><input name="email"></form>`, true},
		{"post with action passes", `<form method="post" action="/api/submit"><input name="email"></form>`, false},
		{"get without action passes", `<form method="get"><input name="q"></form>`, false},
		{"no method without action passes", `<form onsubmit="return false"><input name="email"></form>`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pi := pageOf(t, "index.html", `<!DOCTYPE html><html><body>`+tc.body+`</body></html>`)
			var got []Error
			for _, e := range checkForms(pi, linkCheckContext{}) {
				if e.Kind == KindFormPostNoAction {
					got = append(got, e)
				}
			}
			if tc.wantErr != (len(got) == 1) {
				t.Fatalf("wantErr=%v, got %+v", tc.wantErr, got)
			}
		})
	}
}

// TestCheckForms_WaitlistSkeletonClean pins the platform's own intentional
// no-action form (waitlist skeleton, verbatim): JS shows a confirmation and
// returns false. No form check may flag it.
func TestCheckForms_WaitlistSkeletonClean(t *testing.T) {
	t.Parallel()

	pi := pageOf(t, "index.html", `<!DOCTYPE html><html><body>
<form id="waitlist-form" onsubmit="document.getElementById('thanks').style.display='block'; this.style.display='none'; return false;" class="flex flex-wrap gap-2">
  <input type="email" name="email" placeholder="you@example.com" autocomplete="email" required class="input flex-1 min-w-56 text-base-content">
  <button type="submit" class="btn btn-primary">Join waitlist</button>
</form></body></html>`)
	if errs := checkForms(pi, linkCheckContext{}); len(errs) != 0 {
		t.Fatalf("the waitlist skeleton form must stay clean: %+v", errs)
	}
}

func TestCheckForms_Multipart(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
		want int
	}{
		{"file input anywhere flags", `<input type="file">`, 1},
		{"file input inside form flags", `<form action="/api/up"><input type="file" name="doc"></form>`, 1},
		{"enctype multipart flags", `<form action="/api/up" enctype="multipart/form-data"><input name="x"></form>`, 1},
		{"both flag twice", `<form action="/api/up" enctype="MULTIPART/FORM-DATA"><input type="FILE" name="doc"></form>`, 2},
		{"urlencoded form passes", `<form action="/api/up" enctype="application/x-www-form-urlencoded"><input name="x"></form>`, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pi := pageOf(t, "index.html", `<!DOCTYPE html><html><body>`+tc.body+`</body></html>`)
			var got []Error
			for _, e := range checkForms(pi, linkCheckContext{}) {
				if e.Kind == KindMultipartForm {
					got = append(got, e)
				}
			}
			if len(got) != tc.want {
				t.Fatalf("got %d multipart errors, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

// TestCheckForms_PhotoWallUploadExempt pins the exemption: the event-photo-wall
// upload form legitimately posts multipart with a file input to the dedicated
// /_photos endpoint, so on a photo-wall site it must lint clean — while the same
// form on any other site still trips the data-loss checks.
func TestCheckForms_PhotoWallUploadExempt(t *testing.T) {
	t.Parallel()

	const uploadForm = `<!DOCTYPE html><html><body>
<form method="POST" action="/_photos" enctype="multipart/form-data">
  <input type="file" name="photo" accept="image/*" required>
  <button type="submit">Upload</button>
</form></body></html>`
	pi := pageOf(t, "index.html", uploadForm)

	if errs := checkForms(pi, linkCheckContext{photoWall: true}); len(errs) != 0 {
		t.Fatalf("photo-wall upload form must lint clean, got %+v", errs)
	}

	var multipartErrs int
	for _, e := range checkForms(pi, linkCheckContext{}) {
		if e.Kind == KindMultipartForm {
			multipartErrs++
		}
	}
	if multipartErrs != 2 {
		t.Fatalf("without the wall flag the form should flag file input + enctype (2), got %d", multipartErrs)
	}
}

// TestCheckFetchTargets_PhotoWallApprovedExempt pins the display page's poll:
// fetch('/_photos/approved') is a platform endpoint, valid on a photo-wall site
// and a broken fetch anywhere else.
func TestCheckFetchTargets_PhotoWallApprovedExempt(t *testing.T) {
	t.Parallel()

	pi := pageOf(t, "display.html", `<!DOCTYPE html><html><body><script>fetch('/_photos/approved')</script></body></html>`)
	facts := collectJSFacts("display.html", pi.scripts)

	if errs := checkFetchTargets(pi, facts, linkCheckContext{photoWall: true}); len(errs) != 0 {
		t.Fatalf("photo-wall poll fetch must be exempt, got %+v", errs)
	}
	if errs := checkFetchTargets(pi, facts, linkCheckContext{}); len(errs) != 1 {
		t.Fatalf("non-wall site should flag the /_photos/approved fetch, got %+v", errs)
	}
}

func TestCheckFetchTargets(t *testing.T) {
	t.Parallel()

	fileSet := map[string]bool{
		"index.html":        true,
		"data.json":         true,
		"functions/list.js": true,
	}

	cases := []struct {
		name       string
		js         string
		enablesFns bool
		wantErr    bool
		wantInMsg  string
	}{
		{"existing file passes", `fetch('data.json')`, false, false, ""},
		{"missing file flags with file list", `fetch('missing.json')`, false, true, "Site files: data.json, index.html"},
		{"api with backing function passes", `fetch('/api/list')`, false, false, ""},
		{"api allowed when functions enabled", `fetch('/api/anything')`, true, false, ""},
		{"api without backing flags with create hint", `fetch('/api/ghost')`, false, true, "functions/ghost.js"},
		{"api with query strips name", `fetch('/api/ghost?id=1')`, false, true, "Create functions/ghost.js"},
		{"external skipped", `fetch('https://example.com/x.json')`, false, false, ""},
		{"dynamic skipped", `fetch('/api/' + name)`, false, false, ""},
		{"duplicate reported once", `fetch('nope.json'); fetch('nope.json')`, false, true, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pi := pageOf(t, "index.html", `<!DOCTYPE html><html><body><script>`+tc.js+`</script></body></html>`)
			facts := collectJSFacts("index.html", pi.scripts)
			errs := checkFetchTargets(pi, facts, linkCheckContext{fileSet: fileSet, enablesFns: tc.enablesFns})
			if tc.wantErr && len(errs) != 1 {
				t.Fatalf("want exactly 1 error, got %+v", errs)
			}
			if !tc.wantErr && len(errs) != 0 {
				t.Fatalf("want no errors, got %+v", errs)
			}
			if tc.wantInMsg != "" && !strings.Contains(errs[0].Message, tc.wantInMsg) {
				t.Errorf("message missing %q:\n%s", tc.wantInMsg, errs[0].Message)
			}
			for _, e := range errs {
				if e.Kind != KindBrokenFetch {
					t.Errorf("Kind = %q, want %q", e.Kind, KindBrokenFetch)
				}
			}
		})
	}
}
