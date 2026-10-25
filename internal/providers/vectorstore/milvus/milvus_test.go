package milvus

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
)

func schema(dynamic bool) *schemapb.CollectionSchema {
	return &schemapb.CollectionSchema{EnableDynamicField: dynamic, Fields: []*schemapb.FieldSchema{
		{Name: "pid", DataType: schemapb.DataType_VarChar, IsPrimaryKey: true},
		{Name: "brand", DataType: schemapb.DataType_VarChar, Nullable: true},
		{Name: "price_minor", DataType: schemapb.DataType_Int64},
		{Name: "rating", DataType: schemapb.DataType_Double},
		{Name: "created_at", DataType: schemapb.DataType_Int64},
		{Name: "category_ids", DataType: schemapb.DataType_Array, ElementType: schemapb.DataType_VarChar},
		{Name: "text", DataType: schemapb.DataType_FloatVector, TypeParams: []*commonpb.KeyValuePair{{Key: "dim", Value: "2"}}},
	}}
}

func cond(field string, op filter.Op, v any, t catalog.FieldType) filter.Expr {
	return filter.Expr{Cond: &filter.Condition{Field: field, Op: op, Value: v, Type: t}}
}

func TestTranslate(t *testing.T) {
	spec := vectorstore.Spec{Collection: "c", Fields: map[string]string{"product_id": "pid"}}
	at := time.Unix(1700000000, 0).UTC()
	e := filter.Expr{And: []filter.Expr{
		cond("brand", filter.Eq, `Ri"vet`, catalog.TypeKeyword),
		cond("category_ids", filter.In, []any{"a", "b"}, catalog.TypeKeywordArray),
		{Or: []filter.Expr{
			cond("price_minor", filter.Range, filter.Bounds{Gt: int64(1), Lte: int64(9)}, catalog.TypeInt),
			cond("rating", filter.Gte, 4.5, catalog.TypeFloat),
		}},
		cond("product_id", filter.Ne, "p1", catalog.TypeKeyword),
		cond("created_at", filter.Lt, at, catalog.TypeDate),
		cond("category_ids", filter.Contains, "x", catalog.TypeKeywordArray),
	}}
	got, err := translate(e, spec, schema(false))
	if err != nil {
		t.Fatal(err)
	}
	want := `(brand == "Ri\"vet") and (array_contains_any(category_ids, ["a", "b"])) and ((price_minor > 1 and price_minor <= 9) or (rating >= 4.5)) and (not (pid == "p1")) and (created_at < 1700000000) and (array_contains(category_ids, "x"))`
	if got != want {
		t.Errorf("expr =\n%s\nwant\n%s", got, want)
	}
	if _, err := translate(cond("stock", filter.Gt, int64(0), catalog.TypeInt), spec, schema(false)); err == nil || !strings.Contains(err.Error(), "pushed down") {
		t.Errorf("an unknown field must fail without a dynamic field: %v", err)
	}
	if got, err := translate(cond("stock", filter.Nin, []any{int64(0)}, catalog.TypeInt), spec, schema(true)); err != nil || got != "not (stock in [0])" {
		t.Errorf("dynamic fields can be filtered: %q %v", got, err)
	}
}

func TestColumnsRoundTrip(t *testing.T) {
	sch := schema(true)
	rows := []map[string]any{
		{"pid": "p1", "brand": "Rivet", "price_minor": int64(100), "rating": 4.5, "created_at": "2023-11-14T22:13:20Z", "category_ids": []string{"a"}, "stock": int64(3)},
		{"pid": "p2", "price_minor": 200.0, "category_ids": []any{"b", "c"}},
	}
	vectors := []map[string][]float32{{"text": {1, 0}}, {}}
	var data []*schemapb.FieldData
	for _, f := range sch.GetFields() {
		fd, err := column(f, rows, vectors)
		if err != nil {
			t.Fatalf("%s: %v", f.GetName(), err)
		}
		data = append(data, fd)
	}
	got, err := columns(data, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got[0]["pid"] != "p1" || got[0]["brand"] != "Rivet" || got[0]["price_minor"] != int64(100) || got[0]["created_at"] != int64(1700000000) {
		t.Errorf("row 0 = %v", got[0])
	}
	if _, ok := got[1]["brand"]; ok {
		t.Errorf("a nullable field without a value is null: %v", got[1])
	}
	if got[1]["price_minor"] != int64(200) || !reflect.DeepEqual(got[1]["category_ids"], []string{"b", "c"}) {
		t.Errorf("row 1 = %v", got[1])
	}
	if !reflect.DeepEqual(got[0]["text"], []float32{1, 0}) || !reflect.DeepEqual(got[1]["text"], []float32{0, 0}) {
		t.Errorf("vectors = %v / %v (missing vectors are zero)", got[0]["text"], got[1]["text"])
	}
	dyn := &schemapb.FieldData{Type: schemapb.DataType_JSON, FieldName: "$meta", IsDynamic: true,
		Field: &schemapb.FieldData_Scalars{Scalars: &schemapb.ScalarField{Data: &schemapb.ScalarField_JsonData{JsonData: &schemapb.JSONArray{Data: [][]byte{[]byte(`{"stock":3}`)}}}}}}
	got, _ = columns([]*schemapb.FieldData{dyn}, 1)
	if got[0]["stock"] != 3.0 {
		t.Errorf("dynamic fields are flattened: %v", got[0])
	}
}

func TestNewValidatesEndpoint(t *testing.T) {
	if _, err := New(Config{Endpoint: "http://example.com"}, vectorstore.Spec{}); err == nil {
		t.Error("plain http needs a trusted host")
	}
	s, err := New(Config{Endpoint: "https://example.com", APIKey: "k", Database: "db"}, vectorstore.Spec{Collection: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if s.md.Get("authorization")[0] == "" || s.md.Get("dbname")[0] != "db" || s.ProviderName() != "byo:milvus" {
		t.Error("auth and database metadata")
	}
	_ = s.Close()
	if literal(`a\b"c`) != `"a\\b\"c"` {
		t.Error("literal escaping")
	}
}
