package prerank

import (
	"context"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/stages/scoring"
)

// Stage is S4.
type Stage struct {
	breakers *resilience.Breakers
	now      func() time.Time
}

// New returns the S4 stage.
func New(b *resilience.Breakers) *Stage { return &Stage{breakers: b, now: time.Now} }

// Name implements pipeline.Stage.
func (*Stage) Name() string { return pipeline.StagePrerank }

// Run computes every candidate's request-local features, then scores and trims to prerank.keep.
func (s *Stage) Run(ctx context.Context, st *pipeline.State) (pipeline.Report, error) {
	BuildFeatures(st, s.now())
	cfg := st.Resolved.Config
	rep := scoring.Apply(ctx, s.breakers, st, s.Name(), pipeline.StageRetrieval, st.Providers.Prerank,
		st.Providers.Names["prerank"], cfg.Prerank.Keep)
	return rep, nil
}

// BuildFeatures fills Candidate.Features from retrieval scores, the stored doc and the query.
func BuildFeatures(st *pipeline.State, now time.Time) {
	q := st.Query
	cats := make(map[string]float64, len(q.Categories))
	for _, c := range q.Categories {
		cats[c.ID] = c.Confidence
	}
	total := len(retrieval.Retrievers(st.Resolved.Config, q))
	in := make([]feature.Input, len(st.Candidates))
	for i, c := range st.Candidates {
		scores := make(map[string]float64, len(c.Retrievers))
		for name, h := range c.Retrievers {
			scores[name] = h.Score
		}
		fi := feature.Input{RRF: c.RRF, Retrievers: scores}
		if d := c.Doc; d != nil {
			fi.PriceMinor, fi.Rating, fi.RatingCount = d.PriceMinor, d.Rating, d.RatingCount
			fi.ListingCompleteness, fi.ImageCount, fi.CreatedAt, fi.CategoryIDs = d.ListingCompleteness, d.ImageCount, d.CreatedAt, d.CategoryIDs
		}
		in[i] = fi
	}
	for i, f := range feature.Build(feature.QueryInput{Categories: cats, RetrieverTotal: total}, in, now) {
		st.Candidates[i].Features = f
	}
}
