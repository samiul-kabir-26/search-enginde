package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
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

// Deps is what factories may use: the platform clients, and for BYO providers the egress
// client, secret store and bulkheads.
type Deps struct {
	Platform Platform
	BYO      *BYO
	Log      *slog.Logger
}

// Provider kinds.
const (
	KindEmbedder    = "embedder"
	KindLexical     = "lexical"
	KindVectorStore = "vector_store"
	KindScorer      = "scorer"
	KindCentroids   = "centroids"
)

// Spec is what a factory builds from: the role's config block and the tenant version.
type Spec struct {
	Role     string
	Resolved *tenant.Resolved
	// Block is the role's config block (an EmbedderConfig, StoreConfig or StageScorerConfig).
	Block any
	// Text, for embedders, is the dimension of the text embedder (stores size their vectors).
	Dims map[string]int
}

// Factory builds one provider. Values that implement io.Closer are closed when retired.
type Factory func(ctx context.Context, deps Deps, spec Spec) (any, error)

type factoryKey struct{ kind, typ string }

type cacheKey struct {
	tenant  tenant.ID
	version int
	draft   bool
	role    string
}

type entry struct {
	value   any
	builtAt time.Time
	// expires, when set, rebuilds the entry sooner than its role's refresh (a BYO provider
	// that failed to build).
	expires time.Time
}

// Registry builds providers through factories keyed by (kind, type) and caches them per
// (tenant, config version, role) in an LRU. Providers of a superseded version, or evicted
// ones, are closed after a drain grace so in-flight requests finish first.
type Registry struct {
	deps       Deps
	factories  map[factoryKey]Factory
	cache      *lru.Cache[cacheKey, *entry]
	drainGrace time.Duration
	refresh    time.Duration
	byoRefresh time.Duration

	mu      sync.Mutex
	pending []retiring
	builds  map[cacheKey]*sync.Mutex
}

type retiring struct {
	value any
	at    time.Time
}

// Options tune the registry.
type Options struct {
	// Size bounds cached providers (roles × tenants × versions).
	Size int
	// DrainGrace delays closing retired providers.
	DrainGrace time.Duration
	// Refresh rebuilds time-sensitive entries (the index version and its centroids).
	Refresh time.Duration
	// BYO enables BYO embedders and vector stores.
	BYO *BYO
	// BYORefresh rebuilds BYO providers so connection and secret changes are picked up
	// (default 5 min).
	BYORefresh time.Duration
}

// New returns a registry with the platform factories registered.
func New(p Platform, opts Options, log *slog.Logger) *Registry {
	if opts.Size <= 0 {
		opts.Size = 1024
	}
	if opts.BYORefresh <= 0 {
		opts.BYORefresh = 5 * time.Minute
	}
	r := &Registry{deps: Deps{Platform: p, BYO: opts.BYO, Log: log}, factories: map[factoryKey]Factory{},
		drainGrace: opts.DrainGrace, refresh: opts.Refresh, byoRefresh: opts.BYORefresh, builds: map[cacheKey]*sync.Mutex{}}
	r.cache, _ = lru.NewWithEvict[cacheKey, *entry](opts.Size, func(_ cacheKey, e *entry) { r.retire(e.value) })
	r.Register(KindEmbedder, tenant.ProviderPlatform, platformEmbedder)
	r.Register(KindLexical, tenant.ProviderPlatform, platformLexical)
	r.Register(KindVectorStore, tenant.ProviderPlatform, platformVectors)
	r.Register(KindScorer, tenant.ScorerDefaultLinear, defaultLinear)
	r.Register(KindCentroids, tenant.ProviderPlatform, platformCentroids)
	if opts.BYO != nil {
		r.Register(KindEmbedder, tenant.ProviderBYO, byoEmbedder)
		r.Register(KindVectorStore, tenant.ProviderBYO, byoVectors)
	}
	return r
}

// Register adds or replaces a factory.
func (r *Registry) Register(kind, typ string, f Factory) { r.factories[factoryKey{kind, typ}] = f }

func (r *Registry) retire(v any) {
	if _, ok := v.(io.Closer); !ok {
		return
	}
	r.mu.Lock()
	r.pending = append(r.pending, retiring{value: v, at: time.Now().Add(r.drainGrace)})
	r.mu.Unlock()
}

// Supersede retires a tenant's cached providers for every version other than keep (and its
// drafts). The resolver calls it when a version is activated.
func (r *Registry) Supersede(id tenant.ID, keep ...int) {
	for _, k := range r.cache.Keys() {
		if k.tenant != id {
			continue
		}
		kept := false
		for _, v := range keep {
			kept = kept || (k.version == v && !k.draft)
		}
		if !kept {
			r.cache.Remove(k)
		}
	}
}

// Run closes retired providers once their drain grace has passed.
func (r *Registry) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			r.closeDue(time.Now().Add(24 * time.Hour))
			return
		case now := <-t.C:
			r.closeDue(now)
		}
	}
}

func (r *Registry) closeDue(now time.Time) {
	r.mu.Lock()
	var due []any
	keep := r.pending[:0]
	for _, p := range r.pending {
		if now.After(p.at) {
			due = append(due, p.value)
		} else {
			keep = append(keep, p)
		}
	}
	r.pending = keep
	r.mu.Unlock()
	for _, v := range due {
		if err := v.(io.Closer).Close(); err != nil {
			r.deps.Log.Warn("closing retired provider", "err", err)
		}
	}
}

func (r *Registry) get(ctx context.Context, res *tenant.Resolved, role, kind, typ string, block any, dims map[string]int, ttl time.Duration) (any, error) {
	k := cacheKey{tenant: res.Tenant.ID, version: res.ConfigVersion, draft: res.Draft, role: role}
	fresh := func(e *entry) bool {
		return (ttl <= 0 || time.Since(e.builtAt) < ttl) && (e.expires.IsZero() || time.Now().Before(e.expires))
	}
	if e, ok := r.cache.Get(k); ok && fresh(e) {
		return e.value, nil
	}
	r.mu.Lock()
	l, ok := r.builds[k]
	if !ok {
		l = &sync.Mutex{}
		r.builds[k] = l
	}
	r.mu.Unlock()
	l.Lock()
	defer l.Unlock()
	if e, ok := r.cache.Get(k); ok && fresh(e) {
		return e.value, nil
	}
	f, ok := r.factories[factoryKey{kind, typ}]
	if !ok {
		return nil, fmt.Errorf("%s: no %s provider of type %q (BYO scorers arrive in M7)", role, kind, typ)
	}
	v, err := f(ctx, r.deps, Spec{Role: role, Resolved: res, Block: block, Dims: dims})
	if err != nil {
		if e, ok := r.cache.Get(k); ok {
			r.deps.Log.Warn("provider refresh failed; keeping previous", "tenant", res.Tenant.ID, "role", role, "err", err)
			return e.value, nil
		}
		return nil, fmt.Errorf("%s: %w", role, err)
	}
	if old, ok := r.cache.Peek(k); ok && old.value != v {
		r.retire(old.value)
	}
	e := &entry{value: v, builtAt: time.Now()}
	if rt, ok := v.(retrying); ok {
		e.expires = e.builtAt.Add(rt.retryAfter())
	}
	r.cache.Add(k, e)
	return v, nil
}

func (r *Registry) ttlFor(provider string) time.Duration {
	if provider == tenant.ProviderBYO {
		return r.byoRefresh
	}
	return 0
}

// nameOf labels a provider: its own name when it has one, else fallback.
func nameOf(v any, fallback string) string {
	if n, ok := v.(named); ok && n.ProviderName() != "" {
		return n.ProviderName()
	}
	return fallback
}

// centroidEntry is the index version and its centroids, refreshed together.
type centroidEntry struct {
	indexVersion int
	index        *centroids.Index
}

// For assembles the providers of res's tenant and config version from cached entries.
func (r *Registry) For(ctx context.Context, res *tenant.Resolved) (*pipeline.Providers, error) {
	cfg := res.Config
	text, err := r.get(ctx, res, "text_embedder", KindEmbedder, cfg.Embedders.Text.Provider, cfg.Embedders.Text, nil, r.ttlFor(cfg.Embedders.Text.Provider))
	if err != nil {
		return nil, err
	}
	image, err := r.get(ctx, res, "image_embedder", KindEmbedder, cfg.Embedders.Image.Provider, cfg.Embedders.Image, nil, r.ttlFor(cfg.Embedders.Image.Provider))
	if err != nil {
		return nil, err
	}
	te, ie := text.(pipeline.Embedder), image.(pipeline.Embedder)
	dims := map[string]int{pipeline.VectorText: te.Dim(), pipeline.VectorImage: ie.Dim()}
	lex, err := r.get(ctx, res, "lexical", KindLexical, cfg.Stores.Lexical.Provider, cfg.Stores.Lexical, dims, 0)
	if err != nil {
		return nil, err
	}
	vec, err := r.get(ctx, res, "vector", KindVectorStore, cfg.Stores.Vector.Provider, cfg.Stores.Vector, dims, r.ttlFor(cfg.Stores.Vector.Provider))
	if err != nil {
		return nil, err
	}
	pre, err := r.get(ctx, res, "prerank", KindScorer, cfg.Prerank.Scorer.Type, cfg.Prerank, nil, 0)
	if err != nil {
		return nil, err
	}
	rank, err := r.get(ctx, res, "rank", KindScorer, cfg.Rank.Scorer.Type, cfg.Rank, nil, 0)
	if err != nil {
		return nil, err
	}
	p := &pipeline.Providers{
		TextEmbedder: te, ImageEmbedder: ie, Lexical: lex.(pipeline.LexicalIndex), Vectors: vec.(pipeline.VectorStore),
		Prerank: pre.(pipeline.Scorer), Rank: rank.(pipeline.Scorer),
		Names: map[string]string{
			"text_embedder":  nameOf(te, cfg.Embedders.Text.Provider+":"+cfg.Embedders.Text.Model),
			"image_embedder": nameOf(ie, cfg.Embedders.Image.Provider+":"+cfg.Embedders.Image.Model),
			"lexical":        cfg.Stores.Lexical.Provider + ":opensearch", "vector": nameOf(vec, cfg.Stores.Vector.Provider+":qdrant"),
			"prerank": cfg.Prerank.Scorer.Type, "rank": cfg.Rank.Scorer.Type,
		},
		BYO: map[string]bool{
			"text_embedder": cfg.Embedders.Text.Provider == tenant.ProviderBYO, "image_embedder": cfg.Embedders.Image.Provider == tenant.ProviderBYO,
			"vector": cfg.Stores.Vector.Provider == tenant.ProviderBYO,
		},
	}
	ce, err := r.get(ctx, res, "centroids", KindCentroids, cfg.Stores.Lexical.Provider, lex, nil, r.refresh)
	if err == nil {
		c := ce.(*centroidEntry)
		p.IndexVersion = c.indexVersion
		if c.index != nil {
			p.Centroids = c.index
		}
	}
	return p, nil
}

func platformEmbedder(_ context.Context, d Deps, s Spec) (any, error) {
	c := s.Block.(tenant.EmbedderConfig)
	mc, ok := kservev2.PlatformModel(c.Model)
	if !ok {
		return nil, fmt.Errorf("unknown platform model %q", c.Model)
	}
	if s.Role == "image_embedder" && mc.ImageModel == "" {
		return nil, fmt.Errorf("platform model %q has no image tower", c.Model)
	}
	return kservev2.New(d.Platform.Triton, mc)
}

func platformLexical(_ context.Context, d Deps, s Spec) (any, error) {
	return opensearch.New(d.Platform.OpenSearch, s.Resolved.Tenant.ID, s.Resolved.Registry), nil
}

func platformVectors(_ context.Context, d Deps, s Spec) (any, error) {
	return qdrant.New(d.Platform.Qdrant, s.Resolved.Tenant.ID, qdrant.VectorDims(s.Dims), s.Resolved.Registry), nil
}

func defaultLinear(_ context.Context, _ Deps, s Spec) (any, error) {
	c := s.Block.(tenant.StageScorerConfig)
	w := c.Scorer.Weights
	if len(w) == 0 {
		w = defaultlinear.DefaultRankWeights()
		if s.Role == "prerank" {
			w = defaultlinear.DefaultPrerankWeights()
		}
	}
	return defaultlinear.New(w), nil
}

// platformCentroids reads the active index version from the lexical store and loads that
// version's centroids; it is rebuilt every refresh so re-indexes are picked up.
func platformCentroids(ctx context.Context, d Deps, s Spec) (any, error) {
	lex, ok := s.Block.(*opensearch.Store)
	if !ok {
		return &centroidEntry{}, nil
	}
	v, err := lex.Active(ctx)
	if err != nil {
		return nil, fmt.Errorf("active index version: %w", err)
	}
	e := &centroidEntry{indexVersion: v}
	if d.Platform.Centroids == nil || v == 0 {
		return e, nil
	}
	idx, err := d.Platform.Centroids.Load(ctx, string(s.Resolved.Tenant.ID), v)
	switch {
	case err == nil:
		e.index = idx
	case errors.Is(err, centroids.ErrNotFound):
		d.Log.Info("no category centroids for index version; category prediction disabled", "tenant", s.Resolved.Tenant.ID, "index_version", v)
	default:
		return nil, err
	}
	return e, nil
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
