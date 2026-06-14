package server_test

import "strings"

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
