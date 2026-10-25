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
