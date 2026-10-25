package pipeline

import (
	"context"
	"errors"

	"github.com/zavisoft/search-engine/internal/catalog"
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

// VectorStore holds a tenant's product vectors. Search is added with retrieval (M2).
type VectorStore interface {
	Upsert(ctx context.Context, points []VectorPoint) error
	Delete(ctx context.Context, productIDs []string) error
	Health(ctx context.Context) error
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

// LexicalIndex holds a tenant's BM25 documents. It is also the arbiter of catalog event order:
// writes older than the indexed version are rejected and reported as stale product IDs, and the
// indexer then skips them for every other store. Search is added with retrieval (M2).
type LexicalIndex interface {
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
