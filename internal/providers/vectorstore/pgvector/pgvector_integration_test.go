//go:build integration

package pgvector_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zavisoft/search-engine/internal/providers/vectorstore/storetest"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

const schema = `
CREATE TABLE catalog.products (
  pid text PRIMARY KEY, emb_text vector(4), emb_image vector(4),
  vendor_id text, brand text, price_minor bigint, currency text, status text, ship_regions text[], category_ids text[],
  rating double precision, rating_count bigint, stock bigint, listing_completeness double precision, image_count integer,
  created_at timestamptz, season text);
CREATE INDEX ON catalog.products USING hnsw (emb_text vector_cosine_ops);
CREATE INDEX ON catalog.products USING hnsw (emb_image vector_cosine_ops);`

func TestBYOPgvectorConformance(t *testing.T) {
	dsn := testinfra.Pgvector(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA catalog;"+schema); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)

	// The password lives in a secret; the endpoint carries only the user.
	endpoint := strings.Replace(dsn, "search:search@", "search@", 1)
	byo, ref := storetest.BYO(t, endpoint, "search")
	conn := tenant.Connection{ID: "c1", Kind: tenant.ConnectionVectorStore, Type: tenant.StorePgvector, Endpoint: endpoint, SecretRef: ref}
	st, err := registry.NewBYOVectorStore(ctx, byo, storetest.Tenant, tenant.Plan{}, conn, storetest.Block("catalog.products", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.Describe(ctx)
	if err != nil || info.Vectors["emb_text"].Metric != "cosine" || !info.Fields["season"] {
		t.Fatalf("describe = %+v, %v", info, err)
	}
	storetest.Run(t, st, func() {})

}
