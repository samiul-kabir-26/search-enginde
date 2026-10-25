package pgvector

import (
	"reflect"
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
	spec := vectorstore.Spec{Collection: "products", Fields: map[string]string{"custom.season": "season"}}
	cols := map[string]column{"brand": {}, "price_minor": {}, "ship_regions": {}, "status": {}, "season": {}, "created_at": {}}
	at := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	e := filter.Expr{And: []filter.Expr{
		cond("brand", filter.Eq, "Rivet", catalog.TypeKeyword),
		cond("ship_regions", filter.Contains, "US", catalog.TypeKeywordArray),
		{Or: []filter.Expr{
			cond("price_minor", filter.Range, filter.Bounds{Gte: int64(100), Lt: int64(500)}, catalog.TypeInt),
			cond("status", filter.Nin, []any{"draft", "deleted"}, catalog.TypeKeyword),
		}},
		cond("custom.season", filter.Ne, "winter", catalog.TypeKeyword),
		cond("ship_regions", filter.In, []any{"DE", "FR"}, catalog.TypeKeywordArray),
		cond("created_at", filter.Gt, at, catalog.TypeDate),
	}}
	sql, args, err := translate(e, spec, cols, []any{"[1,0]"})
	if err != nil {
		t.Fatal(err)
	}
	want := `("brand" = $2) AND ($3::text = ANY("ship_regions")) AND (("price_minor" >= $4 AND "price_minor" < $5) OR (NOT COALESCE(("status" = ANY($6::text[])), false))) AND ("season" IS DISTINCT FROM $7) AND ("ship_regions" && $8::text[]) AND ("created_at" > $9)`
	if sql != want {
		t.Errorf("sql =\n%s\nwant\n%s", sql, want)
	}
	wantArgs := []any{"[1,0]", "Rivet", "US", int64(100), int64(500), []string{"draft", "deleted"}, "winter", []string{"DE", "FR"}, at}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args = %#v", args)
	}
	if _, _, err := translate(cond("vendor_id", filter.Eq, "v", catalog.TypeKeyword), spec, cols, nil); err == nil || !strings.Contains(err.Error(), "pushed down") {
		t.Errorf("a field without a column must fail: %v", err)
	}
	if s, _, err := translate(filter.Expr{}, spec, cols, nil); err != nil || s != "" {
		t.Error("an empty filter is empty")
	}
}

func TestVectorCodec(t *testing.T) {
	v := []float32{0.5, -1, 3.25}
	got, err := decodeVector(encodeVector(v))
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Errorf("round trip = %v, %v", got, err)
	}
	if op, score := operator(vectorstore.MetricL2, `"v"`, "$1"); op != "<->" || !strings.HasPrefix(score, "-(") {
		t.Error("l2 operator")
	}
	if op, _ := operator(vectorstore.MetricDot, `"v"`, "$1"); op != "<#>" {
		t.Error("dot operator")
	}
	if columnValue("2025-01-02T03:04:05Z", column{dataType: "timestamp with time zone"}) != time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC) {
		t.Error("RFC 3339 strings become times for timestamp columns")
	}
	if columnValue(3.0, column{dataType: "bigint"}) != int64(3) || columnValue(int64(2), column{dataType: "double precision"}) != 2.0 {
		t.Error("numbers follow the column type")
	}
}
