package featurejob

import (
	"fmt"
	"math"
	"testing"

	"github.com/zavisoft/search-engine/internal/pipeline"
)

func TestFitPropensityRecoversTheExaminationCurve(t *testing.T) {
	for _, eta := range []float64{0.5, 0.7, 1.0} {
		var stats []PositionStat
		for k := 1; k <= 50; k++ {
			imps := int64(4000)
			stats = append(stats, PositionStat{Position: k, Impressions: imps, Clicks: int64(math.Round(float64(imps) * 0.3 * math.Pow(float64(k), -eta)))})
		}
		got, clicks, ok := FitPropensity(stats)
		if !ok || math.Abs(got-eta) > 0.02 || clicks < MinPropensityClicks {
			t.Errorf("eta %.1f: fit %.3f ok=%v clicks=%d", eta, got, ok, clicks)
		}
	}
}

func TestFitPropensityNeedsEnoughTraffic(t *testing.T) {
	few := []PositionStat{{1, 100, 30}, {2, 100, 20}, {3, 100, 15}, {4, 100, 12}, {5, 100, 10}}
	if _, _, ok := FitPropensity(few); ok {
		t.Error("87 clicks must not produce an estimate")
	}
	twoPositions := []PositionStat{{1, 10000, 3000}, {2, 10000, 2000}}
	if _, _, ok := FitPropensity(twoPositions); ok {
		t.Error("two positions must not produce an estimate")
	}
	var rising []PositionStat // clicks grow with position: clamps at 0
	for k := 1; k <= 10; k++ {
		rising = append(rising, PositionStat{k, 1000, int64(20 * k)})
	}
	if eta, _, ok := FitPropensity(rising); !ok || eta != 0 {
		t.Errorf("rising CTR: eta %v ok %v, want clamped to 0", eta, ok)
	}
}

func TestItemFeaturesShrinkTowardTheTenantMean(t *testing.T) {
	tenant := Counts{Exams7: 10000, Exams30: 40000, Clicks7: 1000, Clicks30: 4000, Orders7: 300, Orders30: 1200}
	unseen := ItemFeatures(Counts{}, tenant)
	if unseen.CTR7d != 0.1 || unseen.CTR30d != 0.1 || math.Abs(unseen.CVR7d-0.3) > 1e-9 || unseen.Orders30dLog != 0 {
		t.Errorf("no data = tenant means: %+v", unseen)
	}
	popular := ItemFeatures(Counts{Exams7: 2000, Exams30: 2000, Clicks7: 1000, Clicks30: 1000, Orders7: 500, Orders30: 500}, tenant)
	if math.Abs(popular.CTR7d-1002.0/2020) > 1e-9 || math.Abs(popular.CVR7d-503.0/1010) > 1e-9 {
		t.Errorf("lots of data ≈ raw rates: %+v", popular)
	}
	if want := math.Log1p(500) / 10; math.Abs(popular.Orders30dLog-want) > 1e-9 {
		t.Errorf("orders_30d_log = %v, want %v", popular.Orders30dLog, want)
	}
	lucky := ItemFeatures(Counts{Exams7: 0.5, Clicks7: 3}, tenant)
	if lucky.CTR7d > 0.3 {
		t.Errorf("3 clicks on half an examination must be shrunk, got %v", lucky.CTR7d)
	}
	capped := ItemFeatures(Counts{Exams30: 1, Clicks30: 1e6, Orders30: 1e9}, tenant)
	if capped.CTR30d != 1 || capped.Orders30dLog != 1 {
		t.Errorf("features stay within [0, 1]: %+v", capped)
	}
}

func TestUserFeatures(t *testing.T) {
	docs := map[string]*pipeline.Doc{
		"a": {CategoryIDs: []string{"bags", "handbags"}, PriceMinor: 4000},
		"b": {CategoryIDs: []string{"bags", "wallets"}, PriceMinor: 2000},
		"c": {CategoryIDs: []string{"shoes"}, PriceMinor: 9000},
		"d": nil, // deleted from the catalog
	}
	u, ok := UserFeatures([]Interaction{{"a", 3}, {"b", 1}, {"c", 1}, {"d", 5}, {"missing", 2}}, docs)
	if !ok {
		t.Fatal("want features")
	}
	for cat, want := range map[string]float64{"bags": 0.8, "handbags": 0.6, "wallets": 0.2, "shoes": 0.2} {
		if u.CategoryAffinity[cat] != want {
			t.Errorf("affinity %s = %v, want %v", cat, u.CategoryAffinity[cat], want)
		}
	}
	// Weighted prices: 2000 (w1), 4000 (w3), 9000 (w1) of total 5: p20 = 2000, p80 = 4000.
	if u.PriceLo != 2000 || u.PriceHi != 4000 {
		t.Errorf("price band %d–%d", u.PriceLo, u.PriceHi)
	}

	if u, _ := UserFeatures([]Interaction{{"a", 1}, {"b", 1}}, docs); u.PriceHi != 0 {
		t.Errorf("two interactions are too few for a price band: %+v", u)
	}
	if _, ok := UserFeatures([]Interaction{{"d", 1}}, docs); ok {
		t.Error("no catalog products, no features")
	}

	many := map[string]*pipeline.Doc{}
	var ins []Interaction
	for i := range 30 {
		id := fmt.Sprint(i)
		many[id] = &pipeline.Doc{CategoryIDs: []string{"c" + id}, PriceMinor: 100}
		ins = append(ins, Interaction{id, float64(i + 1)})
	}
	if u, _ := UserFeatures(ins, many); len(u.CategoryAffinity) != TopCategories || u.CategoryAffinity["c29"] == 0 || u.CategoryAffinity["c0"] != 0 {
		t.Errorf("keeps the top %d categories: %v", TopCategories, u.CategoryAffinity)
	}
}
