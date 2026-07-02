package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// shouldSkipChrome reports whether a chromedp error should downgrade the
// test to a skip (Chrome unavailable / network unreachable) rather than a
// hard failure.
func shouldSkipChrome(err error) bool {
	msg := err.Error()

	return strings.Contains(msg, "chrome failed to start") ||
		strings.Contains(msg, "exec:") ||
		strings.Contains(msg, "context deadline exceeded")
}

// jsString quotes a string for embedding in a JS source snippet sent to
// chromedp.Evaluate. Quoting via %q is enough since the values used here
// are plain ASCII; we keep it tiny rather than pulling in encoding/json
// for one literal.
func jsString(s string) string {
	var b strings.Builder

	b.WriteByte('"')

	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}

	b.WriteByte('"')

	return b.String()
}

// dumpInsertState captures the in-page state we need to triage which step
// of the image-drawer Insert flow broke. Best-effort: the caller has already
// decided to fail, so an evaluate error here just means the diagnostic is
// empty.
func dumpInsertState(t *testing.T, ctx context.Context) string {
	t.Helper()
	var dump string
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = chromedp.Run(probeCtx, chromedp.Evaluate(`(function(){
		try {
			var panel = document.getElementById('tb-drawer-panel');
			var detail = document.getElementById('tb-drawer-detail');
			var status = document.getElementById('tb-drawer-status');
			var grid = document.getElementById('tb-drawer-grid');
			var selected = (window.TBImageDrawer && window.TBImageDrawer.selected) || null;
			return JSON.stringify({
				panelOpen: panel ? panel.dataset.open : null,
				detailHidden: detail ? detail.hidden : null,
				drawerStatus: status ? status.textContent : null,
				gridCount: grid ? grid.querySelectorAll('.tb-drawer-card').length : null,
				selectedPath: selected ? selected.path : null,
			});
		} catch (e) { return 'dump-failed: ' + e; }
	})()`, &dump))
	return "diagnostic: " + dump
}
