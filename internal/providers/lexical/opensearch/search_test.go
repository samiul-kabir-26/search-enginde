package opensearch

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/tenant"
)

func cond(field string, op filter.Op, v any, t catalog.FieldType) filter.Expr {
	return filter.Expr{Cond: &filter.Condition{Field: field, Op: op, Value: v, Type: t}}
}

func js(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTranslate(t *testing.T) {
	ts := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		e    filter.Expr
		want string
	}{
		{cond("brand", filter.Eq, "Rivet", catalog.TypeKeyword), `{"term":{"brand":"Rivet"}}`},
		{cond("brand", filter.Ne, "Rivet", catalog.TypeKeyword), `{"bool":{"must_not":[{"term":{"brand":"Rivet"}}]}}`},
		{cond("brand", filter.In, []any{"a", "b"}, catalog.TypeKeyword), `{"terms":{"brand":["a","b"]}}`},
		{cond("brand", filter.Nin, []any{"a"}, catalog.TypeKeyword), `{"bool":{"must_not":[{"terms":{"brand":["a"]}}]}}`},
		{cond("ship_regions", filter.Contains, "US", catalog.TypeKeywordArray), `{"term":{"ship_regions":"US"}}`},
		{cond("stock", filter.Gt, int64(0), catalog.TypeInt), `{"range":{"stock":{"gt":0}}}`},
		{cond("created_at", filter.Gte, ts, catalog.TypeDate), `{"range":{"created_at":{"gte":"2025-01-02T00:00:00Z"}}}`},
		{cond("price_minor", filter.Range, filter.Bounds{Gte: int64(1), Lt: int64(9)}, catalog.TypeInt), `{"range":{"price_minor":{"gte":1,"lt":9}}}`},
		{filter.Expr{Or: []filter.Expr{cond("moq", filter.Eq, int64(1), catalog.TypeInt), cond("custom.eco", filter.Eq, true, catalog.TypeBool)}},
			`{"bool":{"minimum_should_match":1,"should":[{"term":{"moq":1}},{"term":{"custom.eco":true}}]}}`},
	}
	for _, tc := range cases {
		got, err := Translate(tc.e)
		if err != nil {
			t.Fatal(err)
		}
		if g := js(t, got); g != tc.want {
			t.Errorf("got  %s\nwant %s", g, tc.want)
		}
	}
	if got, _ := Translate(filter.Expr{}); got != nil {
		t.Error("empty filter must translate to nil")
	}
}

func TestSearchBodyDisjunctiveFacets(t *testing.T) {
	reg, _ := catalog.NewRegistry([]catalog.CustomField{{Name: "season", Type: catalog.TypeKeyword, Source: "$.s", Searchable: true}})
	s := New(nil, tenant.ID("01JTENANT0000000000000000A"), reg)
	to := 2500.0
	body, err := s.searchBody(pipeline.LexicalSearch{
		Text:    "red bag",
		Boosts:  map[string]float64{"title": 3, "brand": 2, "attributes": 1.5, "custom.season": 1},
		Base:    cond("status", filter.Eq, "active", catalog.TypeKeyword),
		Request: filter.All(cond("brand", filter.Eq, "Rivet", catalog.TypeKeyword), cond("stock", filter.Gt, int64(0), catalog.TypeInt)),
		K:       10,
		Facets:  []pipeline.FacetRequest{{Field: "brand", Size: 5}, {Field: "price_minor", Ranges: []pipeline.FacetRange{{To: &to}, {From: &to}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var b map[string]any
	_ = json.Unmarshal(body, &b)
	q := js(t, b["query"])
	for _, want := range []string{`"attributes.*^1.5"`, `"brand.text^2"`, `"custom.season.text^1"`, `"title^3"`, `{"term":{"status":"active"}}`} {
		if !strings.Contains(q, want) {
			t.Errorf("query lacks %s: %s", want, q)
		}
	}
	if pf := js(t, b["post_filter"]); !strings.Contains(pf, `"brand":"Rivet"`) {
		t.Errorf("request filters must be a post_filter: %s", pf)
	}
	aggs := b["aggs"].(map[string]any)
	brand := js(t, aggs["facet_brand"])
	if strings.Contains(brand, "Rivet") || !strings.Contains(brand, `"stock"`) {
		t.Errorf("brand facet must ignore the brand condition but keep the others: %s", brand)
	}
	price := js(t, aggs["facet_price_minor"])
	if !strings.Contains(price, "Rivet") || !strings.Contains(price, `"key":"-2500"`) || !strings.Contains(price, `"key":"2500-"`) {
		t.Errorf("price facet: %s", price)
	}
}

func TestFacetValues(t *testing.T) {
	var a facetAgg
	_ = json.Unmarshal([]byte(`{"values":{"buckets":[{"key":"Rivet","doc_count":4},{"key":"Solimo","doc_count":2}]}}`), &a)
	v := a.values(pipeline.FacetRequest{Field: "brand"})
	if len(v) != 2 || v[0].Value != "Rivet" || v[0].Count != 4 {
		t.Errorf("terms buckets = %+v", v)
	}
	to := 25.0
	_ = json.Unmarshal([]byte(`{"values":{"buckets":[{"key":"-25","to":25,"doc_count":7}]}}`), &a)
	v = a.values(pipeline.FacetRequest{Field: "price_minor", Ranges: []pipeline.FacetRange{{To: &to}}})
	if v[0].Value != "-25" || *v[0].To != 25 || v[0].Count != 7 {
		t.Errorf("range buckets = %+v", v)
	}
}
