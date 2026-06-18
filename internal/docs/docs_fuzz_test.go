package docs

import (
	"strings"
	"testing"
)

// FuzzSearch asserts the ranker survives arbitrary model-supplied queries: it
// never panics and always honors its caps and sort order. The query is
// untrusted text from the LLM, so an adversarial input must not blow the byte
// budget or crash the build.
func FuzzSearch(f *testing.F) {
	for _, s := range []string{
		"", "badge", "btn-primary", "primary button", "theme colors",
		"   ", "!!!", strings.Repeat("#", 1000), strings.Repeat("a ", 500),
		"café", "\x00\x01", "BTN-PRIMARY", "card_body", "----",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		res := Search(q, Options{})
		if len(res) > HardMaxResults {
			t.Fatalf("returned %d results > HardMaxResults %d for %q", len(res), HardMaxResults, q)
		}
		total := 0
		last := 0.0
		for i, r := range res {
			if len(r.Body) > DefaultChunkBytes {
				t.Fatalf("body %d bytes > cap %d for %q", len(r.Body), DefaultChunkBytes, q)
			}
			total += len(r.Body)
			if i > 0 && r.Score > last {
				t.Fatalf("results not sorted by score desc at %d (%v > %v) for %q", i, r.Score, last, q)
			}
			last = r.Score
		}
		if total > DefaultTotalBytes {
			t.Fatalf("total body %d > budget %d for %q", total, DefaultTotalBytes, q)
		}
	})
}
