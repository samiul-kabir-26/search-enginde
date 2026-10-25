//go:build integration

package opensearch

import (
	"context"
	"slices"
	"testing"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

func product(id, title string) catalog.Product {
	stock := int32(3)
	p := catalog.Product{
		ProductID: id, TenantID: "T", Title: title, CategoryPath: []string{"Bags", "Handbags"},
		PriceMinor: 1000, Currency: "USD", Stock: &stock,
		Attributes: map[string][]string{"Color": {"red"}, "item.keywords": {"tote"}},
		Custom:     map[string]any{"season": "SS25"},
	}
	p.Normalize()
	return p
}

func TestStoreVersionsAndOrdering(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(testinfra.OpenSearch(t))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := catalog.NewRegistry([]catalog.CustomField{{Name: "season", Type: catalog.TypeKeyword, Source: "$.s", Filterable: true}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(client, tenant.NewID(), reg)

	if v, err := s.Active(ctx); err != nil || v != 0 {
		t.Fatalf("Active on empty = %d, %v", v, err)
	}
	if err := s.Create(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, 1); err != nil {
		t.Fatal(err)
	}

	stale, err := s.Upsert(ctx, []pipeline.IndexDoc{
		{Product: product("a", "red bag v5"), SourceVersion: 5},
		{Product: product("b", "blue bag"), SourceVersion: 1},
		{Product: product("c", "unversioned"), SourceVersion: 0},
	})
	if err != nil || len(stale) != 0 {
		t.Fatalf("first upsert: stale=%v err=%v", stale, err)
	}
	stale, err = s.Upsert(ctx, []pipeline.IndexDoc{
		{Product: product("a", "red bag v4"), SourceVersion: 4},
		{Product: product("b", "blue bag replay"), SourceVersion: 1},
	})
	if err != nil || !slices.Equal(stale, []string{"a"}) {
		t.Fatalf("older version must be stale and equal version must apply: stale=%v err=%v", stale, err)
	}
	stale, err = s.Delete(ctx, []pipeline.IndexDelete{{ProductID: "a", SourceVersion: 3}, {ProductID: "b", SourceVersion: 2}, {ProductID: "missing"}})
	if err != nil || !slices.Equal(stale, []string{"a"}) {
		t.Fatalf("delete: stale=%v err=%v", stale, err)
	}
	if n, err := s.Count(ctx, 1); err != nil || n != 2 {
		t.Fatalf("count v1 = %d, %v; want 2 (a, c)", n, err)
	}

	if err := s.Create(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Version(2).Upsert(ctx, []pipeline.IndexDoc{{Product: product("z", "new"), SourceVersion: 1}}); err != nil {
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
