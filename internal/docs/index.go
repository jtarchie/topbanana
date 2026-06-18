package docs

import "sync"

// scoredChunk is a Chunk plus its precomputed BM25 inputs: termFreq folds the
// heading-weight boost in (heading tokens count headingWeight each, body tokens
// 1 each) and docLen is the resulting weighted length.
type scoredChunk struct {
	chunk    Chunk
	termFreq map[string]float64
	docLen   float64
}

var (
	indexOnce sync.Once
	corpus    []scoredChunk
	docFreq   map[string]float64
	avgDocLen float64
)

// buildIndex chunks every source and computes the per-chunk term frequencies
// plus the corpus-wide document frequencies and average length BM25 needs. It
// runs once, lazily, via ensureIndex: the search tool is pull-only, so a build
// that never calls it pays nothing.
func buildIndex() {
	for _, src := range sources {
		for _, ch := range chunkSource(src) {
			tf := make(map[string]float64, len(ch.bodyTok))
			for _, t := range ch.bodyTok {
				tf[t]++
			}
			for _, t := range ch.headingTok {
				tf[t] += headingWeight
			}
			dl := 0.0
			for _, v := range tf {
				dl += v
			}
			corpus = append(corpus, scoredChunk{chunk: ch, termFreq: tf, docLen: dl})
		}
	}

	docFreq = make(map[string]float64)
	total := 0.0
	for _, sc := range corpus {
		total += sc.docLen
		for t := range sc.termFreq {
			docFreq[t]++
		}
	}
	if len(corpus) > 0 {
		avgDocLen = total / float64(len(corpus))
	}
}

// ensureIndex builds the index exactly once. sync.Once publishes the immutable
// corpus/docFreq/avgDocLen before any reader proceeds, and Search never mutates
// them, so concurrent searches (the ADK runner fans tool calls out) are safe.
func ensureIndex() { indexOnce.Do(buildIndex) }
