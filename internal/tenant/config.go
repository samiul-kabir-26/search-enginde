package tenant

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/zavisoft/search-engine/internal/catalog"
)

// CurrentSchemaVersion is the config document schema this build reads and writes.
const CurrentSchemaVersion = 1

//go:embed default_config.json
var defaultDocument []byte

// DefaultDocument returns the default config document for new tenants (docs/TENANT_CONFIG.md §3).
func DefaultDocument() json.RawMessage { return bytes.Clone(defaultDocument) }

// Config is a tenant pipeline config document. Sections the engine does not consume yet are
// kept raw so documents round-trip unchanged; they get typed in the milestone that uses them.
type Config struct {
	SchemaVersion int             `json:"schema_version"`
	Locale        string          `json:"locale"`
	Budgets       json.RawMessage `json:"budgets"`
	Catalog       CatalogConfig   `json:"catalog"`
	Embedders     EmbeddersConfig `json:"embedders"`
	Stores        StoresConfig    `json:"stores"`
	Retrieval     json.RawMessage `json:"retrieval"`
	Policy        json.RawMessage `json:"policy"`
	Prerank       json.RawMessage `json:"prerank"`
	Rank          json.RawMessage `json:"rank"`
	Rerank        json.RawMessage `json:"rerank"`
	Ads           json.RawMessage `json:"ads"`
}

// CatalogConfig is the `catalog` section.
type CatalogConfig struct {
	Ingestion    string                `json:"ingestion"`
	FieldMapping catalog.FieldMapping  `json:"field_mapping"`
	CustomFields []catalog.CustomField `json:"custom_fields,omitempty"`
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

// ParseConfig decodes a config document strictly: unknown fields are errors.
func ParseConfig(doc []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse tenant config: %w", err)
	}
	if c.SchemaVersion != CurrentSchemaVersion {
		return nil, fmt.Errorf("tenant config schema_version %d is not supported (want %d)", c.SchemaVersion, CurrentSchemaVersion)
	}
	return &c, nil
}
