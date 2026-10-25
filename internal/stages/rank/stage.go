package rank

import (
	"context"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/stages/scoring"
)

// Stage is S5.
type Stage struct {
	breakers *resilience.Breakers
}

// New returns the S5 stage.
func New(b *resilience.Breakers) *Stage { return &Stage{breakers: b} }

// Name implements pipeline.Stage.
func (*Stage) Name() string { return pipeline.StageRank }

// Run scores the pre-ranked candidates in one batch and trims to rank.keep.
func (s *Stage) Run(ctx context.Context, st *pipeline.State) (pipeline.Report, error) {
	cfg := st.Resolved.Config
	return scoring.Apply(ctx, s.breakers, st, s.Name(), pipeline.StagePrerank, st.Providers.Rank,
		st.Providers.Names["rank"], cfg.Rank.Keep), nil
}
