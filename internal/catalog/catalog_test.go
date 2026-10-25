package catalog

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/zavisoft/search-engine/internal/platform"
)

// defaultMapping is the field mapping from docs/TENANT_CONFIG.md §3 plus the optional fields.
var defaultMapping = FieldMapping{
	"product_id":    "$.id",
	"title":         "$.name",
	"description":   "$.description",
	"category_path": "$.category.path",
	"brand":         "$.brand",
	"price_minor":   "$.price.amount_minor",
	"currency":      "$.price.currency",
	"image_urls":    "$.images[*].url",
	"vendor_id":     "$.vendor.id",
	"attributes":    "$.attributes",
	"stock":         "$.inventory.stock",
	"rating":        "$.reviews.rating",
	"created_at":    "$.created_at",
}

const sampleDoc = `{
  "id": "abo:B07X",
  "name": "  Red   leather handbag ",
  "description": ["Genuine leather", "Zip closure"],
  "category": {"path": ["Clothing", "Bags & Wallets", "Handbags"]},
  "brand": "Rivet",
  "price": {"amount_minor": 15999, "currency": "usd"},
  "images": [{"url": "http://s3/a.jpg"}, {"url": "http://s3/b.jpg"}],
  "vendor": {"id": "v_9"},
  "attributes": {"color": "red", "material": ["leather"], "item_keywords": ["tote", "purse"]},
  "inventory": {"stock": 0},
  "reviews": {"rating": "4.5"},
  "created_at": "2025-03-01T10:00:00Z",
  "extra": {"season": "SS25", "weight_g": 820, "eco": true}
}`

func TestMapDefaultMapping(t *testing.T) {
	m, err := NewMapper(defaultMapping, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, errs := m.Map("T1", []byte(sampleDoc))
	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if p.Title != "Red leather handbag" {
		t.Errorf("title = %q", p.Title)
	}
	if p.Description != "Genuine leather\nZip closure" {
		t.Errorf("description = %q", p.Description)
	}
	if !slices.Equal(p.ImageURLs, []string{"http://s3/a.jpg", "http://s3/b.jpg"}) {
		t.Errorf("image_urls = %v", p.ImageURLs)
	}
	if !slices.Equal(p.CategoryIDs, []string{"clothing", "bags-wallets", "handbags"}) {
		t.Errorf("category_ids = %v", p.CategoryIDs)
	}
	if p.Currency != "USD" || p.PriceMinor != 15999 || p.MOQ != 1 || p.Status != StatusActive {
		t.Errorf("defaults/normalization wrong: %+v", p)
	}
	if p.Stock == nil || *p.Stock != 0 {
		t.Errorf("stock = %v, want explicit 0", p.Stock)
	}
	if p.Rating == nil || *p.Rating != 4.5 {
		t.Errorf("rating = %v", p.Rating)
	}
	if p.CreatedAt == nil || p.CreatedAt.Year() != 2025 {
		t.Errorf("created_at = %v", p.CreatedAt)
	}
	if got := p.Attributes["color"]; !slices.Equal(got, []string{"red"}) {
		t.Errorf("attributes.color = %v", got)
	}
	if want := 5.0 / 7; p.ListingCompleteness != want {
		t.Errorf("listing_completeness = %v, want %v (product_type and ship_regions unmapped)", p.ListingCompleteness, want)
	}
	want := "Red leather handbag. Rivet. Handbags. color:red item_keywords:tote,purse material:leather"
	if got := p.EmbeddingText(); got != want {
		t.Errorf("embedding text\n got %q\nwant %q", got, want)
	}
}

func TestMapCustomFields(t *testing.T) {
	custom := []CustomField{
		{Name: "season", Type: TypeKeyword, Source: "$.extra.season", Filterable: true, Facetable: true},
		{Name: "weight_g", Type: TypeInt, Source: "$.extra.weight_g", Filterable: true, Sortable: true},
		{Name: "eco", Type: TypeBool, Source: "$.extra.eco", Filterable: true},
		{Name: "gtin", Type: TypeKeyword, Source: "$.gtin", Required: true},
	}
	m, err := NewMapper(defaultMapping, custom)
	if err != nil {
		t.Fatal(err)
	}
	p, errs := m.Map("T1", []byte(sampleDoc))
	if len(errs) != 1 || errs[0].Path != "/custom/gtin" || errs[0].Code != "missing" {
		t.Fatalf("errs = %+v, want one missing /custom/gtin", errs)
	}
	if p.Custom["season"] != "SS25" || p.Custom["weight_g"] != int64(820) || p.Custom["eco"] != true {
		t.Errorf("custom = %#v", p.Custom)
	}

	r, err := NewRegistry(custom)
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := r.Field("custom.season"); !ok || !f.Filterable || !f.Custom {
		t.Errorf("registry custom.season = %+v, %v", f, ok)
	}
	if _, ok := r.Field("season"); ok {
		t.Error("custom fields must only resolve under the custom. prefix")
	}
	if !slices.ContainsFunc(r.Filterable(), func(f Field) bool { return f.Name == "price_minor" }) {
		t.Error("price_minor should be filterable")
	}
}

func TestMapErrors(t *testing.T) {
	m, err := NewMapper(defaultMapping, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, doc, path, code string
	}{
		{"invalid json", `{"id":`, "", "invalid_json"},
		{"missing title", `{"id":"p","category":{"path":["a"]},"price":{"amount_minor":1,"currency":"USD"}}`, "/title", "missing"},
		{"fractional price", `{"id":"p","name":"x","category":{"path":["a"]},"price":{"amount_minor":15.5,"currency":"USD"}}`, "/price_minor", "type"},
		{"bad currency", `{"id":"p","name":"x","category":{"path":["a"]},"price":{"amount_minor":1,"currency":"dollars"}}`, "/currency", "invalid"},
		{"negative stock", `{"id":"p","name":"x","category":{"path":["a"]},"price":{"amount_minor":1,"currency":"USD"},"inventory":{"stock":-2}}`, "/stock", "out_of_range"},
		{"object title", `{"id":"p","name":{"en":"x"},"category":{"path":["a"]},"price":{"amount_minor":1,"currency":"USD"}}`, "/title", "type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := m.Map("T1", []byte(tc.doc))
			if !slices.ContainsFunc(errs, func(e platform.FieldError) bool { return e.Path == tc.path && e.Code == tc.code }) {
				t.Fatalf("errs = %+v, want %s %s", errs, tc.path, tc.code)
			}
		})
	}
}

func TestValidateMapping(t *testing.T) {
	errs := ValidateMapping(FieldMapping{
		"product_id":   "$.id",
		"title":        "name",
		"category_ids": "$.c",
		"colour":       "$.colour",
	})
	codes := map[string]string{}
	for _, e := range errs {
		codes[e.Path] = e.Code
	}
	want := map[string]string{
		"/catalog/field_mapping/title":         "invalid_jsonpath",
		"/catalog/field_mapping/category_ids":  "derived_field",
		"/catalog/field_mapping/colour":        "unknown_field",
		"/catalog/field_mapping/category_path": "required",
		"/catalog/field_mapping/price_minor":   "required",
		"/catalog/field_mapping/currency":      "required",
	}
	for path, code := range want {
		if codes[path] != code {
			t.Errorf("%s: code %q, want %q (all: %+v)", path, codes[path], code, errs)
		}
	}
}

func TestValidateCustomFields(t *testing.T) {
	errs := ValidateCustomFields([]CustomField{
		{Name: "Bad-Name", Type: TypeKeyword, Source: "$.a"},
		{Name: "notes", Type: TypeText, Source: "$.n", Filterable: true},
		{Name: "dup", Type: TypeInt, Source: "$.d"},
		{Name: "dup", Type: TypeInt, Source: "$.d"},
		{Name: "tags", Type: TypeKeywordArray, Source: "$.t", Sortable: true},
		{Name: "kind", Type: "enum", Source: "$.k"},
	}, 5)
	var got []string
	for _, e := range errs {
		got = append(got, e.Path+"="+e.Code)
	}
	for _, want := range []string{
		"/catalog/custom_fields=too_many",
		"/catalog/custom_fields/0/name=invalid",
		"/catalog/custom_fields/1/type=unsupported",
		"/catalog/custom_fields/3/name=duplicate",
		"/catalog/custom_fields/4/sortable=unsupported",
		"/catalog/custom_fields/5/type=invalid",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

func TestSlugAndPointID(t *testing.T) {
	for in, want := range map[string]string{
		"Bags & Wallets":     "bags-wallets",
		"  Café Crème  ":     "cafe-creme",
		"Électronique/TV 4K": "electronique-tv-4k",
		"---":                "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
	a, b := PointID("abo:B07X"), PointID("abo:B07X")
	if a != b || a == PointID("abo:B07Y") || len(a) != 36 || a[14] != '5' {
		t.Errorf("PointID not a stable UUIDv5: %s %s", a, b)
	}
}

func TestProductEvent(t *testing.T) {
	e := ProductEvent{EventID: "e1", TenantID: "T1", ProductID: "abo:1", Op: OpUpsert, SourceVersion: 3, Product: json.RawMessage(`{"id":"abo:1"}`)}
	value, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeProductEvent(e.Key(), value, e.Headers())
	if err != nil || got.ProductID != "abo:1" || got.SourceVersion != 3 {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	if string(e.Key()) != "T1:abo:1" {
		t.Errorf("key = %s", e.Key())
	}

	tomb, err := DecodeProductEvent([]byte("T1:abo:1"), nil, nil)
	if err != nil || tomb.Op != OpDelete || tomb.TenantID != "T1" || tomb.ProductID != "abo:1" {
		t.Errorf("tombstone: %+v, %v", tomb, err)
	}

	bad := map[string]func() ([]byte, []byte, map[string]string){
		"tombstone without product": func() ([]byte, []byte, map[string]string) { return []byte("T1"), nil, nil },
		"tenant header mismatch": func() ([]byte, []byte, map[string]string) {
			h := e.Headers()
			h[HeaderTenantID] = "T2"
			return e.Key(), value, h
		},
		"upsert without product": func() ([]byte, []byte, map[string]string) {
			v, _ := json.Marshal(ProductEvent{EventID: "e", TenantID: "T1", ProductID: "p", Op: OpUpsert})
			return nil, v, nil
		},
		"unknown op": func() ([]byte, []byte, map[string]string) {
			return nil, []byte(`{"tenant_id":"T1","product_id":"p","op":"merge"}`), nil
		},
		"future schema": func() ([]byte, []byte, map[string]string) {
			h := e.Headers()
			h[HeaderSchemaVersion] = "2"
			return e.Key(), value, h
		},
	}
	for name, f := range bad {
		k, v, h := f()
		if _, err := DecodeProductEvent(k, v, h); err == nil || strings.TrimSpace(err.Error()) == "" {
			t.Errorf("%s: want error", name)
		}
	}
}
