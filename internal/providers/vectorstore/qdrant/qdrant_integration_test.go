//go:build integration

package qdrant

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

func vec(dim int, hot int) []float32 {
	v := make([]float32, dim)
	v[hot] = 1
	return v
}

func TestStoreVersionsUpsertDelete(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(testinfra.Qdrant(t))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := catalog.NewRegistry([]catalog.CustomField{{Name: "eco", Type: catalog.TypeBool, Source: "$.e", Filterable: true}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(client, tenant.NewID(), VectorDims{pipeline.VectorText: 4, pipeline.VectorImage: 3}, reg)

	if err := s.Create(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, 1); err != nil {
		t.Fatal(err)
	}
	stock := int32(2)
	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	p := catalog.Product{ProductID: "abo:1", TenantID: "T", Title: "bag", CategoryPath: []string{"Bags"}, PriceMinor: 10, Currency: "USD",
		Stock: &stock, CreatedAt: &created, ImageURLs: []string{"http://img/1.jpg"}, Custom: map[string]any{"eco": true}}
	p.Normalize()
	payload := vectorstore.Payload(p, reg)
	if payload["custom.eco"] != true || payload["created_at"] != "2025-01-02T03:04:05Z" || payload["image_url"] != "http://img/1.jpg" {
		t.Fatalf("payload = %v", payload)
	}

	points := []pipeline.VectorPoint{
		{ProductID: "abo:1", Vectors: map[string][]float32{"text": vec(4, 0), "image": vec(3, 0)}, Payload: payload},
		{ProductID: "abo:2", Vectors: map[string][]float32{"text": vec(4, 1), "image": vec(3, 1)}, Payload: map[string]any{"status": "active"}},
	}
	if err := s.Upsert(ctx, points); err != nil {
		t.Fatal(err)
	}
	if err := s.Upsert(ctx, points[:1]); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Count(ctx, 1); err != nil || n != 2 {
		t.Fatalf("count = %d, %v; want 2 (upsert is idempotent per product)", n, err)
	}
	hits, err := s.Search(ctx, pipeline.VectorSearch{Vector: "text", Query: vec(4, 1), K: 5})
	if err != nil || len(hits) != 2 || hits[0].ProductID != "abo:2" || hits[0].Score < 0.99 {
		t.Fatalf("search = %+v, %v", hits, err)
	}
	onlyActive := filter.Expr{Cond: &filter.Condition{Field: "stock", Op: filter.Gt, Value: int64(0), Type: catalog.TypeInt}}
	hits, err = s.Search(ctx, pipeline.VectorSearch{Vector: "text", Query: vec(4, 1), K: 5, Filter: onlyActive, EF: 64})
	if err != nil || len(hits) != 1 || hits[0].ProductID != "abo:1" || hits[0].Doc.PriceMinor != 10 || hits[0].Doc.ImageCount != 1 {
		t.Fatalf("filtered search must push the filter down: %+v, %v", hits, err)
	}
	got, doc, err := s.Get(ctx, "abo:1")
	if err != nil || len(got.Vectors["text"]) != 4 || len(got.Vectors["image"]) != 3 || doc.PriceMinor != 10 {
		t.Fatalf("get = %+v %+v %v", got, doc, err)
	}
	if _, _, err := s.Get(ctx, "nope"); !errors.Is(err, pipeline.ErrNotFound) {
		t.Errorf("missing product: %v", err)
	}
	var scrolled []string
	if err := s.ScrollVectors(ctx, 1, "text", func(id string, v []float32, cats []string) error {
		scrolled = append(scrolled, id)
		if len(v) != 4 {
			t.Errorf("vector dims = %d", len(v))
		}
		return nil
	}); err != nil || len(scrolled) != 2 {
		t.Errorf("scroll = %v, %v", scrolled, err)
	}
	if err := s.Delete(ctx, []string{"abo:2", "missing"}); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(ctx, 1); n != 1 {
		t.Fatalf("count after delete = %d, want 1", n)
	}

	if err := s.Create(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Active(ctx); v != 2 {
		t.Errorf("Active = %d, want 2", v)
	}
	if v, _ := s.Latest(ctx); v != 2 {
		t.Errorf("Latest = %d, want 2", v)
	}
	if err := s.Drop(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Drop(ctx, 1); err != nil {
		t.Errorf("second drop must be a no-op: %v", err)
	}
	if err := s.Health(ctx); err != nil {
		t.Error(err)
	}
}
