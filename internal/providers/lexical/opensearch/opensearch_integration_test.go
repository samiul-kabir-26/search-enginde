//go:build integration

package opensearch

import (
	"context"
	"slices"
	"testing"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
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

func TestSearchFacetsAndDocs(t *testing.T) {
	ctx := context.Background()
	client, err := NewClient(testinfra.OpenSearch(t))
	if err != nil {
		t.Fatal(err)
	}
	reg, _ := catalog.NewRegistry(nil)
	s := New(client, tenant.NewID(), reg)
	if err := s.Create(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Promote(ctx, 1); err != nil {
		t.Fatal(err)
	}
	mk := func(id, title, brand string, price int64, stock int32, status string) pipeline.IndexDoc {
		p := catalog.Product{ProductID: id, TenantID: "T", Title: title, Brand: brand, CategoryPath: []string{"Bags"},
			PriceMinor: price, Currency: "USD", Stock: &stock, Status: status, ImageURLs: []string{"http://img/" + id}}
		p.Normalize()
		return pipeline.IndexDoc{Product: p, SourceVersion: 1}
	}
	if _, err := s.Upsert(ctx, []pipeline.IndexDoc{
		mk("a", "Red leather handbag", "Rivet", 4500, 3, "active"),
		mk("b", "Red leather tote", "Solimo", 9000, 2, "active"),
		mk("c", "Red leather wallet", "Rivet", 1500, 0, "active"),
		mk("d", "Red leather handbag", "Rivet", 4000, 5, "inactive"),
		mk("e", "Blue canvas bag", "Solimo", 2000, 5, "active"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Count(ctx, 1); err != nil {
		t.Fatal(err)
	}
	reqF := func(field string, op filter.Op, v any, typ catalog.FieldType) filter.Expr {
		return filter.Expr{Cond: &filter.Condition{Field: field, Op: op, Value: v, Type: typ}}
	}
	base := filter.All(reqF("status", filter.Eq, "active", catalog.TypeKeyword), reqF("stock", filter.Gt, int64(0), catalog.TypeInt))
	to := 5000.0
	res, err := s.Search(ctx, pipeline.LexicalSearch{
		Text: "red leather", Base: base, Request: reqF("brand", filter.Eq, "Rivet", catalog.TypeKeyword), K: 10,
		Facets: []pipeline.FacetRequest{{Field: "brand", Size: 5}, {Field: "price_minor", Ranges: []pipeline.FacetRange{{To: &to}, {From: &to}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].ProductID != "a" || res.Hits[0].Doc.PriceMinor != 4500 {
		t.Fatalf("hits = %+v (base and request filters must both apply)", res.Hits)
	}
	brands := map[string]int64{}
	for _, v := range res.Facets["brand"] {
		brands[v.Value] = v.Count
	}
	if brands["Rivet"] != 1 || brands["Solimo"] != 1 {
		t.Errorf("brand facet must ignore the brand filter: %v", res.Facets["brand"])
	}
	if p := res.Facets["price_minor"]; len(p) != 2 || p[0].Count != 1 || p[1].Count != 0 {
		t.Errorf("price facet (filtered by brand) = %+v", p)
	}
	if res.TotalMatches != 1 {
		t.Errorf("total matches = %d, want 1 (hits.total counts after the post-filter)", res.TotalMatches)
	}

	pop, err := s.Search(ctx, pipeline.LexicalSearch{Base: base, K: 10, Popular: true})
	if err != nil || len(pop.Hits) != 3 {
		t.Fatalf("popular = %+v, %v", pop.Hits, err)
	}
	docs, err := s.Docs(ctx, []string{"a", "missing"})
	if err != nil || len(docs) != 1 || docs["a"].Title != "Red leather handbag" || docs["a"].ImageURL != "http://img/a" {
		t.Errorf("docs = %+v, %v", docs, err)
	}
}
