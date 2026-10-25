package tenant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
