package filter

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
)

func registry(t *testing.T) *catalog.Registry {
	t.Helper()
	r, err := catalog.NewRegistry([]catalog.CustomField{
		{Name: "eco", Type: catalog.TypeBool, Source: "$.e", Filterable: true},
		{Name: "weight", Type: catalog.TypeFloat, Source: "$.w", Filterable: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func parse(t *testing.T, s string) Expr {
	t.Helper()
	var e Expr
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		t.Fatalf("unmarshal %s: %v", s, err)
	}
	return e
}

func TestCompileNormalizesValues(t *testing.T) {
	e := parse(t, `{"and":[
		{"field":"price_minor","op":"lte","value":20000},
		{"field":"ship_regions","op":"contains","value":"US"},
		{"field":"brand","op":"in","value":["Rivet","Solimo"]},
		{"field":"custom.eco","op":"eq","value":true},
		{"field":"custom.weight","op":"range","value":{"gte":1.5,"lt":3}},
		{"field":"created_at","op":"gte","value":"2025-01-01T00:00:00Z"},
		{"or":[{"field":"stock","op":"gt","value":0},{"field":"moq","op":"eq","value":1}]}
	]}`)
	c, errs := Compile(e, registry(t), "/filters")
	if len(errs) > 0 {
		t.Fatalf("errors: %+v", errs)
	}
	conds := c.And
	if v := conds[0].Cond.Value; v != int64(20000) {
		t.Errorf("price value = %#v, want int64", v)
	}
	if v := conds[2].Cond.Value.([]any); !slices.Equal([]string{v[0].(string), v[1].(string)}, []string{"Rivet", "Solimo"}) {
		t.Errorf("in values = %#v", v)
	}
	if b := conds[4].Cond.Value.(Bounds); b.Gte != 1.5 || b.Lt != float64(3) || b.Gt != nil {
		t.Errorf("range = %#v", b)
	}
	if ts := conds[5].Cond.Value.(time.Time); ts.Year() != 2025 {
		t.Errorf("date = %v", ts)
	}
	if conds[3].Cond.Type != catalog.TypeBool {
		t.Errorf("type not filled in: %+v", conds[3].Cond)
	}
	if _, err := json.Marshal(c); err != nil {
		t.Errorf("compiled expressions must marshal: %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	cases := map[string]struct{ in, path, code string }{
		"unknown field":   {`{"field":"colour","op":"eq","value":"x"}`, "/f/field", "unknown_field"},
		"not filterable":  {`{"field":"title","op":"eq","value":"x"}`, "/f/field", "not_filterable"},
		"op on type":      {`{"field":"brand","op":"gt","value":"x"}`, "/f/op", "unsupported_op"},
		"contains scalar": {`{"field":"brand","op":"contains","value":"x"}`, "/f/op", "unsupported_op"},
		"wrong type":      {`{"field":"price_minor","op":"eq","value":"cheap"}`, "/f/value", "invalid_value"},
		"fractional int":  {`{"field":"price_minor","op":"eq","value":1.5}`, "/f/value", "invalid_value"},
		"empty in":        {`{"field":"brand","op":"in","value":[]}`, "/f/value", "invalid_value"},
		"bad range key":   {`{"field":"price_minor","op":"range","value":{"from":1}}`, "/f/value/from", "invalid_value"},
		"bad date":        {`{"field":"created_at","op":"gt","value":"yesterday"}`, "/f/value", "invalid_value"},
		"empty and":       {`{"and":[]}`, "/f/and", "empty"},
		"too deep":        {`{"and":[{"or":[{"and":[{"or":[{"field":"moq","op":"eq","value":1}]}]}]}]}`, "/f/and/0/or/0/and/0", "too_deep"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, errs := Compile(parse(t, tc.in), registry(t), "/f")
			for _, e := range errs {
				if e.Path == tc.path && e.Code == tc.code {
					return
				}
			}
			t.Fatalf("want %s %s, got %+v", tc.path, tc.code, errs)
		})
	}
}

func TestUnmarshalRejectsAmbiguousShapes(t *testing.T) {
	for _, s := range []string{
		`{"and":[],"field":"x","op":"eq","value":1}`,
		`{"field":"x","op":"eq","value":1,"extra":true}`,
		`{}`,
	} {
		var e Expr
		if err := json.Unmarshal([]byte(s), &e); err == nil {
			t.Errorf("%s: want error", s)
		}
	}
	var e Expr
	if err := json.Unmarshal([]byte(`null`), &e); err != nil || !e.IsZero() {
		t.Errorf("null must be the empty filter: %v", err)
	}
}

func TestAllAndSplit(t *testing.T) {
	a := Expr{Cond: &Condition{Field: "brand", Op: Eq, Value: "x"}}
	b := Expr{Cond: &Condition{Field: "stock", Op: Gt, Value: int64(0)}}
	or := Expr{Or: []Expr{a, b}}
	if !All().IsZero() || All(Expr{}, a).Cond != a.Cond {
		t.Error("All must drop empty expressions and unwrap a single one")
	}
	by, rest := SplitByField(All(a, b, or))
	if len(by["brand"]) != 1 || len(by["stock"]) != 1 || len(rest) != 1 {
		t.Errorf("split = %v / %v", by, rest)
	}
	by, rest = SplitByField(or)
	if len(by) != 0 || len(rest) != 1 {
		t.Error("a top-level OR is not separable by field")
	}
	m, _ := json.Marshal(All(a, Expr{Cond: &Condition{Field: "price_minor", Op: Range, Value: Bounds{Lte: int64(5)}}}))
	if !strings.Contains(string(m), `"lte":5`) {
		t.Errorf("marshal = %s", m)
	}
}
