package registry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	qdrantgo "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"

	"github.com/zavisoft/search-engine/internal/catalog/centroids"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
	"github.com/zavisoft/search-engine/internal/providers/lexical/opensearch"
	"github.com/zavisoft/search-engine/internal/providers/scorer/defaultlinear"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Platform is the shared infrastructure behind platform-default providers.
type Platform struct {
	Triton     grpc.ClientConnInterface
	OpenSearch *opensearchapi.Client
	Qdrant     *qdrantgo.Client
	Centroids  *centroids.Store
}

// Registry builds and caches a tenant's providers per config version. Entries are also rebuilt
// after refresh, which picks up a new index version (and its centroids) after a re-index.
// BYO providers, factories keyed by (kind, type) and LRU eviction arrive in M4–M5.
type Registry struct {
	platform Platform
	refresh  time.Duration
	log      *slog.Logger

	mu      sync.Mutex
	entries map[tenant.ID]*entry
}

type entry struct {
	mu        sync.Mutex
	version   int
	providers *pipeline.Providers
	builtAt   time.Time
}

// New returns a registry over the platform infrastructure.
func New(p Platform, refresh time.Duration, log *slog.Logger) *Registry {
	return &Registry{platform: p, refresh: refresh, log: log, entries: map[tenant.ID]*entry{}}
}

// For returns the providers for res's tenant and config version.
func (r *Registry) For(ctx context.Context, res *tenant.Resolved) (*pipeline.Providers, error) {
	r.mu.Lock()
	e, ok := r.entries[res.Tenant.ID]
	if !ok {
		e = &entry{}
		r.entries[res.Tenant.ID] = e
	}
	r.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.providers != nil && e.version == res.ConfigVersion && time.Since(e.builtAt) < r.refresh {
		return e.providers, nil
	}
	p, err := r.build(ctx, res)
	if err != nil {
		if e.providers != nil && e.version == res.ConfigVersion {
			r.log.Warn("provider refresh failed; keeping previous providers", "tenant", res.Tenant.ID, "err", err)
			e.builtAt = time.Now()
			return e.providers, nil
		}
		return nil, err
	}
	e.version, e.providers, e.builtAt = res.ConfigVersion, p, time.Now()
	return p, nil
}

func (r *Registry) build(ctx context.Context, res *tenant.Resolved) (*pipeline.Providers, error) {
	cfg := res.Config
	names := map[string]string{}
	text, err := r.embedder(cfg.Embedders.Text, false)
	if err != nil {
		return nil, fmt.Errorf("text embedder: %w", err)
	}
	image, err := r.embedder(cfg.Embedders.Image, true)
	if err != nil {
		return nil, fmt.Errorf("image embedder: %w", err)
	}
	names["text_embedder"] = "platform:" + cfg.Embedders.Text.Model
	names["image_embedder"] = "platform:" + cfg.Embedders.Image.Model
	if cfg.Stores.Lexical.Provider != tenant.ProviderPlatform || cfg.Stores.Vector.Provider != tenant.ProviderPlatform {
		return nil, errors.New("only platform stores are supported until M5")
	}
	lex := opensearch.New(r.platform.OpenSearch, res.Tenant.ID, res.Registry)
	vec := qdrant.New(r.platform.Qdrant, res.Tenant.ID, qdrant.VectorDims{pipeline.VectorText: text.Dim(), pipeline.VectorImage: image.Dim()}, res.Registry)
	names["lexical"], names["vector"] = "platform:opensearch", "platform:qdrant"

	prerank, err := scorer(cfg.Prerank, defaultlinear.DefaultPrerankWeights())
	if err != nil {
		return nil, fmt.Errorf("prerank: %w", err)
	}
	rank, err := scorer(cfg.Rank, defaultlinear.DefaultRankWeights())
	if err != nil {
		return nil, fmt.Errorf("rank: %w", err)
	}
	names["prerank"], names["rank"] = cfg.Prerank.Scorer.Type, cfg.Rank.Scorer.Type

	p := &pipeline.Providers{TextEmbedder: text, ImageEmbedder: image, Lexical: lex, Vectors: vec, Prerank: prerank, Rank: rank, Names: names}
	p.IndexVersion, err = lex.Active(ctx)
	if err != nil {
		return nil, fmt.Errorf("active index version: %w", err)
	}
	if r.platform.Centroids != nil && p.IndexVersion > 0 {
		idx, err := r.platform.Centroids.Load(ctx, string(res.Tenant.ID), p.IndexVersion)
		switch {
		case err == nil:
			p.Centroids = idx
		case errors.Is(err, centroids.ErrNotFound):
			r.log.Info("no category centroids for index version; category prediction disabled", "tenant", res.Tenant.ID, "index_version", p.IndexVersion)
		default:
			r.log.Warn("loading category centroids failed", "tenant", res.Tenant.ID, "err", err)
		}
	}
	return p, nil
}

func (r *Registry) embedder(c tenant.EmbedderConfig, image bool) (*kservev2.Embedder, error) {
	if c.Provider != tenant.ProviderPlatform {
		return nil, errors.New("only platform embedders are supported until M5")
	}
	mc, ok := kservev2.PlatformModel(c.Model)
	if !ok {
		return nil, fmt.Errorf("unknown platform model %q", c.Model)
	}
	if image && mc.ImageModel == "" {
		return nil, fmt.Errorf("platform model %q has no image tower", c.Model)
	}
	return kservev2.New(r.platform.Triton, mc)
}

func scorer(c tenant.StageScorerConfig, defaults map[string]float64) (pipeline.Scorer, error) {
	if c.Scorer.Type != tenant.ScorerDefaultLinear {
		return nil, fmt.Errorf("scorer type %q arrives in M7", c.Scorer.Type)
	}
	w := c.Scorer.Weights
	if len(w) == 0 {
		w = defaults
	}
	return defaultlinear.New(w), nil
}

// PlatformEmbedderInfo describes platform models for config validation.
func PlatformEmbedderInfo(id string) (tenant.EmbedderInfo, bool) {
	mc, ok := kservev2.PlatformModel(id)
	if !ok {
		return tenant.EmbedderInfo{}, false
	}
	return tenant.EmbedderInfo{
		Dim: mc.Dim, Image: mc.ImageModel != "", TextEmbedder: mc.TextModel != "",
		TextToImage: mc.ImageModel != "" && mc.TextModel != "",
	}, true
}
