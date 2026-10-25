package pipeline

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zavisoft/search-engine/internal/tenant"
)

func TestFuseRRF(t *testing.T) {
	doc := &Doc{Title: "x"}
	got := FuseRRF([]RetrieverResult{
		{Name: "lexical_bm25", Weight: 1, Hits: []Hit{{ProductID: "a", Score: 9, Doc: doc}, {ProductID: "b", Score: 5}}},
		{Name: "vector_text", Weight: 0.5, Hits: []Hit{{ProductID: "b", Score: 0.9}, {ProductID: "c", Score: 0.8}, {ProductID: "b", Score: 0.1}}},
	}, 60)
	if len(got) != 3 {
		t.Fatalf("got %d candidates", len(got))
	}
	want := map[string]float64{"a": 1.0 / 61, "b": 1.0/62 + 0.5/61, "c": 0.5 / 62}
	for _, c := range got {
		if math.Abs(c.RRF-want[c.ProductID]) > 1e-12 || c.Score != c.RRF || c.Stage != StageRetrieval {
			t.Errorf("%s: rrf %v want %v", c.ProductID, c.RRF, want[c.ProductID])
		}
	}
	if got[0].ProductID != "b" || got[1].ProductID != "a" {
		t.Errorf("order = %s %s %s", got[0].ProductID, got[1].ProductID, got[2].ProductID)
	}
	if got[0].Retrievers["vector_text"].Rank != 1 {
		t.Error("a duplicate hit in one list must keep its first rank")
	}
	if got[1].Doc != doc {
		t.Error("the first doc for a product must be kept")
	}
	tie := FuseRRF([]RetrieverResult{{Name: "x", Weight: 1, Hits: []Hit{{ProductID: "z"}}}, {Name: "y", Weight: 1, Hits: []Hit{{ProductID: "a"}}}}, 0)
	if tie[0].ProductID != "a" {
		t.Error("ties must break by product id")
	}
}

type sleepStage struct {
	name     string
	sleep    time.Duration
	err      error
	deadline time.Duration
}

func (s *sleepStage) Name() string { return s.name }

func (s *sleepStage) Run(ctx context.Context, st *State) (Report, error) {
	if dl, ok := ctx.Deadline(); ok {
		s.deadline = time.Until(dl)
	}
	select {
	case <-time.After(s.sleep):
	case <-ctx.Done():
	}
	st.Candidates = append(st.Candidates, Candidate{ProductID: s.name, Features: map[string]float64{"f": 1}})
	return Report{Provider: "p-" + s.name}, s.err
}

func TestEngineBudgetsAndTrace(t *testing.T) {
	s1 := &sleepStage{name: StageQueryUnderstanding}
	s2 := &sleepStage{name: StageRetrieval, sleep: 30 * time.Millisecond}
	s3 := &sleepStage{name: StageFilter, err: errors.New("boom")}
	e, err := NewEngine(noop.NewMeterProvider().Meter("t"), s1, s2, s3)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tenant.Config{Budgets: tenant.Budgets{TotalMS: 40, S1MS: 500, S2MS: 1000, S3MS: 1000}}
	res := &tenant.Resolved{Tenant: tenant.Tenant{ID: "T"}, Config: cfg}
	st, tr := e.Run(context.Background(), res, &Providers{}, &Query{Explain: true})

	if s1.deadline > 41*time.Millisecond {
		t.Errorf("stage deadline %v must be min(stage budget, remaining total)", s1.deadline)
	}
	if s3.deadline > 15*time.Millisecond {
		t.Errorf("the last stage gets only what remains of the total budget, got %v", s3.deadline)
	}
	if !st.Degraded || tr.Stages[2].Error != "boom" {
		t.Errorf("a stage error must mark the response degraded: %+v", tr.Stages[2])
	}
	if len(tr.Stages) != 3 || tr.Stages[1].Provider != "p-retrieval" || tr.Stages[1].InCount != 1 || tr.Stages[1].OutCount != 2 {
		t.Errorf("trace = %+v", tr.Stages)
	}
	if c := tr.Stages[0].Candidates; len(c) != 1 || c[0].Scores["f"] != 1 {
		t.Errorf("explain must record candidate scores: %+v", c)
	}
}
