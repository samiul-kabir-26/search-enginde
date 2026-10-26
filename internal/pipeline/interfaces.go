package pipeline

import (
	"context"
	"errors"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Named vectors in every tenant collection (docs/DATA.md §6).
const (
	VectorText  = "text"
	VectorImage = "image"
)

// ErrUnsupported is returned by a provider asked for something its model or store cannot do,
// such as image embeddings from a text-only embedder.
var ErrUnsupported = errors.New("unsupported by this provider")

// ErrInvalidInput marks a provider rejecting the input itself (for example an undecodable
// image), as opposed to being unavailable. Retrying the same input will not help.
var ErrInvalidInput = errors.New("invalid input")

// ErrNotFound is returned when a requested product does not exist in a store.
var ErrNotFound = errors.New("not found")

// Embedder turns text or encoded images into L2-normalized vectors of Dim() dimensions.
type Embedder interface {
	EmbedText(ctx context.Context, texts []string) ([][]float32, error)
	EmbedImage(ctx context.Context, images [][]byte) ([][]float32, error)
	Dim() int
}

// VectorPoint is one product in a vector store: its named vectors and filterable payload.
type VectorPoint struct {
	ProductID string
	Vectors   map[string][]float32
	Payload   map[string]any
}

// VectorSearch is a filtered kNN query on one named vector.
type VectorSearch struct {
	Vector string
	Query  []float32
	Filter filter.Expr
	K      int
	// EF is the HNSW search breadth.
	EF int
}

// VectorWriter is the indexing side of a vector store.
type VectorWriter interface {
	Upsert(ctx context.Context, points []VectorPoint) error
	Delete(ctx context.Context, productIDs []string) error
	Health(ctx context.Context) error
}

// VectorStore holds a tenant's product vectors.
type VectorStore interface {
	VectorWriter
	Search(ctx context.Context, req VectorSearch) ([]Hit, error)
	// Get returns a product's stored vectors and ranking doc, or ErrNotFound.
	Get(ctx context.Context, productID string) (*VectorPoint, *Doc, error)
}

// IndexDoc is a product write ordered by the catalog event's source version.
type IndexDoc struct {
	Product       catalog.Product
	SourceVersion int64
}

// IndexDelete removes a product unless a newer version is indexed; SourceVersion 0 always applies.
type IndexDelete struct {
	ProductID     string
	SourceVersion int64
}

// LexicalSearch is a BM25 query. Base filters (always_filters) constrain the query itself;
// Request filters are applied as a post-filter so facet counts can be disjunctive: each facet
// ignores the request conditions on its own field. With Popular set, Text is ignored and hits
// are ordered by popularity within the filters.
type LexicalSearch struct {
	Text    string
	Boosts  map[string]float64
	Base    filter.Expr
	Request filter.Expr
	K       int
	Facets  []FacetRequest
	Popular bool
}

// LexicalResult is the hits, facet counts and approximate total match count of a search.
type LexicalResult struct {
	Hits         []Hit
	Facets       map[string][]FacetValue
	TotalMatches int64
}

// LexicalIndex holds a tenant's BM25 documents and stored product data. It is also the
// arbiter of catalog event order: writes older than the indexed version are rejected and
// reported as stale product IDs, and the indexer then skips them for every other store.
type LexicalIndex interface {
	LexicalWriter
	Search(ctx context.Context, req LexicalSearch) (LexicalResult, error)
	// Docs returns stored product data by ID (missing IDs are absent from the map).
	Docs(ctx context.Context, ids []string) (map[string]*Doc, error)
}

// LexicalWriter is the indexing side of a lexical index.
type LexicalWriter interface {
	Upsert(ctx context.Context, docs []IndexDoc) (stale []string, err error)
	Delete(ctx context.Context, dels []IndexDelete) (stale []string, err error)
	Health(ctx context.Context) error
}

// IndexVersions manages the physical versions `t_{tenant}_products_v{n}` behind a tenant alias.
// Re-indexing builds v{n+1}, then Promote swaps the alias atomically (docs/ARCHITECTURE.md §7).
type IndexVersions interface {
	// Active returns the version the alias points at, or 0 when there is none.
	Active(ctx context.Context) (int, error)
	// Latest returns the highest existing version, or 0.
	Latest(ctx context.Context) (int, error)
	Create(ctx context.Context, version int) error
	Promote(ctx context.Context, version int) error
	Count(ctx context.Context, version int) (int64, error)
	Drop(ctx context.Context, version int) error
}

// Retriever produces one ranked candidate list for S2.
type Retriever interface {
	Name() string
	Retrieve(ctx context.Context, q *Query, k int) ([]Hit, error)
}

// FeatureSchema is a scorer's ordered input features with their defaults for missing values.
type FeatureSchema struct {
	Names    []string
	Defaults map[string]float64
}

// Scorer scores candidates for S4 and S5.
type Scorer interface {
	Score(ctx context.Context, q *Query, cs []Candidate) ([]float32, error)
	FeatureSchema() FeatureSchema
}

// CentroidIndex predicts categories from a text vector.
type CentroidIndex interface {
	Nearest(vec []float32, k int, minConfidence float64) []CategoryGuess
}

// FeatureRequest names what one request needs from the online feature store.
type FeatureRequest struct {
	ProductIDs []string
	VendorIDs  []string
	UserID     string
}

// OnlineFeatures are the stored item, vendor and user features for one request, keyed by
// catalog feature name (docs/MODELS.md §4) and already normalized. Absent IDs have no entry,
// so scorers use the declared defaults.
type OnlineFeatures struct {
	Items   map[string]map[string]float64
	Vendors map[string]map[string]float64
	// User is nil when the request names no user or the user has no stored features.
	User *UserFeatures
}

// UserFeatures are a user's stored preferences.
type UserFeatures struct {
	// CategoryAffinity is the user's share of recent interactions per category ID.
	CategoryAffinity map[string]float64
	// PriceLo and PriceHi bound the user's usual price range in minor units (0, 0 = unknown).
	PriceLo, PriceHi int64
}

// FeatureReader reads online features (PLAN.md R9) in one round trip.
type FeatureReader interface {
	Read(ctx context.Context, tenant tenant.ID, req FeatureRequest) (*OnlineFeatures, error)
}

// Providers are the tenant's providers for one config version, built by the registry.
type Providers struct {
	TextEmbedder Embedder
	// ImageEmbedder's EmbedText is the text tower used by vector_text_to_image.
	ImageEmbedder Embedder
	Lexical       LexicalIndex
	Vectors       VectorStore
	// Centroids is nil until centroids exist for the active index version.
	Centroids    CentroidIndex
	Prerank      Scorer
	Rank         Scorer
	IndexVersion int
	// Names label providers in traces and breaker keys, by role
	// (text_embedder, image_embedder, lexical, vector, prerank, rank).
	Names map[string]string
	// BYO marks the roles served by a tenant's own endpoint; their failures fall back to the
	// platform providers (lexical retrieval) within the stage budget.
	BYO map[string]bool
}
