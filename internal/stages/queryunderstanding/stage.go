package queryunderstanding

import (
	"context"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Category prediction parameters.
const (
	CategoryK             = 3
	CategoryMinConfidence = 0.15
)

// Stage is S1.
type Stage struct {
	breakers *resilience.Breakers

	mu    sync.Mutex
	dicts map[dictKey]*Dictionary
}

type dictKey struct {
	tenant  tenant.ID
	version int
}

// New returns the S1 stage.
func New(b *resilience.Breakers) *Stage {
	return &Stage{breakers: b, dicts: map[dictKey]*Dictionary{}}
}

// Name implements pipeline.Stage.
func (s *Stage) Name() string { return pipeline.StageQueryUnderstanding }

func (s *Stage) dictionary(res *tenant.Resolved) *Dictionary {
	k := dictKey{res.Tenant.ID, res.ConfigVersion}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.dicts[k]
	if !ok {
		d = NewDictionary(res.Config.Query.Attributes)
		for old := range s.dicts {
			if old.tenant == k.tenant {
				delete(s.dicts, old)
			}
		}
		s.dicts[k] = d
	}
	return d
}

// Run fills Query.Text, Attributes, Filter (price phrases), the embeddings and Categories.
// Embedding failures leave the vector empty, which skips the vector retrievers; that is
// reported as a fallback, not an error.
func (s *Stage) Run(ctx context.Context, st *pipeline.State) (pipeline.Report, error) {
	q, p, res := st.Query, st.Providers, st.Resolved
	rep := pipeline.Report{Provider: p.Names["text_embedder"]}
	if q.Type != pipeline.QueryText && q.Raw == "" {
		return rep, nil
	}
	price, text := ExtractPrice(Normalize(q.Raw))
	if text == "" {
		text = Normalize(q.Raw)
	}
	q.Text = text
	q.Filter = filter.All(q.Filter, price.Filter())
	q.Attributes = s.dictionary(res).Extract(text)
	if q.Text == "" {
		return rep, nil
	}

	needTextToImage := false
	for _, r := range res.Config.Retrieval.Text.Retrievers {
		if r.Name == tenant.RetrieverVectorTextToImage {
			needTextToImage = true
		}
	}
	var mu sync.Mutex
	var reasons []string
	embed := func(e pipeline.Embedder, role string, dst *[]float32) func() error {
		return func() error {
			key := resilience.Key{Tenant: string(res.Tenant.ID), Stage: s.Name(), Provider: p.Names[role]}
			vecs, out, err := resilience.Call(ctx, s.breakers, key, func(ctx context.Context) ([][]float32, error) {
				return e.EmbedText(ctx, []string{q.Text})
			}, nil)
			if err != nil || len(vecs) != 1 {
				mu.Lock()
				reasons = append(reasons, role+":"+out.Reason)
				mu.Unlock()
				return nil
			}
			*dst = vecs[0]
			return nil
		}
	}
	var g errgroup.Group
	g.Go(embed(p.TextEmbedder, "text_embedder", &q.TextVector))
	if needTextToImage && p.ImageEmbedder != nil {
		g.Go(embed(p.ImageEmbedder, "image_embedder", &q.TextToImageVector))
	}
	_ = g.Wait()
	if len(reasons) > 0 {
		rep.Fallback, rep.FallbackReason = true, strings.Join(reasons, ",")
	}
	if p.Centroids != nil && len(q.TextVector) > 0 {
		q.Categories = p.Centroids.Nearest(q.TextVector, CategoryK, CategoryMinConfidence)
	}
	return rep, nil
}
