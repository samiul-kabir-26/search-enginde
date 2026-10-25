package retrieval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Stage is S2.
type Stage struct {
	breakers *resilience.Breakers
}

// New returns the S2 stage.
func New(b *resilience.Breakers) *Stage { return &Stage{breakers: b} }

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return pipeline.StageRetrieval }

// ErrAllFailed means no retriever produced a result; the response is empty and degraded.
var ErrAllFailed = errors.New("all retrievers failed")

type outcome struct {
	name   string
	weight float64
	hits   []pipeline.Hit
	err    error
	reason string
}

// Run fans out to the configured retrievers and replaces the candidates with the fused list.
func (s *Stage) Run(ctx context.Context, st *pipeline.State) (pipeline.Report, error) {
	q, res := st.Query, st.Resolved
	rep := pipeline.Report{Provider: "rrf"}
	cfgs := Retrievers(res.Config, q)

	var (
		mu       sync.Mutex
		outcomes []outcome
		lexical  *pipeline.LexicalResult
	)
	g, gctx := errgroup.WithContext(ctx)
	ran := 0
	for _, rc := range cfgs {
		run := s.retriever(rc, st, func(r pipeline.LexicalResult) { mu.Lock(); lexical = &r; mu.Unlock() })
		if run == nil {
			continue
		}
		ran++
		g.Go(func() error {
			key := resilience.Key{Tenant: string(res.Tenant.ID), Stage: s.Name(), Provider: rc.Name}
			hits, out, err := resilience.Call(gctx, s.breakers, key, run, nil)
			mu.Lock()
			outcomes = append(outcomes, outcome{name: rc.Name, weight: rc.Weight, hits: hits, err: err, reason: out.Reason})
			mu.Unlock()
			return nil
		})
	}
	needFacets := len(q.Facets) > 0 && q.Text != "" && !hasRetriever(cfgs, tenant.RetrieverLexicalBM25)
	if needFacets {
		g.Go(func() error {
			r, err := st.Providers.Lexical.Search(gctx, pipeline.LexicalSearch{Base: q.BaseFilter, Request: q.Filter, Facets: q.Facets})
			if err == nil {
				mu.Lock()
				lexical = &r
				mu.Unlock()
			}
			return nil
		})
	}
	_ = g.Wait()

	if lexical != nil {
		st.Facets, st.TotalMatches = lexical.Facets, lexical.TotalMatches
	}
	sort.Slice(outcomes, func(i, j int) bool { return order(cfgs, outcomes[i].name) < order(cfgs, outcomes[j].name) })
	var results []pipeline.RetrieverResult
	var failed []string
	for _, o := range outcomes {
		if o.err != nil {
			failed = append(failed, o.name+":"+o.reason)
			continue
		}
		results = append(results, pipeline.RetrieverResult{Name: o.name, Weight: o.weight, Hits: o.hits})
	}
	if len(failed) > 0 {
		rep.Fallback, rep.FallbackReason = true, strings.Join(failed, ",")
	}
	if ran > 0 && len(results) == 0 {
		st.Candidates = nil
		return rep, fmt.Errorf("%w (%s)", ErrAllFailed, rep.FallbackReason)
	}
	st.Candidates = pipeline.FuseRRF(results, res.Config.Retrieval.RRFK)
	return rep, nil
}

// similarK is the per-retriever depth of similar-item queries when the config does not list
// vector_image or vector_text.
const similarK = 200

// Retrievers returns the retrievers for the query type (docs/ARCHITECTURE.md §6 S2): the text
// set; the image set plus the text set when an image query carries refinement text; or
// vector_image and vector_text on the anchor's stored vectors for similar items.
func Retrievers(cfg *tenant.Config, q *pipeline.Query) []tenant.RetrieverConfig {
	text, image := cfg.Retrieval.Text.Retrievers, cfg.Retrieval.Image.Retrievers
	switch q.Type {
	case pipeline.QueryImage:
		out := append([]tenant.RetrieverConfig(nil), image...)
		if q.Text != "" {
			for _, r := range text {
				if !hasRetriever(out, r.Name) {
					out = append(out, r)
				}
			}
		}
		return out
	case pipeline.QuerySimilar:
		pick := func(list []tenant.RetrieverConfig, name string) tenant.RetrieverConfig {
			for _, r := range list {
				if r.Name == name {
					return tenant.RetrieverConfig{Name: name, K: r.K, Weight: 1}
				}
			}
			return tenant.RetrieverConfig{Name: name, K: similarK, Weight: 1}
		}
		return []tenant.RetrieverConfig{pick(image, tenant.RetrieverVectorImage), pick(text, tenant.RetrieverVectorText)}
	}
	return text
}

func hasRetriever(cfgs []tenant.RetrieverConfig, name string) bool {
	for _, c := range cfgs {
		if c.Name == name {
			return true
		}
	}
	return false
}

func order(cfgs []tenant.RetrieverConfig, name string) int {
	for i, c := range cfgs {
		if c.Name == name {
			return i
		}
	}
	return len(cfgs)
}

// retriever returns the call for one configured retriever, or nil when the query lacks what
// it needs (no text vector, no predicted category).
func (s *Stage) retriever(rc tenant.RetrieverConfig, st *pipeline.State, onLexical func(pipeline.LexicalResult)) func(context.Context) ([]pipeline.Hit, error) {
	q, p, cfg := st.Query, st.Providers, st.Resolved.Config
	all := filter.All(q.BaseFilter, q.Filter)
	vector := func(name string, vec []float32) func(context.Context) ([]pipeline.Hit, error) {
		if len(vec) == 0 {
			return nil
		}
		return func(ctx context.Context) ([]pipeline.Hit, error) {
			return p.Vectors.Search(ctx, pipeline.VectorSearch{Vector: name, Query: vec, Filter: all, K: rc.K, EF: cfg.EffectiveEF()})
		}
	}
	switch rc.Name {
	case tenant.RetrieverLexicalBM25:
		if q.Text == "" && q.Type != pipeline.QueryText {
			return nil
		}
		return func(ctx context.Context) ([]pipeline.Hit, error) {
			r, err := p.Lexical.Search(ctx, pipeline.LexicalSearch{
				Text: q.Text, Boosts: cfg.Retrieval.FieldBoosts, Base: q.BaseFilter, Request: q.Filter, K: rc.K, Facets: q.Facets,
			})
			if err != nil {
				return nil, err
			}
			onLexical(r)
			return r.Hits, nil
		}
	case tenant.RetrieverVectorText:
		return vector(pipeline.VectorText, q.TextVector)
	case tenant.RetrieverVectorTextToImage:
		return vector(pipeline.VectorImage, q.TextToImageVector)
	case tenant.RetrieverVectorImage:
		return vector(pipeline.VectorImage, q.ImageVector)
	case tenant.RetrieverCategoryPopular:
		if len(q.Categories) == 0 {
			return nil
		}
		ids := make([]any, len(q.Categories))
		for i, c := range q.Categories {
			ids[i] = c.ID
		}
		cat := filter.Expr{Cond: &filter.Condition{Field: "category_ids", Op: filter.In, Value: ids, Type: catalog.TypeKeywordArray}}
		return func(ctx context.Context) ([]pipeline.Hit, error) {
			r, err := p.Lexical.Search(ctx, pipeline.LexicalSearch{Base: filter.All(q.BaseFilter, cat), Request: q.Filter, K: rc.K, Popular: true})
			if err != nil {
				return nil, err
			}
			return r.Hits, nil
		}
	}
	return nil
}
