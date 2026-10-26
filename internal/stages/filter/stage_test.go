package filter_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/policy"
	"github.com/zavisoft/search-engine/internal/stages/filter"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
)

type vendors map[string]map[string]any

func (v vendors) Read(_ context.Context, _ tenant.ID, req pipeline.FeatureRequest) (*pipeline.OnlineFeatures, error) {
	out := &pipeline.OnlineFeatures{VendorAttrs: map[string]map[string]any{}}
	for _, id := range req.VendorIDs {
		if a, ok := v[id]; ok {
			out.VendorAttrs[id] = a
		}
	}
	return out, nil
}

func state(t *testing.T, rules ...policy.Rule) *pipeline.State {
	t.Helper()
	set, errs := policy.Compile(rules, nil)
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	p, _, _ := stagestest.Providers()
	p.Policy = set
	st := &pipeline.State{Resolved: stagestest.Resolved(), Providers: p, Query: &pipeline.Query{Text: "red bag", Type: pipeline.QueryText}}
	for _, c := range stagestest.Catalog() {
		stock := c.Stock
		st.Candidates = append(st.Candidates, pipeline.Candidate{ProductID: c.ID, Doc: &pipeline.Doc{
			Brand: c.Brand, VendorID: "v_" + strings.ToLower(c.Brand[:1]), PriceMinor: c.PriceMinor, CategoryIDs: c.Categories, Stock: &stock,
		}})
	}
	return st
}

func ids(cs []pipeline.Candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ProductID
	}
	return out
}

func TestRulesFilterAndBoost(t *testing.T) {
	st := state(t,
		policy.Rule{ID: "verified-bags", Kind: policy.KindFilter, Expr: "!item.category_ids.exists(c, c == 'bags') || item.vendor.verified"},
		policy.Rule{ID: "cheap", Kind: policy.KindBoost, Expr: "item.price_minor < 2000 ? 0.3 : 0.0"},
		policy.Rule{ID: "fast", Kind: policy.KindBoost, Expr: "item.vendor.on_time_dispatch_rate > 0.95 ? 0.5 : 0.0"},
	)
	rep, err := filter.New(stagestest.Breakers(), vendors{"v_r": {"verified": true, "on_time_dispatch_rate": 0.99}}).Run(context.Background(), st)
	if err != nil || rep.Fallback || rep.Provider != "cel" {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	// Bags survive only from Rivet (v_r, verified); vendors never sent use the defaults (not verified).
	want := "p01,p02,p05,p06,p07,p09"
	if got := strings.Join(ids(st.Candidates), ","); got != want {
		t.Errorf("kept %s, want %s (order preserved)", got, want)
	}
	boosts := map[string]float64{}
	for _, c := range st.Candidates {
		boosts[c.ProductID] = c.PolicyBoost
	}
	if boosts["p05"] != 0.8 || boosts["p01"] != 0.5 || boosts["p07"] != 0.3 || boosts["p06"] != 0 {
		t.Errorf("boosts %v", boosts)
	}
}

func TestRuleErrorsAndDeadline(t *testing.T) {
	st := state(t, policy.Rule{ID: "rating", Kind: policy.KindFilter, Expr: "item.rating > 4.5"})
	rep, _ := filter.New(stagestest.Breakers(), nil).Run(context.Background(), st)
	if len(st.Candidates) != 10 || !rep.Fallback || !strings.Contains(rep.FallbackReason, "cel:errors(10)") {
		t.Errorf("a rule on a missing field keeps every item and is reported: %d %+v", len(st.Candidates), rep)
	}

	st = state(t, policy.Rule{ID: "none", Kind: policy.KindFilter, Expr: "false"})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	rep, _ = filter.New(stagestest.Breakers(), nil).Run(ctx, st)
	if len(st.Candidates) != 10 || !strings.Contains(rep.FallbackReason, "cel:timeout(10 unevaluated)") {
		t.Errorf("past the deadline candidates pass unevaluated: %d %+v", len(st.Candidates), rep)
	}
}

func TestNoRulesIsPushdownOnly(t *testing.T) {
	st := state(t)
	st.Providers.Policy = nil
	rep, _ := filter.New(stagestest.Breakers(), nil).Run(context.Background(), st)
	if rep.Provider != "pushdown" || len(st.Candidates) != 10 || st.Online != nil {
		t.Errorf("rep %+v; no rules means no work and no feature read", rep)
	}
}

func TestItemVars(t *testing.T) {
	r, s := 4.5, int64(2)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	item := filter.ItemVars(&pipeline.Doc{VendorID: "v1", Rating: &r, Stock: &s, CreatedAt: &at, Custom: map[string]any{"season": "summer"}}, "p1",
		&pipeline.OnlineFeatures{VendorAttrs: map[string]map[string]any{"v1": {"score": 0.7}}})
	v := item["vendor"].(map[string]any)
	if item["rating"] != 4.5 || item["stock"] != int64(2) || item["created_at"] != at || v["score"] != 0.7 || v["verified"] != false {
		t.Errorf("item %v", item)
	}
	if v["response_hours"] != 9.6 {
		t.Errorf("default response hours are the declared feature default in hours: %v", v["response_hours"])
	}
	bare := filter.ItemVars(&pipeline.Doc{}, "p2", nil)
	if _, ok := bare["rating"]; ok {
		t.Error("absent optional fields stay absent so has() works")
	}
	if q := filter.QueryVars(&pipeline.Query{Text: "x", Categories: []pipeline.CategoryGuess{{ID: "bags"}}}); q["categories"].([]string)[0] != "bags" {
		t.Errorf("query %v", q)
	}
}
