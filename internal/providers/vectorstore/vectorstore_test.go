package vectorstore

import (
	"strings"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
)

func TestSpecMapping(t *testing.T) {
	s := Spec{Collection: "products", Fields: map[string]string{"product_id": "pid", "custom.season": "season"},
		Vectors: map[string]VectorSpec{"text": {Name: "emb", Dim: 4, Metric: MetricCosine}, "image": {Dim: 8, Metric: MetricDot}}}
	if s.Field("product_id") != "pid" || s.Field("brand") != "brand" {
		t.Error("Field mapping")
	}
	if v, ok := s.Vector("text"); !ok || v.Name != "emb" {
		t.Error("named vector")
	}
	if v, _ := s.Vector("image"); v.Name != "image" {
		t.Error("unnamed vectors default to the pipeline name")
	}
	renamed := s.Rename(map[string]any{"product_id": "p1", "custom.season": "summer", "brand": "x"})
	if renamed["pid"] != "p1" || renamed["season"] != "summer" || renamed["brand"] != "x" {
		t.Errorf("Rename = %v", renamed)
	}
	back := s.Canonical(map[string]any{"pid": "p1", "season": "summer"}, []string{"product_id", "custom.season", "brand"})
	if back["product_id"] != "p1" || back["custom.season"] != "summer" || len(back) != 2 {
		t.Errorf("Canonical = %v", back)
	}
}

func TestDocFromMapAcceptsDriverTypes(t *testing.T) {
	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, m := range []map[string]any{
		{"product_id": "p1", "vendor_id": "v1", "price_minor": int64(999), "rating": 4.5, "rating_count": int32(7),
			"listing_completeness": float32(0.5), "image_count": int64(2), "category_ids": []any{"a", "b"}, "created_at": created},
		{"product_id": "p1", "vendor_id": "v1", "price_minor": 999.0, "rating": float32(4.5), "rating_count": 7.0,
			"listing_completeness": 0.5, "image_count": 2.0, "category_ids": []string{"a", "b"}, "created_at": created.Format(time.RFC3339)},
		{"product_id": "p1", "vendor_id": "v1", "price_minor": int64(999), "rating": 4.5, "rating_count": int64(7),
			"listing_completeness": 0.5, "image_count": int64(2), "category_ids": []string{"a", "b"}, "created_at": created.Unix()},
	} {
		id, d := DocFromMap(m)
		if id != "p1" || d.VendorID != "v1" || d.PriceMinor != 999 || *d.Rating != 4.5 || d.RatingCount != 7 ||
			d.ListingCompleteness != 0.5 || d.ImageCount != 2 || len(d.CategoryIDs) != 2 || !d.CreatedAt.Equal(created) {
			t.Errorf("DocFromMap(%v) = %s %+v", m, id, d)
		}
	}
	if _, d := DocFromMap(map[string]any{}); d.Rating != nil || d.CreatedAt != nil {
		t.Error("absent optional fields stay nil")
	}
}

func TestPayloadIncludesFilterableCustomFields(t *testing.T) {
	reg, err := catalog.NewRegistry([]catalog.CustomField{{Name: "season", Type: catalog.TypeKeyword, Source: "$.s", Filterable: true}})
	if err != nil {
		t.Fatal(err)
	}
	p := catalog.Product{ProductID: "p", Title: "T", VendorID: "v", PriceMinor: 1, Currency: "USD", Custom: map[string]any{"season": "summer"}}
	if Payload(p, reg)["custom.season"] != "summer" {
		t.Error("filterable custom fields are stored")
	}
}

func TestCheck(t *testing.T) {
	spec := Spec{Collection: "c", Fields: map[string]string{"product_id": "pid"},
		Vectors: map[string]VectorSpec{"text": {Name: "t", Dim: 4, Metric: MetricCosine}, "image": {Name: "i", Dim: 8, Metric: "hamming"}}}
	metrics := []string{MetricCosine, MetricDot, MetricL2}
	if p := Check(spec, Info{}, metrics, nil); len(p) != 1 || p[0].Code != "collection_missing" {
		t.Fatalf("missing collection: %+v", p)
	}
	info := Info{Exists: true, Vectors: map[string]VectorInfo{"t": {Dim: 3, Metric: MetricDot}}, Fields: map[string]bool{"pid": true, "brand": true}}
	codes := map[string]bool{}
	for _, p := range Check(spec, info, metrics, []string{"brand", "status"}) {
		codes[p.Path+":"+p.Code] = true
	}
	for _, want := range []string{"/vectors/text/dim:dim_mismatch", "/vectors/image/metric:unsupported_metric", "/vectors/image/name:vector_missing", "/payload_mapping/status:field_missing"} {
		if !codes[want] {
			t.Errorf("missing problem %s in %v", want, codes)
		}
	}
	if codes["/payload_mapping/brand:field_missing"] || codes["/payload_mapping/product_id:field_missing"] {
		t.Errorf("present fields reported missing: %v", codes)
	}
	info.Vectors["t"] = VectorInfo{Dim: 4, Metric: MetricDot}
	found := false
	for _, p := range Check(spec, info, metrics, nil) {
		found = found || (p.Code == "metric_mismatch" && strings.Contains(p.Message, "uses dot"))
	}
	if !found {
		t.Error("a metric mismatch is reported")
	}
	info.AnyField = true
	for _, p := range Check(spec, info, metrics, []string{"anything"}) {
		if p.Code == "field_missing" {
			t.Error("schemaless stores accept any field")
		}
	}
}
