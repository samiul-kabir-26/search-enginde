package queryunderstanding

import (
	"context"
	"math"
	"sort"
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
	q, p := st.Query, st.Providers
	rep := pipeline.Report{Provider: p.Names["text_embedder"]}
	var reasons []string
	if q.Type == pipeline.QueryImage {
		rep.Provider = p.Names["image_embedder"]
		if r := s.image(ctx, st); r != "" {
			reasons = append(reasons, r)
		}
	}
	if q.Raw != "" && q.Type != pipeline.QuerySimilar {
		reasons = append(reasons, s.text(ctx, st, len(q.Categories) == 0)...)
	}
	if len(reasons) > 0 {
		rep.Fallback, rep.FallbackReason = true, strings.Join(reasons, ",")
	}
	return rep, nil
}

// ImageNeighbors is how many nearest products vote on an image query's categories.
const ImageNeighbors = 50

// image embeds the upload and predicts categories by a similarity-weighted majority vote over
// the leaf categories of the nearest image neighbours. It returns a fallback reason, or "".
func (s *Stage) image(ctx context.Context, st *pipeline.State) string {
	q, p, res := st.Query, st.Providers, st.Resolved
	if len(q.ImageBytes) == 0 || p.ImageEmbedder == nil {
		return ""
	}
	key := resilience.Key{Tenant: string(res.Tenant.ID), Stage: s.Name(), Provider: p.Names["image_embedder"]}
	vecs, out, err := resilience.Call(ctx, s.breakers, key, func(ctx context.Context) ([][]float32, error) {
		return p.ImageEmbedder.EmbedImage(ctx, [][]byte{q.ImageBytes})
	}, nil)
	if err != nil || len(vecs) != 1 {
		return "image_embedder:" + out.Reason
	}
	q.ImageVector = vecs[0]
	hits, err := p.Vectors.Search(ctx, pipeline.VectorSearch{
		Vector: pipeline.VectorImage, Query: q.ImageVector, Filter: q.BaseFilter, K: ImageNeighbors, EF: res.Config.EffectiveEF(),
	})
	if err != nil {
		return "category_vote:" + resilience.ReasonError
	}
	q.Categories = VoteCategories(hits, CategoryK, CategoryMinConfidence)
	return ""
}

// VoteCategories weights each neighbour's category by its similarity and returns up to k
// categories whose share of the vote is at least minShare. It votes on leaf categories first and
// moves one level up the category paths while no category reaches minShare, so a fragmented
// catalog still yields the most specific category the neighbours agree on.
func VoteCategories(hits []pipeline.Hit, k int, minShare float64) []pipeline.CategoryGuess {
	maxDepth := 0
	for _, h := range hits {
		if h.Doc != nil {
			maxDepth = max(maxDepth, len(h.Doc.CategoryIDs))
		}
	}
	for up := 0; up < maxDepth; up++ {
		votes := map[string]float64{}
		var total float64
		for _, h := range hits {
			if h.Doc == nil || h.Score <= 0 {
				continue
			}
			path := h.Doc.CategoryIDs
			if len(path) == 0 {
				continue
			}
			votes[path[max(len(path)-1-up, 0)]] += h.Score
			total += h.Score
		}
		var out []pipeline.CategoryGuess
		for id, v := range votes {
			if share := v / total; share >= minShare {
				out = append(out, pipeline.CategoryGuess{ID: id, Confidence: math.Round(share*1e4) / 1e4})
			}
		}
		if len(out) == 0 {
			continue
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].Confidence != out[j].Confidence {
				return out[i].Confidence > out[j].Confidence
			}
			return out[i].ID < out[j].ID
		})
		return out[:min(k, len(out))]
	}
	return nil
}

// text normalizes the query text, parses price phrases, extracts attributes and embeds it;
// with predict set it also predicts categories from the centroids. It returns fallback reasons.
func (s *Stage) text(ctx context.Context, st *pipeline.State, predict bool) []string {
	q, p, res := st.Query, st.Providers, st.Resolved
	price, text := ExtractPrice(Normalize(q.Raw))
	if text == "" {
		text = Normalize(q.Raw)
	}
	q.Text = text
	q.Filter = filter.All(q.Filter, price.Filter())
	q.Attributes = s.dictionary(res).Extract(text)
	if q.Text == "" {
		return nil
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
	if predict && p.Centroids != nil && len(q.TextVector) > 0 {
		q.Categories = p.Centroids.Nearest(q.TextVector, CategoryK, CategoryMinConfidence)
	}
	return reasons
}
