package tenant

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
)

// CurrentSchemaVersion is the config document schema this build reads and writes.
const CurrentSchemaVersion = 1

//go:embed default_config.json
var defaultDocument []byte

// DefaultDocument returns the default config document for new tenants (docs/TENANT_CONFIG.md §3).
func DefaultDocument() json.RawMessage { return bytes.Clone(defaultDocument) }

// Config is a tenant pipeline config document (docs/TENANT_CONFIG.md §3).
type Config struct {
	SchemaVersion int               `json:"schema_version"`
	Locale        string            `json:"locale"`
	Budgets       Budgets           `json:"budgets"`
	Catalog       CatalogConfig     `json:"catalog"`
	Query         QueryConfig       `json:"query,omitzero"`
	Embedders     EmbeddersConfig   `json:"embedders"`
	Stores        StoresConfig      `json:"stores"`
	Retrieval     RetrievalConfig   `json:"retrieval"`
	Facets        []FacetConfig     `json:"facets,omitempty"`
	Policy        PolicyConfig      `json:"policy"`
	Prerank       StageScorerConfig `json:"prerank"`
	Rank          StageScorerConfig `json:"rank"`
	Rerank        RerankConfig      `json:"rerank"`
	Ads           AdsConfig         `json:"ads"`
}

// Budgets are per-request and per-stage time budgets in milliseconds.
type Budgets struct {
	TotalMS int `json:"total_ms"`
	S1MS    int `json:"s1_ms"`
	S2MS    int `json:"s2_ms"`
	S3MS    int `json:"s3_ms"`
	S4MS    int `json:"s4_ms"`
	S5MS    int `json:"s5_ms"`
	S6MS    int `json:"s6_ms"`
	// Image overrides budgets for image and similar-item queries; zero fields inherit. Image
	// embedding on CPU needs a larger S1 than text.
	Image *StageBudgets `json:"image,omitempty"`
}

// StageBudgets is a set of budget overrides.
type StageBudgets struct {
	TotalMS int `json:"total_ms,omitempty"`
	S1MS    int `json:"s1_ms,omitempty"`
	S2MS    int `json:"s2_ms,omitempty"`
	S3MS    int `json:"s3_ms,omitempty"`
	S4MS    int `json:"s4_ms,omitempty"`
	S5MS    int `json:"s5_ms,omitempty"`
	S6MS    int `json:"s6_ms,omitempty"`
}

// Stages returns the stage budgets in pipeline order.
func (b Budgets) Stages() [6]int { return [6]int{b.S1MS, b.S2MS, b.S3MS, b.S4MS, b.S5MS, b.S6MS} }

// ForImage returns the budgets with the image overrides applied.
func (b Budgets) ForImage() Budgets {
	out := b
	out.Image = nil
	if o := b.Image; o != nil {
		pick := func(over, base int) int {
			if over > 0 {
				return over
			}
			return base
		}
		out.TotalMS, out.S1MS, out.S2MS = pick(o.TotalMS, b.TotalMS), pick(o.S1MS, b.S1MS), pick(o.S2MS, b.S2MS)
		out.S3MS, out.S4MS, out.S5MS, out.S6MS = pick(o.S3MS, b.S3MS), pick(o.S4MS, b.S4MS), pick(o.S5MS, b.S5MS), pick(o.S6MS, b.S6MS)
	}
	return out
}

// CatalogConfig is the `catalog` section.
type CatalogConfig struct {
	Ingestion     string                `json:"ingestion"`
	FieldMapping  catalog.FieldMapping  `json:"field_mapping"`
	CustomFields  []catalog.CustomField `json:"custom_fields,omitempty"`
	VendorMapping catalog.VendorMapping `json:"vendor_mapping,omitempty"`
}

// QueryConfig is the optional `query` section: tenant additions to the S1 attribute
// dictionary, keyed by attribute (`color`, `material`, `size` or a new one) and listing terms.
type QueryConfig struct {
	Attributes map[string][]string `json:"attributes,omitempty"`
}

// Provider kinds for embedder and store blocks (docs/TENANT_CONFIG.md §4).
const (
	ProviderPlatform = "platform"
	ProviderBYO      = "byo"
)

// EmbeddersConfig is the `embedders` section.
type EmbeddersConfig struct {
	Text  EmbedderConfig `json:"text"`
	Image EmbedderConfig `json:"image"`
}

// EmbedderConfig selects a platform model by ID or a registered BYO model.
type EmbedderConfig struct {
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	ModelID  string `json:"model_id,omitempty"`
}

// StoresConfig is the `stores` section.
type StoresConfig struct {
	Lexical StoreConfig `json:"lexical"`
	Vector  StoreConfig `json:"vector"`
}

// StoreConfig is a platform store or a BYO connection.
type StoreConfig struct {
	Provider       string                `json:"provider"`
	ConnectionID   string                `json:"connection_id,omitempty"`
	Collection     string                `json:"collection,omitempty"`
	Vectors        map[string]VectorSpec `json:"vectors,omitempty"`
	PayloadMapping map[string]string     `json:"payload_mapping,omitempty"`
}

// VectorSpec describes one named vector in a BYO collection.
type VectorSpec struct {
	Name   string `json:"name"`
	Dim    int    `json:"dim"`
	Metric string `json:"metric"`
}

// Retriever names (docs/ARCHITECTURE.md §6 S2).
const (
	RetrieverLexicalBM25       = "lexical_bm25"
	RetrieverVectorText        = "vector_text"
	RetrieverVectorImage       = "vector_image"
	RetrieverVectorTextToImage = "vector_text_to_image"
	RetrieverCategoryPopular   = "category_popular"
)

// RetrieverConfig is one retriever in a query type's fan-out.
type RetrieverConfig struct {
	Name   string  `json:"name"`
	K      int     `json:"k"`
	Weight float64 `json:"weight"`
}

// QueryTypeRetrieval lists the retrievers for one query type.
type QueryTypeRetrieval struct {
	Retrievers []RetrieverConfig `json:"retrievers"`
}

// RetrievalConfig is the `retrieval` section.
type RetrievalConfig struct {
	Text        QueryTypeRetrieval `json:"text"`
	Image       QueryTypeRetrieval `json:"image"`
	RRFK        int                `json:"rrf_k"`
	FieldBoosts map[string]float64 `json:"field_boosts"`
	// EF is the HNSW search breadth for vector retrievers (docs/DATA.md §6); 0 means 128.
	EF int `json:"ef,omitempty"`
}

// FacetConfig requests counts for one facetable field; numeric fields use Ranges.
type FacetConfig struct {
	Field  string       `json:"field"`
	Size   int          `json:"size,omitempty"`
	Ranges []FacetRange `json:"ranges,omitempty"`
}

// FacetRange is a half-open numeric bucket [From, To); nil ends are open.
type FacetRange struct {
	From *float64 `json:"from,omitempty"`
	To   *float64 `json:"to,omitempty"`
}

// PolicyConfig is the `policy` section.
type PolicyConfig struct {
	AlwaysFilters []filter.Expr `json:"always_filters"`
	Rules         []PolicyRule  `json:"rules"`
}

// PolicyRule is a CEL filter or boost rule (evaluated from M7).
type PolicyRule struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Expr string `json:"expr"`
}

// Scorer types.
const (
	ScorerDefaultLinear = "default_linear"
	ScorerONNX          = "onnx"
	ScorerKServeV2      = "kserve_v2"
)

// ScorerConfig selects a scorer.
type ScorerConfig struct {
	Type    string             `json:"type"`
	ModelID string             `json:"model_id,omitempty"`
	Weights map[string]float64 `json:"weights,omitempty"`
}

// StageScorerConfig is the `prerank` and `rank` sections.
type StageScorerConfig struct {
	Scorer ScorerConfig `json:"scorer"`
	Keep   int          `json:"keep"`
}

// RerankConfig is the `rerank` section.
type RerankConfig struct {
	Personalization *PersonalizationConfig `json:"personalization,omitempty"`
	Diversity       *DiversityConfig       `json:"diversity,omitempty"`
	Model           *ScorerConfig          `json:"model,omitempty"`
}

// PersonalizationConfig weights affinity and price-band match in S6.
type PersonalizationConfig struct {
	Enabled bool    `json:"enabled"`
	Alpha   float64 `json:"alpha"`
	Beta    float64 `json:"beta"`
}

// DiversityConfig limits results per vendor within a window.
type DiversityConfig struct {
	MaxPerVendor int `json:"max_per_vendor"`
	Window       int `json:"window"`
}

// AdsConfig is the `ads` section.
type AdsConfig struct {
	Enabled        bool    `json:"enabled"`
	Slots          []int   `json:"slots"`
	RelevanceFloor float64 `json:"relevance_floor"`
	Auction        string  `json:"auction"`
}

// migrations upgrade a document from schema_version n (the key) to n+1. There are none yet;
// ParseConfig applies them in order before strict decoding.
var migrations = map[int]func(map[string]any) error{}

// ParseConfig decodes a config document strictly (unknown fields are errors), upgrading older
// schema versions first.
func ParseConfig(doc []byte) (*Config, error) {
	doc, err := migrate(doc)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse tenant config: %w", err)
	}
	return &c, nil
}

func migrate(doc []byte) ([]byte, error) {
	var head struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(doc, &head); err != nil {
		return nil, fmt.Errorf("parse tenant config: %w", err)
	}
	v := head.SchemaVersion
	if v == CurrentSchemaVersion {
		return doc, nil
	}
	if v < 1 || v > CurrentSchemaVersion {
		return nil, fmt.Errorf("tenant config schema_version %d is not supported (want 1..%d)", v, CurrentSchemaVersion)
	}
	var m map[string]any
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, fmt.Errorf("parse tenant config: %w", err)
	}
	for ; v < CurrentSchemaVersion; v++ {
		if err := migrations[v](m); err != nil {
			return nil, fmt.Errorf("migrate tenant config v%d: %w", v, err)
		}
	}
	m["schema_version"] = CurrentSchemaVersion
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("migrate tenant config: %w", err)
	}
	return out, nil
}

// EffectiveFacets returns the configured facets, or the defaults (PLAN.md R4).
func (c *Config) EffectiveFacets() []FacetConfig {
	if len(c.Facets) > 0 {
		return c.Facets
	}
	usd := func(d float64) *float64 { v := d * 100; return &v }
	return []FacetConfig{
		{Field: "category_ids", Size: 20},
		{Field: "brand", Size: 20},
		{Field: "price_minor", Ranges: []FacetRange{
			{To: usd(25)}, {From: usd(25), To: usd(50)}, {From: usd(50), To: usd(100)},
			{From: usd(100), To: usd(250)}, {From: usd(250), To: usd(500)}, {From: usd(500)},
		}},
		{Field: "ship_regions", Size: 10},
	}
}

// EffectiveEF returns the HNSW search breadth.
func (c *Config) EffectiveEF() int {
	if c.Retrieval.EF > 0 {
		return c.Retrieval.EF
	}
	return 128
}
