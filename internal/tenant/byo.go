package tenant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// Statuses of registered models and connections. A model or connection must be validated
// before a config version can reference it (docs/TENANT_CONFIG.md §5 rule 5).
const (
	StatusRegistered = "registered"
	StatusUntested   = "untested"
	StatusValidated  = "validated"
	StatusFailed     = "failed"
)

// BYO model types.
const (
	ModelKServeV2         = "kserve_v2"
	ModelOpenAIEmbeddings = "openai_embeddings"
	ModelONNX             = "onnx"
)

// Embedder modalities.
const (
	ModalityText  = "text"
	ModalityImage = "image"
)

// Model is a registered BYO model (tenant_models).
type Model struct {
	ID        string
	Kind      string
	Type      string
	Name      string
	Version   string
	Endpoint  string
	SecretRef string
	Status    string
	// Input and Output are the declared schemas; for embedders, the modality and dimension.
	Input    ModelInput
	Output   ModelOutput
	Settings ModelSettings
}

// ModelInput is an embedder's declared input schema.
type ModelInput struct {
	Modality string `json:"modality"`
}

// ModelOutput is an embedder's declared output schema.
type ModelOutput struct {
	Dim int `json:"dim"`
}

// ModelSettings are a model's options and what validation detected.
type ModelSettings struct {
	// Transport is grpc or rest for kserve_v2, detected by validation.
	Transport           string `json:"transport,omitempty"`
	MaxBatchSize        int    `json:"max_batch_size,omitempty"`
	SupportsTextToImage bool   `json:"supports_text_to_image,omitempty"`
}

// EmbedderInfo describes what a BYO embedder can do.
func (m Model) EmbedderInfo() EmbedderInfo {
	img := m.Input.Modality == ModalityImage
	return EmbedderInfo{Dim: m.Output.Dim, Image: img, TextEmbedder: !img, TextToImage: img && m.Settings.SupportsTextToImage}
}

func decodeLoose(raw json.RawMessage, dst any) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// ModelFromRow converts a tenant_models row.
func ModelFromRow(r tenantdb.TenantModel) (Model, error) {
	m := Model{ID: r.ID, Kind: string(r.Kind), Type: string(r.Type), Name: r.Name, Version: r.Version,
		Endpoint: r.Endpoint.String, SecretRef: r.SecretRef.String, Status: r.Status}
	if err := decodeLoose(r.InputSchema, &m.Input); err != nil {
		return m, fmt.Errorf("model %s input_schema: %w", r.ID, err)
	}
	if err := decodeLoose(r.OutputSchema, &m.Output); err != nil {
		return m, fmt.Errorf("model %s output_schema: %w", r.ID, err)
	}
	if err := decodeLoose(r.Settings, &m.Settings); err != nil {
		return m, fmt.Errorf("model %s settings: %w", r.ID, err)
	}
	return m, nil
}

// Connection is a registered BYO store connection (tenant_connections).
type Connection struct {
	ID        string
	Kind      string
	Type      string
	Endpoint  string
	SecretRef string
	Status    string
	Settings  ConnectionSettings
}

// Connection kinds and BYO vector store types.
const (
	ConnectionVectorStore = "vector_store"
	ConnectionLexical     = "lexical"
	StoreQdrant           = "qdrant"
	StorePgvector         = "pgvector"
	StoreMilvus           = "milvus"
)

// ConnectionSettings are non-secret connection options.
type ConnectionSettings struct {
	// Username, for Milvus user/password auth (the password is the secret).
	Username string `json:"username,omitempty"`
	// Database, for Milvus.
	Database string `json:"database,omitempty"`
}

// ConnectionFromRow converts a tenant_connections row.
func ConnectionFromRow(r tenantdb.TenantConnection) (Connection, error) {
	c := Connection{ID: r.ID, Kind: string(r.Kind), Type: r.Type, Endpoint: r.Endpoint, SecretRef: r.SecretRef.String, Status: r.Status}
	if err := decodeLoose(r.Settings, &c.Settings); err != nil {
		return c, fmt.Errorf("connection %s settings: %w", r.ID, err)
	}
	return c, nil
}

// Model loads one of the tenant's registered models; another tenant's model is ErrNotFound.
func (s *Store) Model(ctx context.Context, id ID, modelID string) (Model, error) {
	row, err := s.q.GetTenantModel(ctx, tenantdb.GetTenantModelParams{TenantID: string(id), ID: modelID})
	if errors.Is(err, sql.ErrNoRows) {
		return Model{}, fmt.Errorf("model %s: %w", modelID, ErrNotFound)
	}
	if err != nil {
		return Model{}, fmt.Errorf("model %s: %w", modelID, err)
	}
	return ModelFromRow(row)
}

// Connection loads one of the tenant's connections; another tenant's is ErrNotFound.
func (s *Store) Connection(ctx context.Context, id ID, connID string) (Connection, error) {
	row, err := s.q.GetTenantConnection(ctx, tenantdb.GetTenantConnectionParams{TenantID: string(id), ID: connID})
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, fmt.Errorf("connection %s: %w", connID, ErrNotFound)
	}
	if err != nil {
		return Connection{}, fmt.Errorf("connection %s: %w", connID, err)
	}
	return ConnectionFromRow(row)
}

// BYOChecker resolves a tenant's registered BYO models and connections for validation rules
// 3–6, probing the endpoints live. Returned field errors have paths relative to the config
// block being checked.
type BYOChecker interface {
	// Embedder checks that a model exists, belongs to the tenant, is a validated embedder, and
	// answers a live probe; it returns the model's capabilities.
	Embedder(ctx context.Context, modelID string) (EmbedderInfo, []platform.FieldError)
	// VectorStore checks a BYO vector store block against its connection and the live
	// collection: dimensions and metrics as declared and supported, and the fields filters must
	// be pushed down on (filterFields, canonical names) present.
	VectorStore(ctx context.Context, block StoreConfig, filterFields []string) []platform.FieldError
}
