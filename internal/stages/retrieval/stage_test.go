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
	if len(st.Facets) != 0 || len(lex.Last.Facets) != 0 {
		t.Errorf("an image query without text has no lexical match set to facet: %v", st.Facets)
	}
}

func TestFacetsOnlyQueryWithoutLexicalRetriever(t *testing.T) {
	res := stagestest.Resolved()
	res.Config.Retrieval.Text.Retrievers = []tenant.RetrieverConfig{{Name: tenant.RetrieverVectorText, K: 10, Weight: 1}}
	p, lex, _ := stagestest.Providers()
	q := &pipeline.Query{Type: pipeline.QueryText, Text: "red bag", TextVector: stagestest.Embed("red bag", stagestest.Dim),
		BaseFilter: res.AlwaysFilters, Facets: []pipeline.FacetRequest{{Field: "brand", Size: 5}}}
	st, _, err := run(t, res, p, q)
	if err != nil || st.Facets["brand"] == nil || lex.Last.K != 0 {
		t.Errorf("facets must come from a size-0 lexical query: facets=%v k=%d err=%v", st.Facets, lex.Last.K, err)
	}
}

func TestRetrieverSelection(t *testing.T) {
	cfg := stagestest.Resolved().Config
	names := func(rs []tenant.RetrieverConfig) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Name)
		}
		return out
	}
	img := names(retrieval.Retrievers(cfg, &pipeline.Query{Type: pipeline.QueryImage}))
	if len(img) != 1 || img[0] != tenant.RetrieverVectorImage {
		t.Errorf("image without text = %v", img)
	}
	both := names(retrieval.Retrievers(cfg, &pipeline.Query{Type: pipeline.QueryImage, Text: "red"}))
	if len(both) != 4 || both[0] != tenant.RetrieverVectorImage {
		t.Errorf("image with text = %v, want vector_image plus the three text retrievers", both)
	}
	sim := retrieval.Retrievers(cfg, &pipeline.Query{Type: pipeline.QuerySimilar})
	if len(sim) != 2 || sim[0].Name != tenant.RetrieverVectorImage || sim[1].Name != tenant.RetrieverVectorText || sim[0].Weight != 1 || sim[1].K != 500 {
		t.Errorf("similar = %+v", sim)
	}
}
