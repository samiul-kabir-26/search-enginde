package retrieval_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// byoVectors is a BYO vector store that fails or hangs until the context ends.
type byoVectors struct {
	pipeline.VectorStore
	hang bool
}

func (b byoVectors) Search(ctx context.Context, _ pipeline.VectorSearch) ([]pipeline.Hit, error) {
	if b.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, errors.New("connection refused")
}

func semanticOnly(res *tenant.Resolved) {
	res.Config.Retrieval.Text.Retrievers = []tenant.RetrieverConfig{
		{Name: tenant.RetrieverVectorText, K: 10, Weight: 1}, {Name: tenant.RetrieverVectorTextToImage, K: 10, Weight: 0.5},
	}
}

func textQuery(res *tenant.Resolved, withVectors bool) *pipeline.Query {
	q := &pipeline.Query{Type: pipeline.QueryText, Text: "red leather handbag", BaseFilter: res.AlwaysFilters}
	if withVectors {
		q.TextVector = stagestest.Embed(q.Text, stagestest.Dim)
		q.TextToImageVector = q.TextVector
	}
	return q
}

func TestBYOVectorFailureFallsBackToLexicalOnce(t *testing.T) {
	for _, hang := range []bool{false, true} {
		res := stagestest.Resolved()
		semanticOnly(res)
		p, lex, _ := stagestest.Providers()
		p.Vectors = byoVectors{VectorStore: p.Vectors, hang: hang}
		p.BYO = map[string]bool{"vector": true}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		start := time.Now()
		st := &pipeline.State{Resolved: res, Providers: p, Query: textQuery(res, true)}
		rep, err := retrieval.New(stagestest.Breakers()).Run(ctx, st)
		cancel()
		if err != nil || len(st.Candidates) == 0 {
			t.Fatalf("hang=%v: the lexical fallback must carry the request: %v (%d candidates)", hang, err, len(st.Candidates))
		}
		if elapsed := time.Since(start); elapsed > 80*time.Millisecond {
			t.Errorf("hang=%v: stage took %v, over its 60 ms budget", hang, elapsed)
		}
		if !rep.Fallback || !strings.Contains(rep.FallbackReason, "vector_text:") || !strings.Contains(rep.FallbackReason, ">lexical_bm25") {
			t.Errorf("hang=%v: report = %+v", hang, rep)
		}
		if hang && !strings.Contains(rep.FallbackReason, "timeout") {
			t.Errorf("a hanging store is a timeout: %s", rep.FallbackReason)
		}
		for _, c := range st.Candidates {
			if _, ok := c.Retrievers[tenant.RetrieverLexicalBM25]; !ok || len(c.Retrievers) != 1 {
				t.Errorf("fallback hits are lexical (fused once): %+v", c.Retrievers)
			}
		}
		if lex.Last.Text != "red leather handbag" || lex.Last.Base.IsZero() {
			t.Errorf("the fallback is a filtered BM25 search: %+v", lex.Last)
		}
	}
}

func TestBYOEmbedderWithoutVectorFallsBack(t *testing.T) {
	res := stagestest.Resolved()
	semanticOnly(res)
	p, _, _ := stagestest.Providers()
	p.BYO = map[string]bool{"text_embedder": true}
	st := &pipeline.State{Resolved: res, Providers: p, Query: textQuery(res, false)}
	rep, err := retrieval.New(stagestest.Breakers()).Run(context.Background(), st)
	if err != nil || len(st.Candidates) == 0 || !strings.Contains(rep.FallbackReason, "vector_text:no_vector>lexical_bm25") {
		t.Fatalf("rep=%+v err=%v candidates=%d", rep, err, len(st.Candidates))
	}

	// A platform embedder without a vector simply skips the retriever, as before.
	p.BYO = nil
	st = &pipeline.State{Resolved: res, Providers: p, Query: textQuery(res, false)}
	if rep, _ := retrieval.New(stagestest.Breakers()).Run(context.Background(), st); rep.Fallback || len(st.Candidates) != 0 {
		t.Errorf("platform providers keep their behavior: %+v %d", rep, len(st.Candidates))
	}
}

func TestBYOFallbackNotNeededWhenLexicalRuns(t *testing.T) {
	res := stagestest.Resolved()
	p, _, _ := stagestest.Providers()
	p.Vectors = byoVectors{VectorStore: p.Vectors}
	p.BYO = map[string]bool{"vector": true}
	st := &pipeline.State{Resolved: res, Providers: p, Query: textQuery(res, true)}
	rep, err := retrieval.New(stagestest.Breakers()).Run(context.Background(), st)
	if err != nil || len(st.Candidates) == 0 {
		t.Fatalf("lexical_bm25 carries the request: %v", err)
	}
	if strings.Contains(rep.FallbackReason, ">lexical") || !strings.Contains(rep.FallbackReason, "vector_text:error") {
		t.Errorf("the failing retriever drops out without a duplicate lexical run: %s", rep.FallbackReason)
	}
}
