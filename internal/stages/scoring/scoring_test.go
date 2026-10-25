package scoring_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/stages/scoring"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
)

// byFeature scores candidates by Features["x"].
type byFeature struct{ fail bool }

func (s byFeature) Score(_ context.Context, _ *pipeline.Query, cs []pipeline.Candidate) ([]float32, error) {
	if s.fail {
		return nil, errors.New("scorer down")
	}
	out := make([]float32, len(cs))
	for i, c := range cs {
		out[i] = float32(c.Features["x"])
	}
	return out, nil
}

func (byFeature) FeatureSchema() pipeline.FeatureSchema {
	return pipeline.FeatureSchema{Names: []string{"x"}}
}

func state(xs ...float64) *pipeline.State {
	st := &pipeline.State{Resolved: stagestest.Resolved(), Query: &pipeline.Query{}}
	for i, x := range xs {
		st.Candidates = append(st.Candidates, pipeline.Candidate{
			ProductID: string(rune('a' + i)), Stage: pipeline.StageRetrieval, Score: float64(len(xs) - i),
			Features: map[string]float64{"x": x},
		})
	}
	return st
}

func ids(cs []pipeline.Candidate) string {
	var b []byte
	for _, c := range cs {
		b = append(b, c.ProductID...)
	}
	return string(b)
}

func TestApplyKeepsTopAndPreservesCutOrder(t *testing.T) {
	st := state(0.1, 0.9, 0.5, 0.7, 0.3)
	rep := scoring.Apply(context.Background(), stagestest.Breakers(), st, pipeline.StagePrerank, pipeline.StageRetrieval, byFeature{}, "x", 2)
	if rep.Fallback {
		t.Fatal(rep)
	}
	if got := ids(st.Candidates); got != "bdace" {
		t.Errorf("order = %s, want bd (scored) then ace (previous order)", got)
	}
	stages := []string{}
	for _, c := range st.Candidates {
		stages = append(stages, c.Stage)
	}
	if !slices.Equal(stages[:2], []string{"prerank", "prerank"}) || stages[2] != "retrieval" {
		t.Errorf("stages = %v", stages)
	}

	rep = scoring.Apply(context.Background(), stagestest.Breakers(), st, pipeline.StageRank, pipeline.StagePrerank, byFeature{}, "x", 1)
	if got := ids(st.Candidates); got != "bdace" || st.Candidates[0].Stage != "rank" || st.Candidates[1].Stage != "prerank" {
		t.Errorf("rank pass = %s (%s, %s)", got, st.Candidates[0].Stage, st.Candidates[1].Stage)
	}
	if rep.Fallback {
		t.Fatal(rep)
	}
}

func TestApplyPassesThroughOnScorerFailure(t *testing.T) {
	st := state(0.1, 0.9)
	rep := scoring.Apply(context.Background(), stagestest.Breakers(), st, pipeline.StagePrerank, pipeline.StageRetrieval, byFeature{fail: true}, "x", 1)
	if !rep.Fallback || rep.FallbackReason == "" || ids(st.Candidates) != "ab" {
		t.Errorf("rep=%+v order=%s", rep, ids(st.Candidates))
	}
}
