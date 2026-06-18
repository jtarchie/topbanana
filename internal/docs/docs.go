package docs

import (
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// Result caps. They are deliberately tighter than grep's match caps (50/200):
// each docs chunk is a paragraph plus a class list, and the point is to inject a
// small focused reference, not a dump.
const (
	DefaultMaxResults = 5
	HardMaxResults    = 10
	DefaultChunkBytes = 1500 // per-result body
	DefaultTotalBytes = 6000 // total body across all results
)

// Options tunes a Search. The zero value is the documented defaults.
type Options struct {
	MaxResults    int    // clamped to [1, HardMaxResults]; 0 => DefaultMaxResults
	MaxChunkBytes int    // 0 => DefaultChunkBytes
	TotalBytes    int    // 0 => DefaultTotalBytes
	Source        string // "" => all sources; else a sourceDef.ID ("daisyui")
}

// Result is one returned chunk: a breadcrumb the model can cite, the (possibly
// truncated) reference body, and the relevance score.
type Result struct {
	Source     string  `json:"source"`
	Breadcrumb string  `json:"breadcrumb"`
	Body       string  `json:"body"`
	Score      float64 `json:"score"`
	Truncated  bool    `json:"truncated,omitempty"`
}

// SourceInfo describes an embedded corpus, so a tool can report what is actually
// searchable and at what version.
type SourceInfo struct {
	Name    string `json:"name"`
	ID      string `json:"id"`
	Version string `json:"version"`
}

func (o Options) withDefaults() Options {
	switch {
	case o.MaxResults <= 0:
		o.MaxResults = DefaultMaxResults
	case o.MaxResults > HardMaxResults:
		o.MaxResults = HardMaxResults
	}
	if o.MaxChunkBytes <= 0 {
		o.MaxChunkBytes = DefaultChunkBytes
	}
	if o.TotalBytes <= 0 {
		o.TotalBytes = DefaultTotalBytes
	}
	return o
}

// Search returns the top documentation chunks for query, honoring opts (the
// zero value uses the defaults). It is pure and deterministic: the same query
// and opts always yield the same ordered results.
func Search(query string, opts Options) []Result {
	ensureIndex()
	opts = opts.withDefaults()

	qTerms := dedupe(tokenize(query))
	if len(qTerms) == 0 {
		return nil
	}
	qHasClass := make(map[string]bool, len(qTerms))
	for _, t := range qTerms {
		if strings.Contains(t, "-") {
			qHasClass[t] = true
		}
	}

	type hit struct {
		sc    scoredChunk
		score float64
	}
	var hits []hit
	for _, sc := range corpus {
		if opts.Source != "" && sc.chunk.SourceID != opts.Source {
			continue
		}
		if s := scoreChunk(sc, qTerms, qHasClass); s > 0 {
			hits = append(hits, hit{sc: sc, score: s})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].sc.chunk.Breadcrumb < hits[j].sc.chunk.Breadcrumb
	})

	results := make([]Result, 0, opts.MaxResults)
	total := 0
	for _, h := range hits {
		if len(results) >= opts.MaxResults {
			break
		}
		body, truncated := capBytes(h.sc.chunk.Body, opts.MaxChunkBytes)
		// Always allow the first result through; afterwards stop before
		// blowing the total byte budget.
		if len(results) > 0 && total+len(body) > opts.TotalBytes {
			break
		}
		total += len(body)
		results = append(results, Result{
			Source:     h.sc.chunk.Source,
			Breadcrumb: h.sc.chunk.Breadcrumb,
			Body:       body,
			Score:      round3(h.score),
			Truncated:  truncated,
		})
	}
	return results
}

// Sources lists the embedded corpora (name, id, vendored version).
func Sources() []SourceInfo {
	out := make([]SourceInfo, 0, len(sources))
	for _, s := range sources {
		out = append(out, SourceInfo{Name: s.Name, ID: s.ID, Version: s.Version})
	}
	return out
}

// capBytes truncates s to at most limit bytes on a rune boundary, preferring
// the last newline in the second half for a clean break. The returned string is
// always <= limit bytes; the bool reports whether it was cut.
func capBytes(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if nl := strings.LastIndexByte(s[:cut], '\n'); nl > limit/2 {
		cut = nl
	}
	return s[:cut], true
}

func round3(x float64) float64 { return math.Round(x*1000) / 1000 }
