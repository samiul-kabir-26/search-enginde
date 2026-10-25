package filter

import (
	"context"

	"github.com/zavisoft/search-engine/internal/pipeline"
)

// Stage is S3. Structured filters (request filters and always_filters) are already pushed
// down into S2; CEL filter and boost rules run here from M7, so for now it passes through.
type Stage struct{}

// New returns the S3 stage.
func New() *Stage { return &Stage{} }

// Name implements pipeline.Stage.
func (*Stage) Name() string { return pipeline.StageFilter }

// Run implements pipeline.Stage.
func (*Stage) Run(context.Context, *pipeline.State) (pipeline.Report, error) {
	return pipeline.Report{Provider: "pushdown"}, nil
}
