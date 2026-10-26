package prerank_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/stages/prerank"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// reader returns fixed features, an error, or blocks until its context ends.
type reader struct {
	out   *pipeline.OnlineFeatures
	err   error
	block bool
	got   pipeline.FeatureRequest
}

func (r *reader) Read(ctx context.Context, _ tenant.ID, req pipeline.FeatureRequest) (*pipeline.OnlineFeatures, error) {
	r.got = req
	if r.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.out, r.err
}

func state() *pipeline.State {
	p, _, _ := stagestest.Providers()
	st := &pipeline.State{Resolved: stagestest.Resolved(), Providers: p, Query: &pipeline.Query{User: pipeline.UserContext{ID: "u1"}}}
	for _, c := range stagestest.Catalog()[:4] {
		st.Candidates = append(st.Candidates, pipeline.Candidate{
			ProductID: c.ID, Stage: pipeline.StageRetrieval, RRF: 0.01,
			Doc: &pipeline.Doc{VendorID: "v_" + c.ID, PriceMinor: c.PriceMinor, CategoryIDs: c.Categories},
		})
	}
	return st
}

func TestOnlineFeaturesAreMerged(t *testing.T) {
	r := &reader{out: &pipeline.OnlineFeatures{
		Items:   map[string]map[string]float64{"p02": {feature.CTR7d: 0.9}},
		Vendors: map[string]map[string]float64{"v_p02": {feature.VendorScore: 0.8}},
		User:    &pipeline.UserFeatures{CategoryAffinity: map[string]float64{"backpacks": 0.4}, PriceLo: 2000, PriceHi: 5000},
	}}
	st := state()
	rep, err := prerank.New(stagestest.Breakers(), r).Run(context.Background(), st)
	if err != nil || rep.Fallback {
		t.Fatalf("rep %+v err %v", rep, err)
	}
	if len(r.got.ProductIDs) != 4 || len(r.got.VendorIDs) != 4 || r.got.UserID != "u1" {
		t.Errorf("request %+v", r.got)
	}
	byID := map[string]map[string]float64{}
	for _, c := range st.Candidates {
		byID[c.ProductID] = c.Features
	}
	if f := byID["p02"]; f[feature.CTR7d] != 0.9 || f[feature.VendorScore] != 0.8 || f[feature.UserPriceBandMatch] != 0 {
		t.Errorf("p02 = %v (7900 is outside 2000–5000)", f)
	}
	if f := byID["p04"]; f[feature.UserCategoryAffinity] != 0.4 || f[feature.UserPriceBandMatch] != 1 {
		t.Errorf("p04 = %v", f)
	}
	if _, ok := byID["p01"][feature.CTR7d]; ok {
		t.Error("candidates without stored features keep the declared default (unset)")
	}
	if byID["p01"][feature.RRFScore] == 0 {
		t.Error("request-local features must still be built")
	}
}

func TestFeatureReadFailureFallsBack(t *testing.T) {
	for name, r := range map[string]*reader{
		"error": {err: errors.New("redis down")},
		"hang":  {block: true},
	} {
		st := state()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		start := time.Now()
		rep, err := prerank.New(stagestest.Breakers(), r).Run(ctx, st)
		elapsed := time.Since(start)
		cancel()
		if err != nil || !rep.Fallback || !strings.HasPrefix(rep.FallbackReason, prerank.ProviderOnlineFeatures+":") {
			t.Errorf("%s: rep %+v err %v", name, rep, err)
		}
		if elapsed > 15*time.Millisecond {
			t.Errorf("%s: took %v; the read may use only half the stage budget", name, elapsed)
		}
		if st.Candidates[0].Stage != pipeline.StagePrerank {
			t.Errorf("%s: scoring must still run on defaults", name)
		}
	}
}
