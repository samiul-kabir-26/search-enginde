package ads_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/ads"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/tenant"
)

func TestTargeting(t *testing.T) {
	t.Run("empty matches all", func(t *testing.T) {
		tgt := ads.Targeting{}
		if !tgt.Matches([]string{"cat1"}, "running shoes") {
			t.Errorf("empty targeting should match")
		}
	})

	t.Run("category match", func(t *testing.T) {
		tgt := ads.Targeting{Categories: []string{"Handbags", "Shoes"}}
		if !tgt.Matches([]string{"handbags"}, "anything") {
			t.Errorf("case-insensitive category should match")
		}
		if tgt.Matches([]string{"backpacks"}, "anything") {
			t.Errorf("different category should not match")
		}
	})

	t.Run("keyword and query match", func(t *testing.T) {
		tgt := ads.Targeting{Keywords: []string{"leather"}}
		if !tgt.Matches(nil, "Red Leather Bag") {
			t.Errorf("keyword in query should match")
		}
		if tgt.Matches(nil, "Canvas Bag") {
			t.Errorf("absent keyword should not match")
		}
	})
}

func TestGSPAuction(t *testing.T) {
	candidates := []ads.Candidate{
		{CampaignID: "c1", ProductID: "p1", BidMinor: 100, Quality: 0.8}, // score 80
		{CampaignID: "c2", ProductID: "p2", BidMinor: 80, Quality: 0.5},  // score 40
		{CampaignID: "c3", ProductID: "p3", BidMinor: 50, Quality: 0.4},  // score 20
	}

	winners := ads.RunGSPAuction(candidates, 10)
	if len(winners) != 3 {
		t.Fatalf("got %d winners, want 3", len(winners))
	}

	// Winner 0: p1. Score 80.
	// Next score is 40. Clearing price = ceil(40 / 0.8) + 1 = 50 + 1 = 51.
	if winners[0].ProductID != "p1" || winners[0].ClearedCPC != 51 {
		t.Errorf("winner 0 = %+v, want p1 with cleared CPC 51", winners[0])
	}

	// Winner 1: p2. Score 40.
	// Next score is 20. Clearing price = ceil(20 / 0.5) + 1 = 40 + 1 = 41.
	if winners[1].ProductID != "p2" || winners[1].ClearedCPC != 41 {
		t.Errorf("winner 1 = %+v, want p2 with cleared CPC 41", winners[1])
	}

	// Winner 2: p3. Last place -> reserve CPC = 10.
	if winners[2].ProductID != "p3" || winners[2].ClearedCPC != 10 {
		t.Errorf("winner 2 = %+v, want p3 with reserve CPC 10", winners[2])
	}
}

func TestGSPAuctionCapping(t *testing.T) {
	candidates := []ads.Candidate{
		{CampaignID: "c1", ProductID: "p1", BidMinor: 40, Quality: 0.5},
		{CampaignID: "c2", ProductID: "p2", BidMinor: 50, Quality: 0.5},
	}
	// Winner 0 is p2 (bid 50 * q 0.5 = 25).
	// Next is p1 (bid 40 * q 0.5 = 20).
	// Price for p2 = ceil(20 / 0.5) + 1 = 41.
	winners := ads.RunGSPAuction(candidates, 5)
	if winners[0].ProductID != "p2" || winners[0].ClearedCPC != 41 {
		t.Errorf("winner 0 = %+v", winners[0])
	}
}

func TestPacing(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	p := ads.NewRedisPacing(rdb)
	ctx := context.Background()
	tid := tenant.ID("t1")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC) // 12:00 -> 13/24 of day

	// 1. Below budget -> admit
	admit, err := p.Admit(ctx, tid, "c1", 2400, now)
	if err != nil || !admit {
		t.Fatalf("expected admit, got %v, err=%v", admit, err)
	}

	// 2. Record spend up to budget limit
	if err := p.RecordSpend(ctx, tid, "c1", 2400, now); err != nil {
		t.Fatal(err)
	}
	admit, err = p.Admit(ctx, tid, "c1", 2400, now)
	if err != nil || admit {
		t.Fatalf("expected exhausted budget to be refused, got admit=%v", admit)
	}

	// 3. Price recording & retrieval
	if err := p.RecordPrice(ctx, tid, "req-1", "prod-1", "c1", 42); err != nil {
		t.Fatal(err)
	}
	camp, price, ok, err := p.GetPrice(ctx, tid, "req-1", "prod-1")
	if err != nil || !ok || camp != "c1" || price != 42 {
		t.Fatalf("get price = (%s, %d, %v), want (c1, 42, true)", camp, price, ok)
	}
}

type fakeStore struct {
	creatives []ads.ActiveCreative
}

func (f *fakeStore) ListActiveCreatives(_ context.Context, _ tenant.ID) ([]ads.ActiveCreative, error) {
	return f.creatives, nil
}

func TestBlender(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	mr := miniredis.RunT(t)
	pacing := ads.NewRedisPacing(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

	store := &fakeStore{
		creatives: []ads.ActiveCreative{
			{
				CampaignID:       "camp_1",
				TenantID:         "demo",
				VendorID:         "v_1",
				DailyBudgetMinor: 5000,
				BidMinor:         100,
				StartAt:          now.Add(-time.Hour),
				ProductID:        "p3", // product p3 will be sponsored
			},
		},
	}
	blender := ads.NewBlender(store, pacing)

	cfg := tenant.AdsConfig{
		Enabled:        true,
		Slots:          []int{1}, // place at position 1 (index 0)
		RelevanceFloor: 0.1,
		Auction:        "gsp",
	}

	st := &pipeline.State{
		Resolved: &tenant.Resolved{Tenant: tenant.Tenant{ID: "demo"}},
		Query:    &pipeline.Query{RequestID: "req-123", Raw: "shoes"},
		Candidates: []pipeline.Candidate{
			{ProductID: "p1", Stage: pipeline.StageRank, Score: 0.9, Features: map[string]float64{feature.CTR30d: 0.05}},
			{ProductID: "p2", Stage: pipeline.StageRank, Score: 0.8, Features: map[string]float64{feature.CTR30d: 0.04}},
			{ProductID: "p3", Stage: pipeline.StageRank, Score: 0.7, Features: map[string]float64{feature.CTR30d: 0.03}},
			{ProductID: "p4", Stage: pipeline.StageRank, Score: 0.6, Features: map[string]float64{feature.CTR30d: 0.02}},
		},
	}

	blender.Blend(context.Background(), st, cfg, now)

	// p3 should be placed at slot 1 (index 0) with Sponsored = true, and removed from original position
	if len(st.Candidates) != 4 {
		t.Fatalf("expected 4 candidates after dedup, got %d", len(st.Candidates))
	}
	if st.Candidates[0].ProductID != "p3" || !st.Candidates[0].Sponsored {
		t.Errorf("expected candidate 0 to be p3 with Sponsored=true, got %+v", st.Candidates[0])
	}
	if st.Candidates[1].ProductID != "p1" || st.Candidates[1].Sponsored {
		t.Errorf("expected candidate 1 to be p1 (organic), got %+v", st.Candidates[1])
	}
	if st.Candidates[2].ProductID != "p2" || st.Candidates[2].Sponsored {
		t.Errorf("expected candidate 2 to be p2 (organic), got %+v", st.Candidates[2])
	}
	if st.Candidates[3].ProductID != "p4" || st.Candidates[3].Sponsored {
		t.Errorf("expected candidate 3 to be p4 (organic), got %+v", st.Candidates[3])
	}

	// Verify clearing price was recorded in Redis
	camp, price, ok, err := pacing.GetPrice(context.Background(), "demo", "req-123", "p3")
	if err != nil || !ok || camp != "camp_1" || price != 1 {
		t.Errorf("recorded price = (%s, %d, %v), want (camp_1, 1, true)", camp, price, ok)
	}
}

func TestBlenderDisabledNoop(t *testing.T) {
	now := time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
	store := &fakeStore{
		creatives: []ads.ActiveCreative{
			{CampaignID: "camp_1", ProductID: "p2", BidMinor: 100, StartAt: now.Add(-time.Hour)},
		},
	}
	blender := ads.NewBlender(store, nil)
	cfg := tenant.AdsConfig{Enabled: false, Slots: []int{1}}

	st := &pipeline.State{
		Resolved: &tenant.Resolved{Tenant: tenant.Tenant{ID: "demo"}},
		Query:    &pipeline.Query{RequestID: "req-1"},
		Candidates: []pipeline.Candidate{
			{ProductID: "p1", Stage: pipeline.StageRank, Score: 0.9},
			{ProductID: "p2", Stage: pipeline.StageRank, Score: 0.8},
		},
	}

	blender.Blend(context.Background(), st, cfg, now)

	if st.Candidates[0].ProductID != "p1" || st.Candidates[0].Sponsored {
		t.Errorf("candidate 0 changed when ads disabled: %+v", st.Candidates[0])
	}
	if st.Candidates[1].ProductID != "p2" || st.Candidates[1].Sponsored {
		t.Errorf("candidate 1 changed when ads disabled: %+v", st.Candidates[1])
	}
}

func rankedCandidates(ids ...string) []pipeline.Candidate {
	cs := make([]pipeline.Candidate, len(ids))
	for i, id := range ids {
		cs[i] = pipeline.Candidate{ProductID: id, Score: 1, Stage: pipeline.StageRank}
	}
	return cs
}

// A later winner's organic copy above an earlier ad must not shift that ad out of its slot.
func TestBlendHonorsEverySlot(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	st := &pipeline.State{
		Resolved:   &tenant.Resolved{Tenant: tenant.Tenant{ID: "t1"}},
		Query:      &pipeline.Query{},
		Candidates: rankedCandidates("a", "b", "c", "d", "e", "f", "g", "h"),
	}
	store := &fakeStore{creatives: []ads.ActiveCreative{
		{CampaignID: "x", BidMinor: 5000, DailyBudgetMinor: 1000, StartAt: now.Add(-time.Hour), ProductID: "g"},
		{CampaignID: "y", BidMinor: 4000, DailyBudgetMinor: 1000, StartAt: now.Add(-time.Hour), ProductID: "h"},
		{CampaignID: "z", BidMinor: 30, DailyBudgetMinor: 1000, StartAt: now.Add(-time.Hour), ProductID: "a"},
	}}
	ads.NewBlender(store, nil).Blend(context.Background(), st, tenant.AdsConfig{Enabled: true, Slots: []int{1, 3, 5}}, now)

	var got []string
	for i, c := range st.Candidates {
		got = append(got, c.ProductID)
		if want := i == 0 || i == 2 || i == 4; c.Sponsored != want {
			t.Errorf("position %d (%s): sponsored = %v, want %v", i+1, c.ProductID, c.Sponsored, want)
		}
	}
	if want := "g,b,h,c,a,d,e,f"; strings.Join(got, ",") != want {
		t.Errorf("order = %s, want %s", strings.Join(got, ","), want)
	}
}

// Candidates S5 did not rank (the pre-rank or RRF tail) are not eligible for ads.
func TestBlendIgnoresUnrankedTail(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cs := rankedCandidates("a", "b")
	cs = append(cs, pipeline.Candidate{ProductID: "tail", Score: 99, Stage: pipeline.StagePrerank})
	st := &pipeline.State{Resolved: &tenant.Resolved{Tenant: tenant.Tenant{ID: "t1"}}, Query: &pipeline.Query{}, Candidates: cs}
	store := &fakeStore{creatives: []ads.ActiveCreative{
		{CampaignID: "x", BidMinor: 5000, DailyBudgetMinor: 1000, StartAt: now.Add(-time.Hour), ProductID: "tail"},
	}}
	ads.NewBlender(store, nil).Blend(context.Background(), st, tenant.AdsConfig{Enabled: true, Slots: []int{1}}, now)
	for _, c := range st.Candidates {
		if c.Sponsored {
			t.Fatalf("%s was placed as an ad; only ranked candidates are eligible", c.ProductID)
		}
	}
}

func TestChargeSettlesAClickOnce(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	p := ads.NewRedisPacing(rdb)
	ctx, tid := context.Background(), tenant.ID("t1")
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

	if _, _, ok, err := p.Charge(ctx, tid, "req-1", "prod-1", now); err != nil || ok {
		t.Fatalf("charge without a served ad = (ok %v, err %v), want nothing", ok, err)
	}
	if err := p.RecordPrice(ctx, tid, "req-1", "prod-1", "c1", 42); err != nil {
		t.Fatal(err)
	}
	for range 3 { // the client resends the click
		camp, price, ok, err := p.Charge(ctx, tid, "req-1", "prod-1", now)
		if err != nil || !ok || camp != "c1" || price != 42 {
			t.Fatalf("charge = (%s, %d, %v, %v), want (c1, 42, true)", camp, price, ok, err)
		}
	}
	// Charged once, however often the click is resent.
	key := "f:{t1}:ad:spend:c1:20261027"
	if got, _ := rdb.Get(ctx, key).Int64(); got != 42 {
		t.Errorf("spend = %d, want 42", got)
	}
}
