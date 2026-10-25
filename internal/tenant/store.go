package tenant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// ErrNotFound is returned when a tenant or its active config does not exist.
var ErrNotFound = errors.New("not found")

// Store reads and writes tenants and their config versions in MySQL.
type Store struct {
	db *sql.DB
	q  *tenantdb.Queries
}

// NewStore wraps a MySQL handle.
func NewStore(db *sql.DB) *Store { return &Store{db: db, q: tenantdb.New(db)} }

// Tenant is a tenant row.
type Tenant struct {
	ID     ID
	Slug   string
	Name   string
	Plan   string
	Status string
}

// BySlug looks a tenant up by its slug.
func (s *Store) BySlug(ctx context.Context, slug string) (Tenant, error) {
	row, err := s.q.GetTenantBySlug(ctx, slug)
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, fmt.Errorf("tenant %q: %w", slug, ErrNotFound)
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant %q: %w", slug, err)
	}
	return Tenant{ID: ID(row.ID), Slug: row.Slug, Name: row.Name, Plan: row.Plan, Status: row.Status}, nil
}

// ActiveConfig returns the tenant's active config version and its parsed document.
func (s *Store) ActiveConfig(ctx context.Context, id ID) (int, *Config, error) {
	row, err := s.q.GetActiveConfigVersion(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, fmt.Errorf("active config for tenant %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return 0, nil, fmt.Errorf("active config for tenant %s: %w", id, err)
	}
	cfg, err := ParseConfig(row.Document)
	if err != nil {
		return 0, nil, fmt.Errorf("tenant %s config v%d: %w", id, row.Version, err)
	}
	return int(row.Version), cfg, nil
}

// NewTenant is what Create needs to provision a tenant.
type NewTenant struct {
	Slug   string
	Name   string
	Plan   string
	Region string
	// Document is both the initial draft and the active version 1.
	Document json.RawMessage
}

// Create provisions a tenant in one transaction: the tenant row, its draft config, an active
// config version 1 and a completed onboarding state.
func (s *Store) Create(ctx context.Context, nt NewTenant) (ID, error) {
	if _, err := ParseConfig(nt.Document); err != nil {
		return "", err
	}
	id := NewID()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("create tenant %q: %w", nt.Slug, err)
	}
	defer func() { _ = tx.Rollback() }()
	q := s.q.WithTx(tx)
	steps := []func() error{
		func() error {
			return q.CreateTenant(ctx, tenantdb.CreateTenantParams{
				ID: string(id), Slug: nt.Slug, Name: nt.Name, Plan: nt.Plan, Region: nt.Region, Status: "active",
			})
		},
		func() error {
			return q.UpsertConfigDraft(ctx, tenantdb.UpsertConfigDraftParams{
				TenantID: string(id), Document: nt.Document, SchemaVersion: CurrentSchemaVersion,
			})
		},
		func() error {
			return q.CreateConfigVersion(ctx, tenantdb.CreateConfigVersionParams{
				ID: string(NewID()), TenantID: string(id), Version: 1, Document: nt.Document,
				SchemaVersion: CurrentSchemaVersion, State: tenantdb.TenantConfigVersionsStateActive,
			})
		},
		func() error {
			return q.CreateOnboardingState(ctx, tenantdb.CreateOnboardingStateParams{
				TenantID: string(id), CurrentStep: "done", CompletedSteps: json.RawMessage(`[]`),
			})
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return "", fmt.Errorf("create tenant %q: %w", nt.Slug, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("create tenant %q: %w", nt.Slug, err)
	}
	return id, nil
}

// Plan is a plan's limits (PLAN.md R3).
type Plan struct {
	ID              string
	Name            string
	MaxTotalMS      int
	SearchRPS       int
	SearchBurst     int
	EventsRPS       int
	BYOAllowed      bool
	EgressAllowlist []string
	MaxCustomFields int
	MaxPageDepth    int
}

// Plan loads a plan by ID.
func (s *Store) Plan(ctx context.Context, id string) (Plan, error) {
	row, err := s.q.GetPlan(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, fmt.Errorf("plan %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Plan{}, fmt.Errorf("plan %q: %w", id, err)
	}
	var allow []string
	if err := json.Unmarshal(row.EgressAllowlist, &allow); err != nil {
		return Plan{}, fmt.Errorf("plan %q egress_allowlist: %w", id, err)
	}
	return Plan{
		ID: row.ID, Name: row.Name, MaxTotalMS: int(row.MaxTotalMs), SearchRPS: int(row.SearchRps),
		SearchBurst: int(row.SearchBurst), EventsRPS: int(row.EventsRps), BYOAllowed: row.ByoAllowed,
		EgressAllowlist: allow, MaxCustomFields: int(row.MaxCustomFields), MaxPageDepth: int(row.MaxPageDepth),
	}, nil
}

// Tenant loads a tenant by ID.
func (s *Store) Tenant(ctx context.Context, id ID) (Tenant, error) {
	row, err := s.q.GetTenant(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return Tenant{}, fmt.Errorf("tenant %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant %s: %w", id, err)
	}
	return Tenant{ID: ID(row.ID), Slug: row.Slug, Name: row.Name, Plan: row.Plan, Status: row.Status}, nil
}

// APIKey is a stored tenant API key; the secret itself is never stored, only its hash.
type APIKey struct {
	ID       string
	TenantID ID
	Prefix   string
	Hash     []byte
	Scopes   []string
}

// CreateAPIKey stores a key's prefix, hash and scopes.
func (s *Store) CreateAPIKey(ctx context.Context, k APIKey) error {
	scopes, err := json.Marshal(k.Scopes)
	if err != nil {
		return fmt.Errorf("api key scopes: %w", err)
	}
	if err := s.q.CreateAPIKey(ctx, tenantdb.CreateAPIKeyParams{
		ID: k.ID, TenantID: string(k.TenantID), Prefix: k.Prefix, Hash: k.Hash, Scopes: scopes,
	}); err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	return nil
}

// APIKeyByPrefix returns the active key with this prefix.
func (s *Store) APIKeyByPrefix(ctx context.Context, prefix string) (APIKey, error) {
	row, err := s.q.GetAPIKeyByPrefix(ctx, prefix)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, fmt.Errorf("api key: %w", ErrNotFound)
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("api key: %w", err)
	}
	var scopes []string
	if err := json.Unmarshal(row.Scopes, &scopes); err != nil {
		return APIKey{}, fmt.Errorf("api key scopes: %w", err)
	}
	return APIKey{ID: row.ID, TenantID: ID(row.TenantID), Prefix: row.Prefix, Hash: row.Hash, Scopes: scopes}, nil
}

// TouchAPIKey records a key's last use.
func (s *Store) TouchAPIKey(ctx context.Context, id string) error {
	if err := s.q.TouchAPIKey(ctx, id); err != nil {
		return fmt.Errorf("touch api key: %w", err)
	}
	return nil
}

// CountActiveAPIKeys counts a tenant's unrevoked keys.
func (s *Store) CountActiveAPIKeys(ctx context.Context, id ID) (int, error) {
	n, err := s.q.CountActiveAPIKeys(ctx, string(id))
	if err != nil {
		return 0, fmt.Errorf("count api keys: %w", err)
	}
	return int(n), nil
}

// ShadowConfig returns the tenant's shadow version, its sample rate and document, or
// ErrNotFound.
func (s *Store) ShadowConfig(ctx context.Context, id ID) (int, float64, *Config, error) {
	row, err := s.q.GetShadowConfigVersion(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, nil, fmt.Errorf("shadow config for tenant %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return 0, 0, nil, fmt.Errorf("shadow config for tenant %s: %w", id, err)
	}
	cfg, err := ParseConfig(row.Document)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("tenant %s config v%d: %w", id, row.Version, err)
	}
	rate, _ := strconv.ParseFloat(row.ShadowSampleRate.String, 64)
	return int(row.Version), rate, cfg, nil
}

// ConfigVersion returns one published version's document.
func (s *Store) ConfigVersion(ctx context.Context, id ID, v int) (*Config, error) {
	row, err := s.q.GetConfigVersion(ctx, tenantdb.GetConfigVersionParams{TenantID: string(id), Version: int32(v)}) //nolint:gosec // versions are small
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("tenant %s config v%d: %w", id, v, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("tenant %s config v%d: %w", id, v, err)
	}
	return ParseConfig(row.Document)
}

// Draft returns the tenant's current draft document.
func (s *Store) Draft(ctx context.Context, id ID) (*Config, error) {
	row, err := s.q.GetConfigDraft(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("tenant %s draft: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("tenant %s draft: %w", id, err)
	}
	return ParseConfig(row.Document)
}

// LiveVersion is a tenant's active or shadow version.
type LiveVersion struct {
	Tenant  ID
	Version int
	Shadow  bool
	Rate    float64
}

// LiveVersions lists every active and shadow version (the reconcile loop's source of truth).
func (s *Store) LiveVersions(ctx context.Context) ([]LiveVersion, error) {
	rows, err := s.q.ListLiveConfigVersions(ctx)
	if err != nil {
		return nil, fmt.Errorf("live config versions: %w", err)
	}
	out := make([]LiveVersion, len(rows))
	for i, r := range rows {
		rate, _ := strconv.ParseFloat(r.ShadowSampleRate.String, 64)
		out[i] = LiveVersion{Tenant: ID(r.TenantID), Version: int(r.Version), Shadow: r.State == tenantdb.TenantConfigVersionsStateShadow, Rate: rate}
	}
	return out, nil
}
