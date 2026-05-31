package editrec

import "testing"

func TestRecorderSetTemplate(t *testing.T) {
	t.Parallel()

	r := New("slug", "build", "prompt", "", 0)
	r.SetTemplate("landing-page")

	if got := r.transcript.Template; got != "landing-page" {
		t.Errorf("Template = %q, want %q", got, "landing-page")
	}
}

func TestRecorderSetTemplateNilReceiver(t *testing.T) {
	t.Parallel()

	// Start only stamps the template when one is present; a nil recorder
	// (transcript capture disabled) must be a no-op rather than panic.
	var r *Recorder
	r.SetTemplate("blank")
}
