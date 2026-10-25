package queryunderstanding

import (
	"reflect"
	"testing"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
)

func TestNormalize(t *testing.T) {
	if got := Normalize("  Ｒｅｄ   LEATHER\tBag "); got != "red leather bag" {
		t.Errorf("Normalize = %q", got)
	}
}

func i64(v int64) *int64 { return &v }

func TestExtractPrice(t *testing.T) {
	cases := []struct {
		in       string
		min, max *int64
		rest     string
	}{
		{"red bag under $50", nil, i64(5000), "red bag"},
		{"chair below 120 dollars", nil, i64(12000), "chair"},
		{"lamp less than 19.99", nil, i64(1999), "lamp"},
		{"desk over 200", i64(20000), nil, "desk"},
		{"shoes between 40 and 60 usd", i64(4000), i64(6000), "shoes"},
		{"shoes between 60 and 40", i64(4000), i64(6000), "shoes"},
		{"kettlebell under 5 lbs", nil, nil, "kettlebell under 5 lbs"},
		{"tv over 40 inch", nil, nil, "tv over 40 inch"},
		{"made from wood", nil, nil, "made from wood"},
		{"red bag", nil, nil, "red bag"},
	}
	for _, tc := range cases {
		b, rest := ExtractPrice(tc.in)
		if !reflect.DeepEqual(b.Min, tc.min) || !reflect.DeepEqual(b.Max, tc.max) || rest != tc.rest {
			t.Errorf("%q: min=%v max=%v rest=%q", tc.in, deref(b.Min), deref(b.Max), rest)
		}
	}
	b, _ := ExtractPrice("bag under $50")
	f := b.Filter()
	if f.Cond == nil || f.Cond.Field != "price_minor" || f.Cond.Value.(filter.Bounds).Lte != int64(5000) {
		t.Errorf("filter = %+v", f.Cond)
	}
	if !(PriceBounds{}).Filter().IsZero() {
		t.Error("no bounds must give the empty filter")
	}
}

func deref(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestDictionary(t *testing.T) {
	d := NewDictionary(map[string][]string{"material": {"Vegan Leather"}, "style": {"boho", "mid century"}})
	got := d.Extract("navy blue vegan leather mid century chair in stainless steel")
	want := map[string][]string{
		"color":    {"navy blue"},
		"material": {"stainless steel", "vegan leather"},
		"style":    {"mid century"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Extract = %v, want %v", got, want)
	}
	if got := d.Extract("redwood"); len(got) != 0 {
		t.Errorf("terms must match whole words, got %v", got)
	}
}

func TestVoteCategories(t *testing.T) {
	hit := func(score float64, path ...string) pipeline.Hit {
		return pipeline.Hit{Score: score, Doc: &pipeline.Doc{CategoryIDs: path}}
	}
	// Leaves are fragmented (each 20%), so the vote moves up to "chairs" (60%).
	hits := []pipeline.Hit{
		hit(1, "furniture", "chairs", "office-chairs"), hit(1, "furniture", "chairs", "gaming-chairs"),
		hit(1, "furniture", "chairs", "dining-chairs"), hit(1, "furniture", "tables", "desks"),
		hit(1, "furniture", "tables", "side-tables"), hit(0, "ignored"), {Score: 1},
	}
	got := VoteCategories(hits, 3, 0.5)
	if len(got) != 1 || got[0].ID != "chairs" || got[0].Confidence != 0.6 {
		t.Errorf("vote = %+v, want chairs 0.6", got)
	}
	// With a low threshold the leaf level already qualifies; ties break by id.
	got = VoteCategories(hits, 2, 0.15)
	if len(got) != 2 || got[0].ID != "desks" || got[0].Confidence != 0.2 {
		t.Errorf("leaf vote = %+v", got)
	}
	if VoteCategories(nil, 3, 0.1) != nil {
		t.Error("no hits, no categories")
	}
}
