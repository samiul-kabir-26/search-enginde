package stages_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/stages/filter"
	"github.com/zavisoft/search-engine/internal/stages/prerank"
	"github.com/zavisoft/search-engine/internal/stages/queryunderstanding"
	"github.com/zavisoft/search-engine/internal/stages/rank"
	"github.com/zavisoft/search-engine/internal/stages/rerank"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
)

var update = flag.Bool("update", false, "rewrite golden files")

func engine(t *testing.T) *pipeline.Engine {
	t.Helper()
	b := stagestest.Breakers()
	e, err := pipeline.NewEngine(noop.NewMeterProvider().Meter("test"),
		queryunderstanding.New(b), retrieval.New(b), filter.New(), prerank.New(b, nil), rank.New(b), rerank.New())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

type golden struct {
	Query      any                              `json:"query"`
	Results    []string                         `json:"results"`
	Facets     map[string][]pipeline.FacetValue `json:"facets"`
	Degraded   bool                             `json:"degraded"`
	Trace      *pipeline.Trace                  `json:"trace"`
	TotalMatch int64                            `json:"total_matches"`
}

// TestExplainTraceGolden runs the real stages over the fake catalog and compares the full
// explain trace (latencies zeroed) with testdata/explain_trace.golden.json. Run with -update
// after an intended ranking change and review the diff.
func TestExplainTraceGolden(t *testing.T) {
	res := stagestest.Resolved()
	p, _, _ := stagestest.Providers()
	q := &pipeline.Query{
		Tenant: res.Tenant.ID, ConfigVersion: res.ConfigVersion, Type: pipeline.QueryText,
		Raw: "Red leather handbag under $50", BaseFilter: res.AlwaysFilters, Depth: 1000, Explain: true,
		Facets: []pipeline.FacetRequest{{Field: "brand", Size: 10}},
	}
	st, tr := engine(t).Run(context.Background(), res, p, q)
	for i := range tr.Stages {
		tr.Stages[i].LatencyMS = 0
	}
	got := golden{
		Query:  map[string]any{"text": q.Text, "attributes": q.Attributes, "categories": q.Categories, "filter": q.Filter},
		Facets: st.Facets, Degraded: st.Degraded, Trace: tr, TotalMatch: st.TotalMatches,
	}
	for _, c := range st.Candidates {
		got.Results = append(got.Results, c.ProductID)
	}
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	path := filepath.Join("testdata", "explain_trace.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/stages -run Golden -update)", err)
	}
	if !bytes.Equal(b, want) {
		t.Errorf("explain trace differs from %s; rerun with -update if intended.\n got:\n%s", path, b)
	}
}

// TestPipelineInvariants checks pushdown and ranking properties independent of exact scores.
func TestPipelineInvariants(t *testing.T) {
	res := stagestest.Resolved()
	p, lex, _ := stagestest.Providers()
	q := &pipeline.Query{Tenant: res.Tenant.ID, Type: pipeline.QueryText, Raw: "red leather handbag under $50",
		BaseFilter: res.AlwaysFilters, Depth: 3}
	st, tr := engine(t).Run(context.Background(), res, p, q)
	if st.Degraded || len(tr.Stages) != 6 {
		t.Fatalf("degraded=%v stages=%d", st.Degraded, len(tr.Stages))
	}
	if len(st.Candidates) != 3 {
		t.Errorf("depth 3 must cut the list to 3, got %d", len(st.Candidates))
	}
	for _, c := range st.Candidates {
		for _, prod := range stagestest.Catalog() {
			if prod.ID == c.ProductID && (prod.Status != "active" || prod.Stock <= 0 || prod.PriceMinor > 5000) {
				t.Errorf("%s violates always_filters or the price phrase", c.ProductID)
			}
		}
	}
	if lex.Last.Request.IsZero() || lex.Last.Base.IsZero() {
		t.Error("lexical search must receive both base and request filters")
	}
	if tr.Stages[0].Candidates != nil {
		t.Error("candidate scores are only traced with explain")
	}
}

func TestDegradedWhenAllRetrieversFail(t *testing.T) {
	res := stagestest.Resolved()
	p, lex, vec := stagestest.Providers()
	lex.Fail, vec.Fail = true, true
	q := &pipeline.Query{Tenant: res.Tenant.ID, Type: pipeline.QueryText, Raw: "red bag", BaseFilter: res.AlwaysFilters, Depth: 10}
	st, tr := engine(t).Run(context.Background(), res, p, q)
	if !st.Degraded || len(st.Candidates) != 0 {
		t.Fatalf("want degraded empty result, got degraded=%v n=%d", st.Degraded, len(st.Candidates))
	}
	if tr.Stages[1].Error == "" || !tr.Stages[1].Fallback {
		t.Errorf("retrieval trace must record the failure: %+v", tr.Stages[1])
	}
}

func TestPartialRetrieverFailureIsTolerated(t *testing.T) {
	res := stagestest.Resolved()
	p, _, vec := stagestest.Providers()
	vec.Fail = true
	q := &pipeline.Query{Tenant: res.Tenant.ID, Type: pipeline.QueryText, Raw: "red bag", BaseFilter: res.AlwaysFilters, Depth: 10}
	st, tr := engine(t).Run(context.Background(), res, p, q)
	if st.Degraded || len(st.Candidates) == 0 {
		t.Fatalf("lexical results must survive a vector outage: degraded=%v n=%d", st.Degraded, len(st.Candidates))
	}
	if !tr.Stages[1].Fallback {
		t.Error("the failed retrievers must be reported as fallbacks")
	}
}

func TestEmbedderOutageFallsBackToLexical(t *testing.T) {
	res := stagestest.Resolved()
	p, _, _ := stagestest.Providers()
	p.TextEmbedder.(*stagestest.Embedder).Fail = true
	p.ImageEmbedder.(*stagestest.Embedder).Fail = true
	q := &pipeline.Query{Tenant: res.Tenant.ID, Type: pipeline.QueryText, Raw: "red bag", BaseFilter: res.AlwaysFilters, Depth: 10}
	st, tr := engine(t).Run(context.Background(), res, p, q)
	if st.Degraded || len(st.Candidates) == 0 || !tr.Stages[0].Fallback {
		t.Fatalf("degraded=%v n=%d s1=%+v", st.Degraded, len(st.Candidates), tr.Stages[0])
	}
	for _, c := range st.Candidates {
		if _, ok := c.Retrievers["lexical_bm25"]; !ok {
			t.Errorf("%s came from a vector retriever without a query vector", c.ProductID)
		}
	}
}
