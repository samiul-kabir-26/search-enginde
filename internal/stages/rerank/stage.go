package rerank

import (
	"context"
	"sort"
	"time"

	"github.com/zavisoft/search-engine/internal/ads"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/resilience"
)

// Option configures the S6 stage.
type Option func(*Stage)

// WithBreakers sets the circuit breaker provider for remote re-rankers.
func WithBreakers(b *resilience.Breakers) Option {
	return func(s *Stage) { s.breakers = b }
}

// WithBlender sets the ads blender for sponsored placements.
func WithBlender(b *ads.Blender) Option {
	return func(s *Stage) { s.blender = b }
}

// WithClock overrides the clock for testing.
func WithClock(fn func() time.Time) Option {
	return func(s *Stage) { s.now = fn }
}

// Stage is S6. Personalization, vendor diversity and the ads blend (PLAN.md M8).
type Stage struct {
	breakers *resilience.Breakers
	blender  *ads.Blender
	now      func() time.Time
}

// New returns the S6 stage.
func New(opts ...Option) *Stage {
	s := &Stage{now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Name implements pipeline.Stage.
func (*Stage) Name() string { return pipeline.StageRerank }

// Run implements pipeline.Stage.
func (s *Stage) Run(ctx context.Context, st *pipeline.State) (pipeline.Report, error) {
	if len(st.Candidates) == 0 {
		return pipeline.Report{Provider: "passthrough"}, nil
	}

	rep := pipeline.Report{Provider: "passthrough"}
	if st.Resolved == nil || st.Resolved.Config == nil {
		if d := st.Query.Depth; d > 0 && len(st.Candidates) > d {
			st.Candidates = st.Candidates[:d]
		}
		return rep, nil
	}

	cfg := st.Resolved.Config
	now := s.now()

	// Personalization and a BYO re-ranker re-order only the candidates S5 ranked: the tail
	// behind them keeps its pre-rank and RRF order (PLAN.md R4), on another score scale.
	n := 0
	for n < len(st.Candidates) && st.Candidates[n].Stage == pipeline.StageRank {
		n++
	}
	ranked := st.Candidates[:n]

	// 1. Personalization: BYO re-ranker OR built-in formula
	if st.Providers != nil && st.Providers.Rerank != nil && n > 0 {
		scorer := st.Providers.Rerank
		providerName := st.Providers.Names["rerank"]
		if providerName == "" {
			providerName = "byo:rerank"
		}
		rep.Provider = providerName

		key := resilience.Key{Tenant: string(st.Resolved.Tenant.ID), Stage: pipeline.StageRerank, Provider: providerName}
		var scores []float32
		var out resilience.Outcome
		var err error
		if s.breakers != nil {
			scores, out, err = resilience.Call(ctx, s.breakers, key, func(c context.Context) ([]float32, error) {
				return scorer.Score(c, st.Query, ranked)
			}, nil)
		} else {
			scores, err = scorer.Score(ctx, st.Query, ranked)
		}
		if err != nil || len(scores) != n {
			rep.Fallback = true
			rep.FallbackReason = out.Reason
			if rep.FallbackReason == "" {
				rep.FallbackReason = resilience.ReasonError
			}
		} else {
			for i := range ranked {
				ranked[i].Score = float64(scores[i])
				ranked[i].Stage = pipeline.StageRerank
			}
			sortByScore(ranked)
		}
	} else if p := cfg.Rerank.Personalization; p != nil && p.Enabled {
		// Built-in personalization: rank + α·affinity + β·price_band_match
		modified := false
		for i := range ranked {
			c := &ranked[i]
			aff := c.Features[feature.UserCategoryAffinity]
			priceMatch := c.Features[feature.UserPriceBandMatch]
			if aff != 0 || priceMatch != 0 {
				c.Score += p.Alpha*aff + p.Beta*priceMatch
				modified = true
			}
		}
		if modified {
			rep.Provider = "personalization"
			for i := range ranked {
				ranked[i].Stage = pipeline.StageRerank
			}
			sortByScore(ranked)
		}
	}

	// 2. Diversity: greedy max_per_vendor per window
	if d := cfg.Rerank.Diversity; d != nil && d.MaxPerVendor > 0 && d.Window > 0 {
		st.Candidates = enforceDiversity(st.Candidates, d.MaxPerVendor, d.Window)
	}

	// 3. Ads: blend sponsored placements into configured slots
	if cfg.Ads.Enabled && len(cfg.Ads.Slots) > 0 && s.blender != nil {
		s.blender.Blend(ctx, st, cfg.Ads, now)
	}

	// 4. Depth cut
	if d := st.Query.Depth; d > 0 && len(st.Candidates) > d {
		st.Candidates = st.Candidates[:d]
	}

	return rep, nil
}

// sortByScore orders candidates by score, highest first, ties by product ID (as S4 and S5).
func sortByScore(cs []pipeline.Candidate) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].Score != cs[j].Score {
			return cs[i].Score > cs[j].Score
		}
		return cs[i].ProductID < cs[j].ProductID
	})
}

// enforceDiversity greedily places, at each position, the best remaining candidate whose vendor
// appears fewer than maxPerVendor times among the window-1 results before it, so no window of
// `window` consecutive results holds more than maxPerVendor of one vendor. When nothing fits,
// the best remaining candidate is placed anyway. Candidates without a vendor always fit.
func enforceDiversity(cs []pipeline.Candidate, maxPerVendor, window int) []pipeline.Candidate {
	if maxPerVendor <= 0 || window <= 0 || len(cs) <= 1 {
		return cs
	}
	vendor := func(c pipeline.Candidate) string {
		if c.Doc == nil {
			return ""
		}
		return c.Doc.VendorID
	}
	out := make([]pipeline.Candidate, 0, len(cs))
	counts := make(map[string]int)
	fits := func(c pipeline.Candidate) bool {
		v := vendor(c)
		return v == "" || counts[v] < maxPerVendor
	}
	place := func(c pipeline.Candidate) {
		out = append(out, c)
		if v := vendor(c); v != "" {
			counts[v]++
		}
		// Keep counts over the last window-1 placed results.
		if k := len(out) - window; k >= 0 {
			if v := vendor(out[k]); v != "" {
				counts[v]--
			}
		}
	}
	// deferred holds skipped candidates in their original order; they all rank above the
	// unread input, so the best remaining fit is the first fitting deferred one, else the
	// first fitting input one.
	var deferred []pipeline.Candidate
	next := 0
	for len(out) < len(cs) {
		picked := false
		for i, c := range deferred {
			if fits(c) {
				deferred = append(deferred[:i], deferred[i+1:]...)
				place(c)
				picked = true
				break
			}
		}
		for !picked && next < len(cs) {
			c := cs[next]
			next++
			if fits(c) {
				place(c)
				picked = true
			} else {
				deferred = append(deferred, c)
			}
		}
		if !picked {
			place(deferred[0])
			deferred = deferred[1:]
		}
	}
	return out
}
