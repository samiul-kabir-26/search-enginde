package indexer

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
	qdrantgo "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/catalog/centroids"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
	"github.com/zavisoft/search-engine/internal/providers/lexical/opensearch"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Builder builds indexing targets: the platform lexical index plus the tenant's vector store
// (the platform collection, or a BYO store written in place) and embedders.
type Builder struct {
	Tenants    *tenant.Store
	Triton     grpc.ClientConnInterface
	OpenSearch *opensearchapi.Client
	Qdrant     *qdrantgo.Client
	// Centroids, when set, receives category centroids for every backfilled version.
	Centroids *centroids.Store
	// BYO enables tenants with BYO embedders and vector stores.
	BYO *registry.BYO
	// Vendors stores vendor features from catalog.vendor.v1 and catalog.vendor_mapping (nil
	// disables both).
	Vendors VendorWriter
}

// ErrBYO marks a failure building or calling a tenant's own (BYO) provider. Such failures are
// the tenant's alone, so the consumer skips that tenant's events after a few retries rather
// than stalling every tenant.
var ErrBYO = errors.New("BYO provider")

// Stores are a tenant's platform stores, which can be bound to a physical version. Vectors is
// nil when the tenant's vectors live in a BYO store, which has no versions and is written in
// place; BYO is that store.
type Stores struct {
	Lexical *opensearch.Store
	Vectors *qdrant.Store
	BYO     registry.BYOVectorStore
}

// Build loads the tenant's active config and returns a Target writing to its aliases.
func (b *Builder) Build(ctx context.Context, id tenant.ID) (*Target, Stores, error) {
	return b.BuildVersion(ctx, id, 0)
}

// BuildVersion is Build for a specific published config version (0 means the active one). The
// caller closes the returned Target when done with it.
func (b *Builder) BuildVersion(ctx context.Context, id tenant.ID, v int) (*Target, Stores, error) {
	version, cfg, err := v, (*tenant.Config)(nil), error(nil)
	if v == 0 {
		version, cfg, err = b.Tenants.ActiveConfig(ctx, id)
	} else {
		cfg, err = b.Tenants.ConfigVersion(ctx, id, v)
	}
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
	if cfg.Stores.Lexical.Provider != tenant.ProviderPlatform {
		return nil, Stores{}, fmt.Errorf("tenant %s: the lexical index is platform-hosted only", id)
	}
	vendorMapper, err := catalog.NewVendorMapper(cfg.Catalog.VendorMapping)
	if err != nil {
		return nil, Stores{}, fmt.Errorf("tenant %s: %w", id, err)
	}
	t := &Target{Tenant: id, ConfigVersion: version, Mapper: mapper, Registry: reg, VendorMapper: vendorMapper, Vendors: b.Vendors}
	var plan tenant.Plan
	if uses(cfg, tenant.ProviderBYO) {
		if b.BYO == nil {
			return nil, Stores{}, fmt.Errorf("tenant %s: %w providers are not enabled in this indexer", id, ErrBYO)
		}
		tn, err := b.Tenants.Tenant(ctx, id)
		if err != nil {
			return nil, Stores{}, err
		}
		if plan, err = b.Tenants.Plan(ctx, tn.Plan); err != nil {
			return nil, Stores{}, err
		}
		t.BYO = true
	}
	fail := func(err error) (*Target, Stores, error) {
		_ = t.Close()
		return nil, Stores{}, err
	}
	if t.TextEmbedder, err = b.embedder(ctx, id, plan, cfg.Embedders.Text, "text"); err != nil {
		return fail(err)
	}
	image, err := b.embedder(ctx, id, plan, cfg.Embedders.Image, "image")
	if err != nil {
		return fail(err)
	}
	stores := Stores{Lexical: opensearch.New(b.OpenSearch, id, reg)}
	t.Lexical = stores.Lexical
	if vc := cfg.Stores.Vector; vc.Provider == tenant.ProviderBYO {
		conn, err := b.Tenants.Connection(ctx, id, vc.ConnectionID)
		if err != nil {
			return fail(fmt.Errorf("tenant %s: %w: %w", id, ErrBYO, err))
		}
		if stores.BYO, err = registry.NewBYOVectorStore(ctx, b.BYO, id, plan, conn, vc); err != nil {
			return fail(fmt.Errorf("tenant %s: %w: %w", id, ErrBYO, err))
		}
		t.Vectors, t.closers = stores.BYO, append(t.closers, stores.BYO)
		if _, ok := vc.Vectors[pipeline.VectorImage]; ok {
			t.ImageEmbedder = image
		} else if c, ok := image.(io.Closer); ok {
			_ = c.Close()
		}
	} else {
		dims := qdrant.VectorDims{pipeline.VectorText: t.TextEmbedder.Dim(), pipeline.VectorImage: image.Dim()}
		stores.Vectors = qdrant.New(b.Qdrant, id, dims, reg)
		t.Vectors, t.ImageEmbedder = stores.Vectors, image
	}
	return t, stores, nil
}

func uses(cfg *tenant.Config, provider string) bool {
	return cfg.Embedders.Text.Provider == provider || cfg.Embedders.Image.Provider == provider || cfg.Stores.Vector.Provider == provider
}

func (b *Builder) embedder(ctx context.Context, id tenant.ID, plan tenant.Plan, c tenant.EmbedderConfig, kind string) (pipeline.Embedder, error) {
	switch c.Provider {
	case tenant.ProviderPlatform:
		mc, ok := kservev2.PlatformModel(c.Model)
		if !ok {
			return nil, fmt.Errorf("%s embedder: unknown platform model %q", kind, c.Model)
		}
		if kind == "image" && mc.ImageModel == "" {
			return nil, fmt.Errorf("image embedder: platform model %q has no image tower", c.Model)
		}
		return kservev2.New(b.Triton, mc)
	case tenant.ProviderBYO:
		m, err := b.Tenants.Model(ctx, id, c.ModelID)
		if err != nil {
			return nil, fmt.Errorf("%s embedder: %w: %w", kind, ErrBYO, err)
		}
		e, err := registry.NewBYOEmbedder(ctx, b.BYO, id, plan, m)
		if err != nil {
			return nil, fmt.Errorf("%s embedder: %w: %w", kind, ErrBYO, err)
		}
		return e, nil
	}
	return nil, fmt.Errorf("%s embedder: provider %q is not platform or byo", kind, c.Provider)
}

// EnsureActive makes sure both stores have an active version, creating and promoting the same
// new version number in each on first use. It returns the active version. A BYO vector store
// has no versions, so only the lexical index is versioned.
func (s Stores) EnsureActive(ctx context.Context) (int, error) {
	la, err := s.Lexical.Active(ctx)
	if err != nil {
		return 0, err
	}
	va := la
	if s.Vectors != nil {
		if va, err = s.Vectors.Active(ctx); err != nil {
			return 0, err
		}
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
	if err != nil || s.Vectors == nil {
		return ll + 1, err
	}
	vl, err := s.Vectors.Latest(ctx)
	if err != nil {
		return 0, err
	}
	return max(ll, vl) + 1, nil
}

func (s Stores) create(ctx context.Context, v int) error {
	if err := s.Lexical.Create(ctx, v); err != nil || s.Vectors == nil {
		return err
	}
	return s.Vectors.Create(ctx, v)
}

func (s Stores) promote(ctx context.Context, v int) error {
	if err := s.Lexical.Promote(ctx, v); err != nil || s.Vectors == nil {
		return err
	}
	return s.Vectors.Promote(ctx, v)
}

func (s Stores) drop(ctx context.Context, v int) error {
	if err := s.Lexical.Drop(ctx, v); err != nil || s.Vectors == nil {
		return err
	}
	return s.Vectors.Drop(ctx, v)
}
