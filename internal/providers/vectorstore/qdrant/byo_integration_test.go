//go:build integration

package qdrant_test

import (
	"context"
	"testing"

	qdrantgo "github.com/qdrant/go-client/qdrant"

	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/storetest"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

func TestBYOQdrantConformance(t *testing.T) {
	addr := testinfra.Qdrant(t)
	admin, err := qdrant.NewClient(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	ctx := context.Background()
	err = admin.CreateCollection(ctx, &qdrantgo.CreateCollection{CollectionName: "tenant_products", VectorsConfig: qdrantgo.NewVectorsConfigMap(map[string]*qdrantgo.VectorParams{
		"emb_text": {Size: storetest.Dim, Distance: qdrantgo.Distance_Cosine}, "emb_image": {Size: storetest.Dim, Distance: qdrantgo.Distance_Cosine},
	})})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + addr
	byo, ref := storetest.BYO(t, endpoint, "")
	conn := tenant.Connection{ID: "c1", Kind: tenant.ConnectionVectorStore, Type: tenant.StoreQdrant, Endpoint: endpoint, SecretRef: ref}
	st, err := registry.NewBYOVectorStore(ctx, byo, storetest.Tenant, tenant.Plan{}, conn, storetest.Block("tenant_products", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.ProviderName() != "byo:qdrant" {
		t.Errorf("name = %s", st.ProviderName())
	}
	storetest.Run(t, st, func() {})
}
