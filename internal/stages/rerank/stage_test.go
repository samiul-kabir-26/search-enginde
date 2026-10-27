package rerank_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/ads"
	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/stages/rerank"
	"github.com/zavisoft/search-engine/internal/tenant"
)

func TestPersonalizationFormula(t *testing.T) {
	cfg := &tenant.Config{
		Rerank: tenant.RerankConfig{
			Personalization: &tenant.PersonalizationConfig{
				Enabled: true,
				Alpha:   0.5,
				Beta:    0.2,
			},
		},
	}
	res := &tenant.Resolved{Config: cfg}
	stage := rerank.New()

	st := &pipeline.State{
		Resolved: res,
		Query:    &pipeline.Query{Depth: 10},
		Candidates: []pipeline.Candidate{
			{ProductID: "p1", Stage: pipeline.StageRank, Score: 1.0, Features: map[string]float64{feature.UserCategoryAffinity: 0.0, feature.UserPriceBandMatch: 0.0}},
			{ProductID: "p2", Stage: pipeline.StageRank, Score: 0.9, Features: map[string]float64{feature.UserCategoryAffinity: 0.8, feature.UserPriceBandMatch: 1.0}},
		},
	}

	rep, err := stage.Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Provider != "personalization" {
		t.Errorf("rep.Provider = %s, want personalization", rep.Provider)
	}

	// p2 score: 0.9 + 0.5*0.8 + 0.2*1.0 = 0.9 + 0.4 + 0.2 = 1.5
	// p1 score: 1.0
	// p2 should now be first!
	if st.Candidates[0].ProductID != "p2" || st.Candidates[0].Score < 1.49 {
		t.Errorf("expected p2 to be ranked first with score ~1.5, got %+v", st.Candidates[0])
	}
	if st.Candidates[1].ProductID != "p1" {
		t.Errorf("expected p1 to be second, got %+v", st.Candidates[1])
	}
}

func TestDiversity(t *testing.T) {
	cfg := &tenant.Config{
		Rerank: tenant.RerankConfig{
			Diversity: &tenant.DiversityConfig{
				MaxPerVendor: 2,
				Window:       3,
			},
		},
	}
	res := &tenant.Resolved{Config: cfg}
	stage := rerank.New()

	st := &pipeline.State{
		Resolved: res,
		Query:    &pipeline.Query{Depth: 10},
		Candidates: []pipeline.Candidate{
			{ProductID: "p1", Score: 1.0, Doc: &pipeline.Doc{VendorID: "v1"}},
			{ProductID: "p2", Score: 0.9, Doc: &pipeline.Doc{VendorID: "v1"}},
			{ProductID: "p3", Score: 0.8, Doc: &pipeline.Doc{VendorID: "v1"}}, // 3rd v1 in window of 3 -> should be skipped!
			{ProductID: "p4", Score: 0.7, Doc: &pipeline.Doc{VendorID: "v2"}}, // v2 -> pulled ahead of p3
			{ProductID: "p5", Score: 0.6, Doc: &pipeline.Doc{VendorID: "v3"}},
		},
	}

	_, err := stage.Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}

	// Window of 3, max 2 per vendor: each position sees the 2 results before it.
	// pos 0: p1 (v1)
	// pos 1: p2 (v1)
	// pos 2: before it [p1, p2], v1 has 2. p3 is deferred; p4 (v2) placed.
	// pos 3: before it [p2, p4], v1 has 1. p3 (v1) placed: window [p2, p4, p3] holds 2 v1.
	// pos 4: p5 (v3)
	expected := []string{"p1", "p2", "p4", "p3", "p5"}
	for i, want := range expected {
		if st.Candidates[i].ProductID != want {
			t.Errorf("position %d: got %s, want %s", i, st.Candidates[i].ProductID, want)
		}
	}
}

type mockAdStore struct {
	creatives []ads.ActiveCreative
}

func (m *mockAdStore) ListActiveCreatives(_ context.Context, _ tenant.ID) ([]ads.ActiveCreative, error) {
	return m.creatives, nil
}

func TestAdsBlendingInRerank(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cfg := &tenant.Config{
		Ads: tenant.AdsConfig{
			Enabled:        true,
			Slots:          []int{1}, // Slot 1
			RelevanceFloor: 0.1,
		},
	}
	res := &tenant.Resolved{
		Tenant:   tenant.Tenant{ID: "demo"},
		Config:   cfg,
		Registry: mustRegistry(),
	}

	adStore := &mockAdStore{
		creatives: []ads.ActiveCreative{
			{
				CampaignID:       "c1",
				TenantID:         "demo",
				VendorID:         "v_ad",
				DailyBudgetMinor: 1000,
				BidMinor:         50,
				StartAt:          now.Add(-time.Hour),
				ProductID:        "p_ad",
			},
		},
	}
	blender := ads.NewBlender(adStore, nil)
	stage := rerank.New(rerank.WithBlender(blender), rerank.WithClock(func() time.Time { return now }))

	st := &pipeline.State{
		Resolved: res,
		Query:    &pipeline.Query{Depth: 10},
		Candidates: []pipeline.Candidate{
			{ProductID: "p1", Stage: pipeline.StageRank, Score: 1.0},
			{ProductID: "p_ad", Stage: pipeline.StageRank, Score: 0.8},
			{ProductID: "p2", Stage: pipeline.StageRank, Score: 0.5},
		},
	}

	_, err := stage.Run(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}

	// Slot 1 should have p_ad with Sponsored=true
	if st.Candidates[0].ProductID != "p_ad" || !st.Candidates[0].Sponsored {
		t.Errorf("candidate 0 = %+v, want p_ad with Sponsored=true", st.Candidates[0])
	}
	// Organic list should not duplicate p_ad
	if len(st.Candidates) != 3 {
		t.Fatalf("candidates len = %d, want 3", len(st.Candidates))
	}
	if st.Candidates[1].ProductID != "p1" || st.Candidates[1].Sponsored {
		t.Errorf("candidate 1 = %+v, want p1 (organic)", st.Candidates[1])
	}
	if st.Candidates[2].ProductID != "p2" || st.Candidates[2].Sponsored {
		t.Errorf("candidate 2 = %+v, want p2 (organic)", st.Candidates[2])
	}
}

func mustRegistry() *catalog.Registry {
	r, _ := catalog.NewRegistry(nil)
	return r
}

// Personalization re-orders the ranked candidates only; the tail keeps its order (PLAN.md R4).
func TestPersonalizationLeavesTailOrder(t *testing.T) {
	cfg := &tenant.Config{Rerank: tenant.RerankConfig{
		Personalization: &tenant.PersonalizationConfig{Enabled: true, Alpha: 1, Beta: 0},
	}}
	aff := func(x float64) map[string]float64 { return map[string]float64{feature.UserCategoryAffinity: x} }
	st := &pipeline.State{
		Resolved: &tenant.Resolved{Config: cfg},
		Query:    &pipeline.Query{Depth: 10},
		Candidates: []pipeline.Candidate{
			{ProductID: "r1", Score: 1.0, Stage: pipeline.StageRank, Features: aff(0)},
			{ProductID: "r2", Score: 0.9, Stage: pipeline.StageRank, Features: aff(0.5)},
			// Pre-rank scores are on another scale: high, but behind the ranked ones.
			{ProductID: "t1", Score: 7, Stage: pipeline.StagePrerank, Features: aff(0)},
			{ProductID: "t2", Score: 9, Stage: pipeline.StagePrerank, Features: aff(1)},
		},
	}
	if _, err := rerank.New().Run(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range st.Candidates {
		got = append(got, c.ProductID)
	}
	if want := []string{"r2", "r1", "t1", "t2"}; !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if st.Candidates[2].Score != 7 || st.Candidates[3].Score != 9 {
		t.Errorf("tail scores changed: %v, %v", st.Candidates[2].Score, st.Candidates[3].Score)
	}
}

// Every window of results holds at most max_per_vendor of a vendor while the input allows it,
// and the output is a permutation of the input.
func TestDiversityWindowsAndPermutation(t *testing.T) {
	const maxPer, window = 2, 5
	vendors := []string{"a", "a", "a", "a", "a", "a", "b", "b", "b", "c", "a", "d", "e", "a", "f", "g", "h", "i", "j", "k"}
	var cs []pipeline.Candidate
	for i, v := range vendors {
		cs = append(cs, pipeline.Candidate{ProductID: fmt.Sprintf("p%02d", i), Score: float64(100 - i), Doc: &pipeline.Doc{VendorID: v}})
	}
	cfg := &tenant.Config{Rerank: tenant.RerankConfig{Diversity: &tenant.DiversityConfig{MaxPerVendor: maxPer, Window: window}}}
	st := &pipeline.State{Resolved: &tenant.Resolved{Config: cfg}, Query: &pipeline.Query{}, Candidates: cs}
	if _, err := rerank.New().Run(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	out := st.Candidates
	seen := map[string]bool{}
	for _, c := range out {
		seen[c.ProductID] = true
	}
	if len(out) != len(cs) || len(seen) != len(cs) {
		t.Fatalf("output has %d candidates (%d distinct), want a permutation of %d", len(out), len(seen), len(cs))
	}
	// Up to the point where only vendor a is left, no window may break the limit.
	for start := 0; start+window <= 14; start++ {
		counts := map[string]int{}
		for _, c := range out[start : start+window] {
			counts[c.Doc.VendorID]++
			if counts[c.Doc.VendorID] > maxPer {
				t.Fatalf("window at %d holds %d of vendor %s", start, counts[c.Doc.VendorID], c.Doc.VendorID)
			}
		}
	}
}
