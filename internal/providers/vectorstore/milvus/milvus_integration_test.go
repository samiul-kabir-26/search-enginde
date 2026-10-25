//go:build integration

package milvus_test

import (
	"context"
	"testing"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"

	"github.com/zavisoft/search-engine/internal/providers/vectorstore/storetest"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

func kv(k, v string) *commonpb.KeyValuePair { return &commonpb.KeyValuePair{Key: k, Value: v} }

func check(t *testing.T, what string, st *commonpb.Status, err error) {
	t.Helper()
	if err != nil || st.GetCode() != 0 {
		t.Fatalf("%s: %v %v", what, err, st.GetReason())
	}
}

// createCollection is what a tenant does in its own Milvus: fixed fields for the IDs, vectors
// and the numeric/array filters, and the dynamic field for everything else.
func createCollection(t *testing.T, addr string) {
	t.Helper()
	ctx := context.Background()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	svc := milvuspb.NewMilvusServiceClient(conn)
	vec := func(name string) *schemapb.FieldSchema {
		return &schemapb.FieldSchema{Name: name, DataType: schemapb.DataType_FloatVector, TypeParams: []*commonpb.KeyValuePair{kv("dim", "4")}}
	}
	arr := func(name string) *schemapb.FieldSchema {
		return &schemapb.FieldSchema{Name: name, DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_VarChar,
			TypeParams: []*commonpb.KeyValuePair{kv("max_capacity", "16"), kv("max_length", "64")}}
	}
	sch := &schemapb.CollectionSchema{Name: "products", EnableDynamicField: true, Fields: []*schemapb.FieldSchema{
		{Name: "pid", DataType: schemapb.DataType_VarChar, IsPrimaryKey: true, TypeParams: []*commonpb.KeyValuePair{kv("max_length", "64")}},
		vec("emb_text"), vec("emb_image"),
		{Name: "price_minor", DataType: schemapb.DataType_Int64},
		{Name: "status", DataType: schemapb.DataType_VarChar, TypeParams: []*commonpb.KeyValuePair{kv("max_length", "16")}},
		{Name: "created_at", DataType: schemapb.DataType_Int64},
		arr("ship_regions"), arr("category_ids"),
	}}
	raw, err := proto.Marshal(sch)
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.CreateCollection(ctx, &milvuspb.CreateCollectionRequest{CollectionName: "products", Schema: raw, ShardsNum: 1})
	check(t, "create collection", st, err)
	for _, f := range []string{"emb_text", "emb_image"} {
		st, err = svc.CreateIndex(ctx, &milvuspb.CreateIndexRequest{CollectionName: "products", FieldName: f, IndexName: f + "_idx",
			ExtraParams: []*commonpb.KeyValuePair{kv("index_type", "HNSW"), kv("metric_type", "COSINE"), kv("params", `{"M":16,"efConstruction":64}`)}})
		check(t, "create index", st, err)
	}
	st, err = svc.LoadCollection(ctx, &milvuspb.LoadCollectionRequest{CollectionName: "products"})
	check(t, "load", st, err)
	for range 120 {
		resp, err := svc.GetLoadState(ctx, &milvuspb.GetLoadStateRequest{CollectionName: "products"})
		if err == nil && resp.GetState() == commonpb.LoadState_LoadStateLoaded {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("collection did not load")
}

func TestBYOMilvusConformance(t *testing.T) {
	addr := testinfra.Milvus(t)
	createCollection(t, addr)
	endpoint := "http://" + addr
	byo, ref := storetest.BYO(t, endpoint, "")
	conn := tenant.Connection{ID: "c1", Kind: tenant.ConnectionVectorStore, Type: tenant.StoreMilvus, Endpoint: endpoint, SecretRef: ref}
	st, err := registry.NewBYOVectorStore(context.Background(), byo, storetest.Tenant, tenant.Plan{}, conn, storetest.Block("products", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	info, err := st.Describe(context.Background())
	if err != nil || info.Vectors["emb_text"].Metric != "cosine" || !info.AnyField {
		t.Fatalf("describe = %+v, %v", info, err)
	}
	// Searches use bounded consistency, so writes become visible within Milvus's graceful time.
	storetest.Run(t, st, func() { time.Sleep(6 * time.Second) })
}
