// Package scoring is the shared scoring step of S4 and S5: score the candidates the previous
// stage kept, keep the top ones, and leave the cut ones in their previous order so deep pages
// follow pre-rank and then RRF order (PLAN.md R4).
package scoring

import (
	"context"
	"sort"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/resilience"
)

// Apply scores the prefix of st.Candidates last scored by prev, keeps the best keep of them
// (marked with stage), and appends the cut ones and the untouched tail in their prior order.
// A scorer failure passes the input through and is reported as a fallback.
func Apply(ctx context.Context, b *resilience.Breakers, st *pipeline.State, stage, prev string, sc pipeline.Scorer, provider string, keep int) pipeline.Report {
	rep := pipeline.Report{Provider: provider}
	cs := st.Candidates
	n := 0
	for n < len(cs) && cs[n].Stage == prev {
		n++
	}
	if n == 0 || sc == nil {
		return rep
	}
	pool := cs[:n]
	key := resilience.Key{Tenant: string(st.Resolved.Tenant.ID), Stage: stage, Provider: provider}
	scores, out, err := resilience.Call(ctx, b, key, func(ctx context.Context) ([]float32, error) {
		return sc.Score(ctx, st.Query, pool)
	}, nil)
	if err != nil || len(scores) != n {
		rep.Fallback, rep.FallbackReason = true, out.Reason
		if rep.FallbackReason == "" {
			rep.FallbackReason = resilience.ReasonError
		}
		return rep
	}
	idx := make([]int, n)
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if scores[idx[a]] != scores[idx[b]] {
			return scores[idx[a]] > scores[idx[b]]
		}
		return pool[idx[a]].ProductID < pool[idx[b]].ProductID
	})
	keep = min(keep, n)
	kept := make(map[int]bool, keep)
	out2 := make([]pipeline.Candidate, 0, len(cs))
	for _, i := range idx[:keep] {
		c := pool[i]
		c.Score = float64(scores[i])
		c.Stage = stage
		out2 = append(out2, c)
		kept[i] = true
	}
	for i, c := range pool {
		if !kept[i] {
			out2 = append(out2, c)
		}
	}
	out2 = append(out2, cs[n:]...)
	st.Candidates = out2
	return rep
}
