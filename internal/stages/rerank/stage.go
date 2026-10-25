package rerank

import (
	"context"

	"github.com/zavisoft/search-engine/internal/pipeline"
)

// Stage is S6. Personalization, vendor diversity and the ads blend arrive in M8; until then it
// passes the ranking through and trims the list to the requested depth.
type Stage struct{}

// New returns the S6 stage.
func New() *Stage { return &Stage{} }

// Name implements pipeline.Stage.
func (*Stage) Name() string { return pipeline.StageRerank }

// Run implements pipeline.Stage.
func (*Stage) Run(_ context.Context, st *pipeline.State) (pipeline.Report, error) {
	if d := st.Query.Depth; d > 0 && len(st.Candidates) > d {
		st.Candidates = st.Candidates[:d]
	}
	return pipeline.Report{Provider: "passthrough"}, nil
}
