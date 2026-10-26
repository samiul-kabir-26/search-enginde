//go:build integration

package tenant_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

func TestStoreAndResolverAgainstMySQL(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("mysql", testinfra.MySQL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := tenant.NewStore(db)

	plan, err := s.Plan(ctx, "starter")
	if err != nil || plan.MaxTotalMS != 300 || plan.SearchRPS != 20 || plan.BYOAllowed || plan.MaxPageDepth != 500 {
		t.Fatalf("seeded starter plan = %+v, %v", plan, err)
	}
	if _, err := s.Plan(ctx, "platinum"); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("unknown plan: %v", err)
	}

	id, err := s.Create(ctx, tenant.NewTenant{Slug: "acme", Name: "Acme", Plan: "growth", Region: "eu", Document: tenant.DefaultDocument()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, tenant.NewTenant{Slug: "acme", Name: "Dup", Plan: "growth", Region: "eu", Document: tenant.DefaultDocument()}); err == nil {
		t.Error("duplicate slug must fail")
	}
	if _, err := s.Create(ctx, tenant.NewTenant{Slug: "bad", Name: "Bad", Plan: "platinum", Region: "eu", Document: tenant.DefaultDocument()}); err == nil {
		t.Error("an unknown plan must violate the plans foreign key")
	}
	got, err := s.BySlug(ctx, "acme")
	if err != nil || got.ID != id || got.Status != "active" {
		t.Fatalf("BySlug = %+v, %v", got, err)
	}
	v, cfg, err := s.ActiveConfig(ctx, id)
	if err != nil || v != 1 || cfg.Embedders.Text.Model != "bge-small-en-v1.5" {
		t.Fatalf("ActiveConfig = %d %+v %v", v, cfg, err)
	}

	if n, _ := s.CountActiveAPIKeys(ctx, id); n != 0 {
		t.Errorf("new tenant has %d keys", n)
	}
	key := tenant.APIKey{ID: tenant.NewID().String(), TenantID: id, Prefix: "abcdef012345", Hash: []byte("h"), Scopes: []string{"search:read"}}
	if err := s.CreateAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	k, err := s.APIKeyByPrefix(ctx, "abcdef012345")
	if err != nil || k.TenantID != id || k.Scopes[0] != "search:read" {
		t.Fatalf("APIKeyByPrefix = %+v, %v", k, err)
	}
	if err := s.TouchAPIKey(ctx, k.ID); err != nil {
		t.Error(err)
	}
	if _, err := s.APIKeyByPrefix(ctx, "000000000000"); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("unknown key: %v", err)
	}

	r := tenant.NewResolver(s, time.Minute)
	res, err := r.Get(ctx, id)
	if err != nil || res.Plan.ID != "growth" || res.ConfigVersion != 1 || res.AlwaysFilters.IsZero() {
		t.Fatalf("resolved = %+v, %v", res, err)
	}
	if _, err := r.Get(ctx, tenant.NewID()); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("unknown tenant: %v", err)
	}
}

func TestBYORegistrationsAndConfigsAgainstMySQL(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("mysql", testinfra.MySQL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := tenant.NewStore(db)
	q := tenantdb.New(db)
	a, err := s.Create(ctx, tenant.NewTenant{Slug: "a", Name: "A", Plan: "growth", Region: "eu", Document: tenant.DefaultDocument()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Create(ctx, tenant.NewTenant{Slug: "b", Name: "B", Plan: "growth", Region: "eu", Document: tenant.DefaultDocument()})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CreateTenantModel(ctx, tenantdb.CreateTenantModelParams{
		ID: "m1", TenantID: string(a), Kind: tenantdb.TenantModelsKindEmbedder, Type: tenantdb.TenantModelsTypeKserveV2,
		Name: "clip", Version: "1", Endpoint: sql.NullString{String: "https://kserve.example.com", Valid: true},
		InputSchema: []byte(`{"modality":"image"}`), OutputSchema: []byte(`{"dim":512}`),
		Settings: []byte(`{"transport":"rest","supports_text_to_image":true}`),
		Status:   tenant.StatusRegistered,
	}); err != nil {
		t.Fatal(err)
	}
	m, err := s.Model(ctx, a, "m1")
	if err != nil || m.Status != tenant.StatusRegistered || m.Settings.Transport != "rest" || m.Endpoint != "https://kserve.example.com" {
		t.Fatalf("Model = %+v, %v", m, err)
	}
	if info := m.EmbedderInfo(); info.Dim != 512 || !info.Image || !info.TextToImage || info.TextEmbedder {
		t.Errorf("EmbedderInfo = %+v", info)
	}
	if _, err := s.Model(ctx, b, "m1"); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("another tenant's model: %v", err)
	}
	if err := q.CreateTenantConnection(ctx, tenantdb.CreateTenantConnectionParams{
		ID: "c1", TenantID: string(a), Kind: tenantdb.TenantConnectionsKindVectorStore, Type: tenant.StoreMilvus,
		Endpoint: "https://milvus.example.com", Settings: []byte(`{"username":"u","database":"d"}`),
	}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Connection(ctx, a, "c1")
	if err != nil || c.Status != tenant.StatusUntested || c.Settings.Username != "u" || c.Settings.Database != "d" {
		t.Fatalf("Connection = %+v, %v", c, err)
	}
	if _, err := s.Connection(ctx, b, "c1"); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("another tenant's connection: %v", err)
	}

	if d, err := s.Draft(ctx, a); err != nil || d.Embedders.Text.Model != "bge-small-en-v1.5" {
		t.Errorf("Draft = %+v, %v", d, err)
	}
	if c, err := s.ConfigVersion(ctx, a, 1); err != nil || c.Rank.Keep == 0 {
		t.Errorf("ConfigVersion = %+v, %v", c, err)
	}
	if _, err := s.ConfigVersion(ctx, a, 9); !errors.Is(err, tenant.ErrNotFound) {
		t.Errorf("unknown version: %v", err)
	}
	live, err := s.LiveVersions(ctx)
	if err != nil || len(live) < 2 {
		t.Errorf("LiveVersions = %+v, %v", live, err)
	}
}
