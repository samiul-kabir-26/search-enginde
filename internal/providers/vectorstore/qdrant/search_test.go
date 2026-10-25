package qdrant

import (
	"strings"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
)

func cond(field string, op filter.Op, v any, t catalog.FieldType) filter.Expr {
	return filter.Expr{Cond: &filter.Condition{Field: field, Op: op, Value: v, Type: t}}
}

func TestTranslate(t *testing.T) {
	ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	e := filter.All(
		cond("brand", filter.Eq, "Rivet", catalog.TypeKeyword),
		cond("brand", filter.Nin, []any{"Acme"}, catalog.TypeKeyword),
		cond("moq", filter.In, []any{int64(1), int64(10)}, catalog.TypeInt),
		cond("rating", filter.Eq, 4.5, catalog.TypeFloat),
		cond("stock", filter.Gt, int64(0), catalog.TypeInt),
		cond("created_at", filter.Range, filter.Bounds{Gte: ts}, catalog.TypeDate),
		cond("custom.eco", filter.Ne, true, catalog.TypeBool),
		filter.Expr{Or: []filter.Expr{cond("ship_regions", filter.Contains, "US", catalog.TypeKeywordArray), cond("status", filter.Eq, "active", catalog.TypeKeyword)}},
	)
	f, err := Translate(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.GetMust()) != 8 {
		t.Fatalf("want 8 must conditions, got %d: %v", len(f.GetMust()), f)
	}
	s := f.String()
	for _, want := range []string{`key:"brand"`, `keyword:"Rivet"`, `key:"moq"`, `key:"stock"`, `gt:0`, `key:"created_at"`, `key:"custom.eco"`, `should:`, `must_not:`} {
		if !strings.Contains(s, want) {
			t.Errorf("filter lacks %s:\n%s", want, s)
		}
	}
	if f, _ := Translate(filter.Expr{}); f != nil {
		t.Error("empty filter must translate to nil")
	}
	if _, err := Translate(cond("brand", filter.Range, filter.Bounds{Gt: "a"}, catalog.TypeKeyword)); err == nil {
		t.Error("range on a keyword must fail")
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	stock, rating := int32(2), 4.2
	created := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	p := catalog.Product{ProductID: "abo:1", TenantID: "T", Title: "Bag", Brand: "Rivet", CategoryPath: []string{"Bags"},
		PriceMinor: 999, Currency: "USD", Stock: &stock, Rating: &rating, RatingCount: 12, CreatedAt: &created,
		ImageURLs: []string{"http://a", "http://b"}}
	p.Normalize()
	reg, _ := catalog.NewRegistry(nil)
	payload := vectorstore.Payload(p, reg)
	payload["product_id"] = p.ProductID
	vm, err := toValueMap(payload)
	if err != nil {
		t.Fatal(err)
	}
	id, d := docFromPayload(vm, vectorstore.Spec{})
	if id != "abo:1" || d.Title != "Bag" || d.PriceMinor != 999 || d.RatingCount != 12 || d.ImageCount != 2 ||
		d.ImageURL != "http://a" || *d.Rating != 4.2 || !d.CreatedAt.Equal(created) || d.CategoryIDs[0] != "bags" {
		t.Errorf("round trip lost data: %s %+v", id, d)
	}
}
