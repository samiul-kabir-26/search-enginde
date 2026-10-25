package retrieval_test

import (
	"context"
	"testing"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
)

func run(t *testing.T, res *tenant.Resolved, p *pipeline.Providers, q *pipeline.Query) (*pipeline.State, pipeline.Report, error) {
	t.Helper()
	st := &pipeline.State{Resolved: res, Providers: p, Query: q}
	rep, err := retrieval.New(stagestest.Breakers()).Run(context.Background(), st)
	return st, rep, err
}

func TestCategoryPopularNeedsAPrediction(t *testing.T) {
	res := stagestest.Resolved()
	res.Config.Retrieval.Text.Retrievers = []tenant.RetrieverConfig{{Name: tenant.RetrieverCategoryPopular, K: 3, Weight: 1}}
	p, lex, _ := stagestest.Providers()

	st, _, err := run(t, res, p, &pipeline.Query{Type: pipeline.QueryText, Text: "bag", BaseFilter: res.AlwaysFilters})
	if err != nil || len(st.Candidates) != 0 {
		t.Fatalf("no category guess: want no candidates and no error, got %d, %v", len(st.Candidates), err)
	}

	q := &pipeline.Query{Type: pipeline.QueryText, Text: "bag", BaseFilter: res.AlwaysFilters,
		Categories: []pipeline.CategoryGuess{{ID: "handbags", Confidence: 0.9}}}
	st, _, err = run(t, res, p, q)
	if err != nil || len(st.Candidates) == 0 {
		t.Fatalf("popular in handbags: %d, %v", len(st.Candidates), err)
	}
	if !lex.Last.Popular || lex.Last.Base.IsZero() {
		t.Errorf("popular query must be a filtered popularity search: %+v", lex.Last)
	}
	for _, c := range st.Candidates {
		if c.ProductID != "p01" && c.ProductID != "p02" {
			t.Errorf("%s is not an active, in-stock handbag", c.ProductID)
		}
	}
}

func TestImageQueryUsesImageVectorAndFacetsOnly(t *testing.T) {
	res := stagestest.Resolved()
	p, lex, _ := stagestest.Providers()
	q := &pipeline.Query{
		Type: pipeline.QueryImage, ImageVector: stagestest.Embed("Red leather handbag photo", stagestest.Dim),
		BaseFilter: res.AlwaysFilters, Facets: []pipeline.FacetRequest{{Field: "brand", Size: 5}},
	}
	st, rep, err := run(t, res, p, q)
	if err != nil || rep.Fallback {
		t.Fatalf("rep=%+v err=%v", rep, err)
	}
	if len(st.Candidates) == 0 || st.Candidates[0].ProductID != "p01" {
		t.Fatalf("image query top = %+v", st.Candidates)
	}
	if _, ok := st.Candidates[0].Retrievers[tenant.RetrieverVectorImage]; !ok {
		t.Error("image queries must use vector_image")
	}
	if st.Facets["brand"] == nil || lex.Last.K != 0 {
		t.Errorf("without a lexical retriever, facets come from a size-0 lexical query: facets=%v k=%d", st.Facets, lex.Last.K)
	}
}
