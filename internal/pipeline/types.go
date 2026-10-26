package pipeline

import (
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// QueryType selects the retriever set (docs/ARCHITECTURE.md §6 S2).
type QueryType string

// Query types.
const (
	QueryText    QueryType = "text"
	QueryImage   QueryType = "image"
	QuerySimilar QueryType = "similar"
)

// Query is a search request after S1: normalized text, extracted attributes, predicted
// categories, embeddings and filters.
type Query struct {
	Tenant        tenant.ID
	ConfigVersion int
	Type          QueryType
	// Raw is the request text; Text is the normalized text with recognized price phrases removed.
	Raw        string
	Text       string
	Attributes map[string][]string
	Categories []CategoryGuess
	// TextVector comes from the text embedder; TextToImageVector from the image embedder's text
	// tower; ImageVector from an uploaded image or a similar-items anchor.
	TextVector        []float32
	TextToImageVector []float32
	ImageVector       []float32
	// ImageBytes is the validated, re-encoded upload of an image query (embedded in S1).
	ImageBytes []byte
	// AnchorID is the product a similar-items query starts from; it is excluded from results.
	AnchorID string
	// Filter is the compiled request filter, including filters derived in S1 (price phrases).
	Filter filter.Expr
	// BaseFilter is the tenant's compiled always_filters; it is never shown as a facet selection.
	BaseFilter filter.Expr
	User       UserContext
	Facets     []FacetRequest
	// Depth is how many ordered results the pipeline must produce (the plan's page depth).
	Depth   int
	Explain bool
}

// CategoryGuess is a predicted category with a confidence in [0, 1].
type CategoryGuess struct {
	ID         string  `json:"id"`
	Confidence float64 `json:"confidence"`
}

// UserContext identifies the shopper, if known.
type UserContext struct {
	ID        string `json:"id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

// FacetRequest asks for counts on one field; numeric fields use Ranges.
type FacetRequest struct {
	Field  string
	Size   int
	Ranges []FacetRange
}

// FacetRange is a half-open bucket [From, To); nil ends are open.
type FacetRange struct {
	From *float64
	To   *float64
}

// FacetValue is one facet bucket.
type FacetValue struct {
	Value string   `json:"value"`
	Count int64    `json:"count"`
	From  *float64 `json:"from,omitempty"`
	To    *float64 `json:"to,omitempty"`
}

// Doc is the stored product data a candidate carries through ranking and into the response.
type Doc struct {
	Title               string     `json:"title"`
	Brand               string     `json:"brand,omitempty"`
	VendorID            string     `json:"vendor_id"`
	PriceMinor          int64      `json:"price_minor"`
	Currency            string     `json:"currency"`
	ImageURL            string     `json:"image_url,omitempty"`
	CategoryIDs         []string   `json:"category_ids,omitempty"`
	Rating              *float64   `json:"rating,omitempty"`
	RatingCount         int64      `json:"rating_count,omitempty"`
	ListingCompleteness float64    `json:"listing_completeness,omitempty"`
	ImageCount          int        `json:"image_count,omitempty"`
	CreatedAt           *time.Time `json:"created_at,omitempty"`
	// Stock, Status, ShipRegions and Custom (filterable custom fields, keyed without the
	// `custom.` prefix) are read for CEL policies.
	Stock       *int64         `json:"stock,omitempty"`
	Status      string         `json:"status,omitempty"`
	ShipRegions []string       `json:"ship_regions,omitempty"`
	Custom      map[string]any `json:"custom,omitempty"`
}

// Hit is one retriever result: a raw retriever score (BM25 or cosine) and the stored doc.
type Hit struct {
	ProductID string
	Score     float64
	Doc       *Doc
}

// RetrieverHit is a candidate's rank (1-based) and raw score in one retriever.
type RetrieverHit struct {
	Rank  int
	Score float64
}

// Candidate is a product moving through S3–S6.
type Candidate struct {
	ProductID  string
	Doc        *Doc
	RRF        float64
	Retrievers map[string]RetrieverHit
	Features   map[string]float64
	// Score is the latest stage's score; ordering is by Score descending.
	Score float64
	// Stage is the last stage that scored this candidate, which tells deep pages apart:
	// positions past rank.keep keep their pre-rank or RRF order.
	Stage     string
	Sponsored bool
	// PolicyBoost is the sum of the tenant's CEL boost rules, clamped to [-1, 1] (S3).
	PolicyBoost float64
}
