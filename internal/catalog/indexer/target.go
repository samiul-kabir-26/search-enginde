package indexer

import (
	"context"
	"fmt"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	qdrantgo "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
	"github.com/zavisoft/search-engine/internal/providers/lexical/opensearch"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Builder builds indexing targets on the platform-default infrastructure. BYO stores and
// embedders are wired through the provider registry in M4–M5.
type Builder struct {
	Tenants    *tenant.Store
	Triton     grpc.ClientConnInterface
	OpenSearch *opensearchapi.Client
	Qdrant     *qdrantgo.Client
}

// Stores are a tenant's platform stores, which can be bound to a physical version.
type Stores struct {
	Lexical *opensearch.Store
	Vectors *qdrant.Store
}

// Build loads the tenant's active config and returns a Target writing to its aliases.
func (b *Builder) Build(ctx context.Context, id tenant.ID) (*Target, Stores, error) {
	version, cfg, err := b.Tenants.ActiveConfig(ctx, id)
	if err != nil {
		return nil, Stores{}, err
	}
	reg, err := catalog.NewRegistry(cfg.Catalog.CustomFields)
	if err != nil {
		return nil, Stores{}, err
	}
	mapper, err := catalog.NewMapper(cfg.Catalog.FieldMapping, cfg.Catalog.CustomFields)
	if err != nil {
		return nil, Stores{}, err
	}
	text, err := b.platformEmbedder(cfg.Embedders.Text, "text")
	if err != nil {
		return nil, Stores{}, err
	}
	image, err := b.platformEmbedder(cfg.Embedders.Image, "image")
	if err != nil {
		return nil, Stores{}, err
	}
	if cfg.Stores.Lexical.Provider != tenant.ProviderPlatform || cfg.Stores.Vector.Provider != tenant.ProviderPlatform {
		return nil, Stores{}, fmt.Errorf("tenant %s: only platform stores are supported until M5", id)
	}
	dims := qdrant.VectorDims{pipeline.VectorText: text.Dim(), pipeline.VectorImage: image.Dim()}
	stores := Stores{
		Lexical: opensearch.New(b.OpenSearch, id, reg),
		Vectors: qdrant.New(b.Qdrant, id, dims, reg),
	}
	return &Target{
		Tenant:        id,
		ConfigVersion: version,
		Mapper:        mapper,
		Registry:      reg,
		TextEmbedder:  text,
		ImageEmbedder: image,
		Lexical:       stores.Lexical,
		Vectors:       stores.Vectors,
	}, stores, nil
}

func (b *Builder) platformEmbedder(c tenant.EmbedderConfig, kind string) (*kservev2.Embedder, error) {
	if c.Provider != tenant.ProviderPlatform {
		return nil, fmt.Errorf("%s embedder: only platform embedders are supported until M5", kind)
	}
	mc, ok := kservev2.PlatformModel(c.Model)
	if !ok {
		return nil, fmt.Errorf("%s embedder: unknown platform model %q", kind, c.Model)
	}
	if kind == "image" && mc.ImageModel == "" {
		return nil, fmt.Errorf("image embedder: platform model %q has no image tower", c.Model)
	}
	return kservev2.New(b.Triton, mc)
}

// EnsureActive makes sure both stores have an active version, creating and promoting the same
// new version number in each on first use. It returns the active version.
func (s Stores) EnsureActive(ctx context.Context) (int, error) {
	la, err := s.Lexical.Active(ctx)
	if err != nil {
		return 0, err
	}
	va, err := s.Vectors.Active(ctx)
	if err != nil {
		return 0, err
	}
	if la > 0 && la == va {
		return la, nil
	}
	if la > 0 || va > 0 {
		return 0, fmt.Errorf("lexical alias is at v%d but vector alias is at v%d; run a backfill to realign", la, va)
	}
	v, err := s.nextVersion(ctx)
	if err != nil {
		return 0, err
	}
	if err := s.create(ctx, v); err != nil {
		return 0, err
	}
	return v, s.promote(ctx, v)
}

func (s Stores) nextVersion(ctx context.Context) (int, error) {
	ll, err := s.Lexical.Latest(ctx)
	if err != nil {
		return 0, err
	}
	vl, err := s.Vectors.Latest(ctx)
	if err != nil {
		return 0, err
	}
	return max(ll, vl) + 1, nil
}

func (s Stores) create(ctx context.Context, v int) error {
	if err := s.Lexical.Create(ctx, v); err != nil {
		return err
	}
	return s.Vectors.Create(ctx, v)
}

func (s Stores) promote(ctx context.Context, v int) error {
	if err := s.Lexical.Promote(ctx, v); err != nil {
		return err
	}
	return s.Vectors.Promote(ctx, v)
}

func (s Stores) drop(ctx context.Context, v int) error {
	if err := s.Lexical.Drop(ctx, v); err != nil {
		return err
	}
	return s.Vectors.Drop(ctx, v)
}
