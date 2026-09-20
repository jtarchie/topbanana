package server

import (
	"net/url"
	"testing"
)

// TestURLEscape_RoundTripsNonASCII: flash strings are typographic (em dashes,
// curly apostrophes) and land in a redirect's query string, so escaping has to
// be over UTF-8 bytes. Percent-encoding the rune value instead turned "—"
// (U+2014) into "%2014", which a browser decodes as a space plus "14" — the
// operator read a mangled sentence.
func TestURLEscape_RoundTripsNonASCII(t *testing.T) {
	t.Parallel()

	for _, in := range []string{
		"Enable a@b.com first — a disabled account can't enroll a passkey.",
		"plain ascii + symbols &=?#",
		"emoji 🍌 and accents café",
	} {
		got, err := url.QueryUnescape(urlEscape(in))
		if err != nil {
			t.Errorf("urlEscape(%q) is not decodable: %v", in, err)
			continue
		}
		if got != in {
			t.Errorf("round trip of %q = %q", in, got)
		}
	}
}
