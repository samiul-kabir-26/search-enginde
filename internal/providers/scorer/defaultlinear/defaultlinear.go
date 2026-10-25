package defaultlinear

import (
	"context"
	"maps"
	"slices"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
)

// DefaultPrerankWeights apply when the prerank scorer declares no weights: cheap retrieval and
// query signals only.
func DefaultPrerankWeights() map[string]float64 {
	return map[string]float64{
		feature.RRFScore: 1.0, feature.CosineText: 0.3, feature.BM25Norm: 0.2, feature.QueryCategoryMatch: 0.2,
	}
}

// DefaultRankWeights match the default tenant config (docs/TENANT_CONFIG.md §3).
func DefaultRankWeights() map[string]float64 {
	return map[string]float64{
		feature.RRFScore: 1.0, feature.BM25Norm: 0.4, feature.CosineText: 0.6, feature.CosineImage: 0.6,
		feature.CTR7d: 0.3, feature.CVR7d: 0.5, feature.Rating: 0.2, feature.VendorScore: 0.3, feature.PolicyBoost: 1.0,
	}
}

// Scorer is a weighted sum of catalog features; missing features take their declared default.
type Scorer struct {
	weights map[string]float64
	schema  pipeline.FeatureSchema
}

var _ pipeline.Scorer = (*Scorer)(nil)

// New returns a scorer for weights (validated against the feature catalog at publish).
func New(weights map[string]float64) *Scorer {
	names := slices.Sorted(maps.Keys(weights))
	defaults := feature.Defaults()
	d := make(map[string]float64, len(names))
	for _, n := range names {
		d[n] = defaults[n]
	}
	return &Scorer{weights: maps.Clone(weights), schema: pipeline.FeatureSchema{Names: names, Defaults: d}}
}

// FeatureSchema lists the weighted features in name order.
func (s *Scorer) FeatureSchema() pipeline.FeatureSchema { return s.schema }

// Score returns Σ weight·feature for each candidate.
func (s *Scorer) Score(_ context.Context, _ *pipeline.Query, cs []pipeline.Candidate) ([]float32, error) {
	out := make([]float32, len(cs))
	for i, c := range cs {
		var sum float64
		for _, n := range s.schema.Names {
			v, ok := c.Features[n]
			if !ok {
				v = s.schema.Defaults[n]
			}
			sum += s.weights[n] * v
		}
		out[i] = float32(sum)
	}
	return out, nil
}
