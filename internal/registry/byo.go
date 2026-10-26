package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
	"github.com/zavisoft/search-engine/internal/providers/embedder/openaiembeddings"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/milvus"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/pgvector"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// BYOSource loads a tenant's registered models and connections (implemented by tenant.Store).
type BYOSource interface {
	Model(ctx context.Context, id tenant.ID, modelID string) (tenant.Model, error)
	Connection(ctx context.Context, id tenant.ID, connID string) (tenant.Connection, error)
}

// BYO is what BYO factories need: every call to a tenant endpoint goes through Egress, and
// credentials come from Secrets.
type BYO struct {
	Source  BYOSource
	Secrets security.SecretStore
	Egress  *security.Egress
	// Bulkheads, when set, bound each tenant's concurrent BYO calls (search-api).
	Bulkheads *resilience.Bulkheads
	// Timeout is the backstop for one BYO HTTP request; callers pass tighter deadlines.
	Timeout time.Duration
}

// byoRetry is how long a BYO provider that failed to build is kept before rebuilding it.
const byoRetry = 30 * time.Second

func (b *BYO) secret(ctx context.Context, t tenant.ID, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	s, err := b.Secrets.Get(ctx, string(t), ref)
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", ref, err)
	}
	return s.Reveal(), nil
}

func (b *BYO) plaintext(endpoint string) bool {
	u, err := url.Parse(endpoint)
	return err == nil && b.Egress.Trusted(u.Host)
}

func (b *BYO) httpTimeout() time.Duration {
	if b.Timeout > 0 {
		return b.Timeout
	}
	return 10 * time.Second
}

// NewBYOEmbedder builds a registered model's embedder. It reads the model's secret but dials
// nothing: connections are made by the first call, under that call's deadline.
func NewBYOEmbedder(ctx context.Context, b *BYO, t tenant.ID, plan tenant.Plan, m tenant.Model) (pipeline.Embedder, error) {
	if m.Kind != "embedder" {
		return nil, fmt.Errorf("model %s is a %s model, not an embedder", m.ID, m.Kind)
	}
	token, err := b.secret(ctx, t, m.SecretRef)
	if err != nil {
		return nil, err
	}
	allow := plan.EgressAllowlist
	name := "byo:" + m.Type + ":" + m.Name
	switch m.Type {
	case tenant.ModelKServeV2:
		cfg := kservev2.Config{Version: m.Version, Dim: m.Output.Dim, MaxBatch: m.Settings.MaxBatchSize}
		if m.Input.Modality == tenant.ModalityImage {
			cfg.ImageModel = m.Name
			if m.Settings.SupportsTextToImage {
				cfg.TextModel = m.Name
			}
		} else {
			cfg.TextModel = m.Name
		}
		return kservev2.NewRemote(kservev2.Remote{
			Endpoint: m.Endpoint, Transport: m.Settings.Transport, Token: token, Plaintext: b.plaintext(m.Endpoint),
			Dial: b.Egress.GRPCDialer(allow), HTTP: b.Egress.HTTPClient(allow, b.httpTimeout()), Name: name,
		}, cfg)
	case tenant.ModelOpenAIEmbeddings:
		if m.Input.Modality != tenant.ModalityText {
			return nil, fmt.Errorf("openai_embeddings models embed text only")
		}
		return openaiembeddings.New(b.Egress.HTTPClient(allow, b.httpTimeout()), openaiembeddings.Config{
			Endpoint: m.Endpoint, Model: m.Name, Dim: m.Output.Dim, MaxBatch: m.Settings.MaxBatchSize, APIKey: token, Name: name,
		})
	}
	return nil, fmt.Errorf("model type %q cannot embed", m.Type)
}

// StoreSpec converts a BYO vector store block to the adapters' collection spec.
func StoreSpec(block tenant.StoreConfig, custom ...string) vectorstore.Spec {
	spec := vectorstore.Spec{Collection: block.Collection, Fields: block.PayloadMapping, Vectors: map[string]vectorstore.VectorSpec{}, Custom: custom}
	for name, v := range block.Vectors {
		spec.Vectors[name] = vectorstore.VectorSpec{Name: v.Name, Dim: v.Dim, Metric: v.Metric}
	}
	return spec
}

// BYOVectorStore is a BYO store adapter: the pipeline interface plus what connection tests,
// backfills and centroid builds use.
type BYOVectorStore interface {
	pipeline.VectorStore
	vectorstore.Describer
	Count(ctx context.Context, v int) (int64, error)
	ScrollVectors(ctx context.Context, v int, vector string, fn func(string, []float32, []string) error) error
	ProviderName() string
	io.Closer
}

// NewBYOVectorStore builds the adapter for a connection and a config block. It reads the
// connection's secret but dials nothing. custom are filterable custom fields read back for
// CEL policies (canonical `custom.x` names).
func NewBYOVectorStore(ctx context.Context, b *BYO, t tenant.ID, plan tenant.Plan, conn tenant.Connection, block tenant.StoreConfig, custom ...string) (BYOVectorStore, error) {
	if conn.Kind != tenant.ConnectionVectorStore {
		return nil, fmt.Errorf("connection %s is a %s connection, not a vector store", conn.ID, conn.Kind)
	}
	secret, err := b.secret(ctx, t, conn.SecretRef)
	if err != nil {
		return nil, err
	}
	allow, plain, spec := plan.EgressAllowlist, b.plaintext(conn.Endpoint), StoreSpec(block, custom...)
	switch conn.Type {
	case tenant.StoreQdrant:
		c, err := qdrant.DialBYO(conn.Endpoint, secret, plain, b.Egress.GRPCDialer(allow))
		if err != nil {
			return nil, err
		}
		return qdrant.NewBYO(c, spec), nil
	case tenant.StorePgvector:
		pool, err := pgvector.Connect(ctx, conn.Endpoint, secret, plain, b.Egress.DialContext(allow))
		if err != nil {
			return nil, err
		}
		return pgvector.New(pool, spec), nil
	case tenant.StoreMilvus:
		cfg := milvus.Config{Endpoint: conn.Endpoint, Username: conn.Settings.Username, Database: conn.Settings.Database,
			Plaintext: plain, Dial: b.Egress.GRPCDialer(allow)}
		if cfg.Username != "" {
			cfg.Password = secret
		} else {
			cfg.APIKey = secret
		}
		return milvus.New(cfg, spec)
	}
	return nil, fmt.Errorf("vector store type %q is not supported", conn.Type)
}

func byoEmbedder(ctx context.Context, d Deps, s Spec) (any, error) {
	c := s.Block.(tenant.EmbedderConfig)
	id := s.Resolved.Tenant.ID
	m, err := d.BYO.Source.Model(ctx, id, c.ModelID)
	if err != nil {
		return &brokenEmbedder{err: err, name: "byo:" + c.ModelID}, nil
	}
	if m.Status != tenant.StatusValidated {
		return &brokenEmbedder{err: fmt.Errorf("model %s is %s, not validated", m.ID, m.Status), dim: m.Output.Dim, name: "byo:" + c.ModelID}, nil
	}
	e, err := NewBYOEmbedder(ctx, d.BYO, id, s.Resolved.Plan, m)
	if err != nil {
		return &brokenEmbedder{err: err, dim: m.Output.Dim, name: "byo:" + c.ModelID}, nil
	}
	if d.BYO.Bulkheads == nil {
		return e, nil
	}
	return &guardedEmbedder{Embedder: e, tenant: string(id), b: d.BYO.Bulkheads}, nil
}

func byoVectors(ctx context.Context, d Deps, s Spec) (any, error) {
	c := s.Block.(tenant.StoreConfig)
	id := s.Resolved.Tenant.ID
	conn, err := d.BYO.Source.Connection(ctx, id, c.ConnectionID)
	if err != nil {
		return &brokenStore{err: err, name: "byo:" + c.ConnectionID}, nil
	}
	if conn.Status != tenant.StatusValidated {
		return &brokenStore{err: fmt.Errorf("connection %s is %s, not validated", conn.ID, conn.Status), name: "byo:" + conn.Type}, nil
	}
	st, err := NewBYOVectorStore(ctx, d.BYO, id, s.Resolved.Plan, conn, c, vectorstore.CustomRankingFields(s.Resolved.Registry)...)
	if err != nil {
		return &brokenStore{err: err, name: "byo:" + conn.Type}, nil
	}
	if d.BYO.Bulkheads == nil {
		return st, nil
	}
	return &guardedStore{VectorStore: st, closer: st, name: st.ProviderName(), tenant: string(id), b: d.BYO.Bulkheads}, nil
}

// named is implemented by providers that label themselves in traces and breaker keys.
type named interface{ ProviderName() string }

// retrying is implemented by providers cached for a shorter time than their role's refresh.
type retrying interface{ retryAfter() time.Duration }

// errBYOUnavailable wraps why a BYO provider could not be built; calls fail with it, so the
// stage falls back instead of the request failing. Broken providers are pointers because the
// registry compares cached values.
var errBYOUnavailable = errors.New("BYO provider unavailable")

type brokenEmbedder struct {
	err  error
	dim  int
	name string
}

func (b *brokenEmbedder) fail() error { return fmt.Errorf("%w: %w", errBYOUnavailable, b.err) }

func (b *brokenEmbedder) EmbedText(context.Context, []string) ([][]float32, error) {
	return nil, b.fail()
}

func (b *brokenEmbedder) EmbedImage(context.Context, [][]byte) ([][]float32, error) {
	return nil, b.fail()
}

func (b *brokenEmbedder) Dim() int                  { return b.dim }
func (b *brokenEmbedder) ProviderName() string      { return b.name }
func (b *brokenEmbedder) retryAfter() time.Duration { return byoRetry }

type brokenStore struct {
	err  error
	name string
}

func (b *brokenStore) fail() error { return fmt.Errorf("%w: %w", errBYOUnavailable, b.err) }

func (b *brokenStore) Search(context.Context, pipeline.VectorSearch) ([]pipeline.Hit, error) {
	return nil, b.fail()
}

func (b *brokenStore) Get(context.Context, string) (*pipeline.VectorPoint, *pipeline.Doc, error) {
	return nil, nil, b.fail()
}

func (b *brokenStore) Upsert(context.Context, []pipeline.VectorPoint) error { return b.fail() }
func (b *brokenStore) Delete(context.Context, []string) error               { return b.fail() }
func (b *brokenStore) Health(context.Context) error                         { return b.fail() }
func (b *brokenStore) ProviderName() string                                 { return b.name }
func (b *brokenStore) retryAfter() time.Duration                            { return byoRetry }

// guardedEmbedder takes a slot of the tenant's bulkhead for every call.
type guardedEmbedder struct {
	pipeline.Embedder
	tenant string
	b      *resilience.Bulkheads
}

func (g *guardedEmbedder) EmbedText(ctx context.Context, texts []string) ([][]float32, error) {
	release, err := g.b.Acquire(g.tenant)
	if err != nil {
		return nil, err
	}
	defer release()
	return g.Embedder.EmbedText(ctx, texts)
}

func (g *guardedEmbedder) EmbedImage(ctx context.Context, images [][]byte) ([][]float32, error) {
	release, err := g.b.Acquire(g.tenant)
	if err != nil {
		return nil, err
	}
	defer release()
	return g.Embedder.EmbedImage(ctx, images)
}

func (g *guardedEmbedder) ProviderName() string {
	if n, ok := g.Embedder.(named); ok {
		return n.ProviderName()
	}
	return "byo"
}

func (g *guardedEmbedder) Close() error {
	if c, ok := g.Embedder.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("close %s: %w", g.ProviderName(), err)
		}
	}
	return nil
}

// guardedStore takes a slot of the tenant's bulkhead for every call.
type guardedStore struct {
	pipeline.VectorStore
	closer io.Closer
	name   string
	tenant string
	b      *resilience.Bulkheads
}

func guarded[T any](g *guardedStore, f func() (T, error)) (T, error) {
	release, err := g.b.Acquire(g.tenant)
	if err != nil {
		var zero T
		return zero, err
	}
	defer release()
	return f()
}

func (g *guardedStore) Search(ctx context.Context, req pipeline.VectorSearch) ([]pipeline.Hit, error) {
	return guarded(g, func() ([]pipeline.Hit, error) { return g.VectorStore.Search(ctx, req) })
}

func (g *guardedStore) Get(ctx context.Context, id string) (*pipeline.VectorPoint, *pipeline.Doc, error) {
	type got struct {
		p *pipeline.VectorPoint
		d *pipeline.Doc
	}
	r, err := guarded(g, func() (got, error) {
		p, d, err := g.VectorStore.Get(ctx, id)
		return got{p, d}, err
	})
	return r.p, r.d, err
}

func (g *guardedStore) Upsert(ctx context.Context, points []pipeline.VectorPoint) error {
	_, err := guarded(g, func() (struct{}, error) { return struct{}{}, g.VectorStore.Upsert(ctx, points) })
	return err
}

func (g *guardedStore) Delete(ctx context.Context, ids []string) error {
	_, err := guarded(g, func() (struct{}, error) { return struct{}{}, g.VectorStore.Delete(ctx, ids) })
	return err
}

func (g *guardedStore) ProviderName() string { return g.name }
func (g *guardedStore) Close() error {
	if err := g.closer.Close(); err != nil {
		return fmt.Errorf("close %s: %w", g.name, err)
	}
	return nil
}
