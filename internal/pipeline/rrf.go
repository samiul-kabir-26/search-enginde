package pipeline

import (
	"sort"
)

// DefaultRRFK is the RRF smoothing constant when the config leaves it unset.
const DefaultRRFK = 60

// RetrieverResult is one retriever's ranked hits and its fusion weight.
type RetrieverResult struct {
	Name   string
	Weight float64
	Hits   []Hit
}

// FuseRRF merges ranked lists with weighted Reciprocal Rank Fusion:
// score = Σ w_r / (k + rank_r). Ties break by product ID so the order is deterministic. The
// first hit's doc for a product is kept.
func FuseRRF(results []RetrieverResult, k int) []Candidate {
	if k <= 0 {
		k = DefaultRRFK
	}
	byID := map[string]*Candidate{}
	var order []string
	for _, r := range results {
		for i, h := range r.Hits {
			c, ok := byID[h.ProductID]
			if !ok {
				c = &Candidate{ProductID: h.ProductID, Doc: h.Doc, Retrievers: map[string]RetrieverHit{}, Features: map[string]float64{}}
				byID[h.ProductID] = c
				order = append(order, h.ProductID)
			}
			if _, dup := c.Retrievers[r.Name]; dup {
				continue
			}
			if c.Doc == nil {
				c.Doc = h.Doc
			}
			rank := i + 1
			c.Retrievers[r.Name] = RetrieverHit{Rank: rank, Score: h.Score}
			c.RRF += r.Weight / float64(k+rank)
		}
	}
	out := make([]Candidate, 0, len(order))
	for _, id := range order {
		c := byID[id]
		c.Score = c.RRF
		c.Stage = StageRetrieval
		out = append(out, *c)
	}
	SortByScore(out)
	return out
}

// SortByScore orders candidates by Score descending, then product ID.
func SortByScore(cs []Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Score != cs[j].Score {
			return cs[i].Score > cs[j].Score
		}
		return cs[i].ProductID < cs[j].ProductID
	})
}
