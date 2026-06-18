package docs

import (
	"math"
	"strings"
)

const (
	// headingWeight is how much a token in a chunk's heading counts versus the
	// same token in its body. A query equal to a component name ("badge") thus
	// ranks that component above a chunk that only mentions it in prose.
	headingWeight = 3.0
	// bm25K1 / bm25B are the BM25 term-saturation and length-normalization
	// parameters. They are tuned looser than the textbook 1.2/0.75: a component
	// that *defines* a class family repeats its prefix many times (the button
	// section lists ~25 "btn-*" classes), so we want repetition to keep paying
	// (higher k1) and we don't want the canonical, longer sections penalized
	// against a shorter section that merely *uses* the class (lower b).
	bm25K1 = 2.0
	bm25B  = 0.4
	// exactClassBonus rewards a hyphenated query token (a literal class name
	// like "btn-primary") that appears verbatim in a chunk.
	exactClassBonus = 2.0
	// headingMatchBonus fires when a query term equals a token of the chunk's
	// heading — i.e. the query names this exact component. This is what makes
	// "primary button" rank the button section over a section that merely uses
	// buttons heavily (e.g. fab), where raw term frequency alone would not.
	headingMatchBonus = 4.0
	// componentMatchBonus fires when a query term is this component's class or a
	// member of its class family (query "btn-primary" against the section whose
	// "- component:" is `btn`). It pins the *defining* component above sections
	// that only use the class in examples.
	componentMatchBonus = 6.0
)

// tokenize lowercases s and splits it into search tokens. Words break on any
// character that is not [a-z0-9-_], so a class name like "btn-primary" survives
// as one word; that word is then split on -/_ into its parts AND re-emitted in
// joined "-" form. So "btn-primary" -> ["btn", "primary", "btn-primary"], while
// "primary button" -> ["primary", "button"]. The dual emission lets "primary
// button" match via the parts and an exact "btn-primary" match via the joined
// form. The same function tokenizes both queries and chunk text.
func tokenize(s string) []string {
	s = strings.ToLower(s)
	var (
		out  []string
		word strings.Builder
	)
	flush := func() {
		if word.Len() == 0 {
			return
		}
		w := word.String()
		word.Reset()
		parts := strings.FieldsFunc(w, func(r rune) bool { return r == '-' || r == '_' })
		out = append(out, parts...)
		if len(parts) > 1 {
			out = append(out, strings.Join(parts, "-"))
		}
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			word.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// dedupe returns the distinct elements of in, preserving first-seen order, so a
// repeated query term is scored once.
func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// scoreChunk is BM25 over the chunk corpus (heading tokens pre-weighted into
// termFreq; see buildIndex) plus three bonuses that encode "this section is
// *about* the query, not just mentions it": an exact class-token hit, an exact
// component-name (heading) hit, and a component-class-family hit.
func scoreChunk(sc scoredChunk, qTerms []string, qHasClass map[string]bool) float64 {
	n := float64(len(corpus))
	cc := sc.chunk.componentClass
	score := 0.0
	for _, t := range qTerms {
		if f := sc.termFreq[t]; f > 0 {
			df := docFreq[t]
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			denom := f + bm25K1*(1-bm25B+bm25B*sc.docLen/avgDocLen)
			score += idf * (f * (bm25K1 + 1)) / denom
			if qHasClass[t] {
				score += exactClassBonus
			}
		}
		if containsTok(sc.chunk.headingTok, t) {
			score += headingMatchBonus
		}
		if cc != "" && (t == cc || (qHasClass[t] && strings.HasPrefix(t, cc+"-"))) {
			score += componentMatchBonus
		}
	}
	return score
}

// containsTok reports whether toks contains t. toks are heading tokens (a
// handful), so a linear scan is cheaper than a map.
func containsTok(toks []string, t string) bool {
	for _, x := range toks {
		if x == t {
			return true
		}
	}
	return false
}
