package policy

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func item() map[string]any {
	return map[string]any{
		"product_id": "p1", "brand": "Rivet", "price_minor": int64(4500), "category_ids": []string{"bags", "electronics"},
		"rating": 4.6, "stock": int64(3), "ship_regions": []string{"US"}, "created_at": time.Now(),
		"vendor": map[string]any{"score": 0.9, "on_time_dispatch_rate": 0.97, "verified": false},
		"custom": map[string]any{"season": "summer"},
	}
}

func query() map[string]any {
	return map[string]any{"text": "red bag", "type": "text", "categories": []string{"bags"}, "attributes": map[string]any{}}
}

func TestCompileAndEvalDocExamples(t *testing.T) {
	set, errs := Compile([]Rule{
		{ID: "hide-unverified-electronics", Kind: KindFilter, Expr: "!(item.category_ids.exists(c, c == 'electronics') && !item.vendor.verified)"},
		{ID: "boost-fast-shippers", Kind: KindBoost, Expr: "item.vendor.on_time_dispatch_rate > 0.95 ? 0.2 : 0.0"},
	}, nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if set.Len() != 2 {
		t.Fatalf("len %d", set.Len())
	}
	if keep, _, errs := set.Eval(item(), query()); keep || errs != 0 {
		t.Errorf("an unverified vendor's electronics must be filtered: keep=%v errs=%d", keep, errs)
	}
	it := item()
	it["vendor"].(map[string]any)["verified"] = true
	if keep, boost, errs := set.Eval(it, query()); !keep || boost != 0.2 || errs != 0 {
		t.Errorf("verified: keep=%v boost=%v errs=%d", keep, boost, errs)
	}
}

func TestBoostsSumAndClamp(t *testing.T) {
	set, errs := Compile([]Rule{
		{ID: "a", Kind: KindBoost, Expr: "0.8"},
		{ID: "b", Kind: KindBoost, Expr: "item.price_minor < 5000 ? 1 : 0"},
		{ID: "c", Kind: KindBoost, Expr: "query.text.contains('bag') ? -0.1 : 0.0"},
	}, nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if _, boost, _ := set.Eval(item(), query()); boost != 1 {
		t.Errorf("0.8 + 1 - 0.1 clamps to 1, got %v", boost)
	}
	neg, _ := Compile([]Rule{{ID: "n", Kind: KindBoost, Expr: "-3.0"}}, nil)
	if _, boost, _ := neg.Eval(item(), query()); boost != -1 {
		t.Errorf("clamps to -1, got %v", boost)
	}
}

func TestEvalErrorsKeepTheItem(t *testing.T) {
	set, errs := Compile([]Rule{
		{ID: "missing", Kind: KindFilter, Expr: "item.custom.color == 'red'"},
		{ID: "no-rating", Kind: KindBoost, Expr: "item.rating > 4.0 ? 0.5 : 0.0"},
	}, []string{"color", "season"})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	it := item()
	delete(it, "rating")
	keep, boost, n := set.Eval(it, query())
	if !keep || boost != 0 || n != 2 {
		t.Errorf("keep=%v boost=%v errs=%d; failing rules keep the item and add nothing", keep, boost, n)
	}
	guarded, _ := Compile([]Rule{{ID: "g", Kind: KindBoost, Expr: "has(item.rating) && item.rating > 4.0 ? 0.5 : 0.0"}}, nil)
	if _, boost, n := guarded.Eval(it, query()); boost != 0 || n != 0 {
		t.Errorf("has() guards a missing field: boost=%v errs=%d", boost, n)
	}
	var nilSet *Set
	if keep, boost, n := nilSet.Eval(it, query()); !keep || boost != 0 || n != 0 || nilSet.Len() != 0 {
		t.Error("a nil set keeps everything")
	}
}

func TestCompileErrors(t *testing.T) {
	long := "item.price_minor > 0" + strings.Repeat(" ", MaxExprBytes)
	tooCostly := "item.category_ids.all(a, item.ship_regions.all(b, item.category_ids.exists(c, a + b == c)))"
	for name, tc := range map[string]struct {
		rule  Rule
		field string
		code  string
	}{
		"typo":           {Rule{ID: "x", Kind: KindFilter, Expr: "item.vendor.verifed"}, "expr", "unknown_field"},
		"unknown item":   {Rule{ID: "x", Kind: KindFilter, Expr: "item.title == 'x'"}, "expr", "unknown_field"},
		"unknown custom": {Rule{ID: "x", Kind: KindFilter, Expr: "item.custom.colour == 'x'"}, "expr", "unknown_field"},
		"unknown query":  {Rule{ID: "x", Kind: KindFilter, Expr: "query.user == 'x'"}, "expr", "unknown_field"},
		"filter type":    {Rule{ID: "x", Kind: KindFilter, Expr: "'yes'"}, "expr", "cel_type"},
		"boost type":     {Rule{ID: "x", Kind: KindBoost, Expr: "true"}, "expr", "cel_type"},
		"syntax":         {Rule{ID: "x", Kind: KindFilter, Expr: "item.brand =="}, "expr", "cel_compile"},
		"no functions":   {Rule{ID: "x", Kind: KindFilter, Expr: "http_get('x')"}, "expr", "cel_compile"},
		"too long":       {Rule{ID: "x", Kind: KindFilter, Expr: long}, "expr", "invalid"},
		"bad kind":       {Rule{ID: "x", Kind: "rank", Expr: "true"}, "kind", "invalid"},
		"no id":          {Rule{Kind: KindFilter, Expr: "true"}, "id", "invalid"},
		"cost":           {Rule{ID: "x", Kind: KindFilter, Expr: tooCostly}, "expr", "cel_cost"},
	} {
		_, errs := Compile([]Rule{tc.rule}, []string{"color"})
		if len(errs) != 1 || errs[0].Field != tc.field || errs[0].Code != tc.code || errs[0].Index != 0 {
			t.Errorf("%s: errors %+v, want %s/%s", name, errs, tc.field, tc.code)
		}
	}
	rules := make([]Rule, MaxRules+1)
	for i := range rules {
		rules[i] = Rule{ID: fmt.Sprint(i), Kind: KindFilter, Expr: "true"}
	}
	if _, errs := Compile(rules, nil); len(errs) != 1 || errs[0].Code != "too_many" || errs[0].Index != -1 {
		t.Errorf("too many: %+v", errs)
	}
	if _, errs := Compile([]Rule{{ID: "a", Kind: KindFilter, Expr: "true"}, {ID: "a", Kind: KindFilter, Expr: "true"}}, nil); len(errs) != 1 || errs[0].Index != 1 {
		t.Errorf("duplicate id: %+v", errs)
	}
	if _, errs := Compile([]Rule{{ID: "ok", Kind: KindFilter, Expr: "item.custom.color == 'red' || item.brand.matches('^Ri')"}}, []string{"color"}); len(errs) != 0 {
		t.Errorf("custom field and matches(): %+v", errs)
	}
}

func BenchmarkEval(b *testing.B) {
	set, _ := Compile([]Rule{
		{ID: "f", Kind: KindFilter, Expr: "!(item.category_ids.exists(c, c == 'electronics') && !item.vendor.verified)"},
		{ID: "b", Kind: KindBoost, Expr: "item.vendor.on_time_dispatch_rate > 0.95 ? 0.2 : 0.0"},
	}, nil)
	it, q := item(), query()
	it["vendor"].(map[string]any)["verified"] = true
	for b.Loop() {
		set.Eval(it, q)
	}
}
