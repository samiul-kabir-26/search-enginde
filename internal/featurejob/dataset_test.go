package featurejob

import (
	"context"
	"slices"
	"testing"

	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/providers/features"
)

func TestDatasetColumns(t *testing.T) {
	cols := Columns()
	if !slices.Equal(cols[:9], []string{"request_id", "ts", "split", "product_id", "position", "label", "randomized", "baseline_prerank", "baseline_rank"}) {
		t.Errorf("leading columns %v", cols[:9])
	}
	feats := cols[9:]
	cat := feature.Catalog()
	for _, f := range feats {
		if cat[f] == feature.SourceUser {
			t.Errorf("user feature %s must not be a training column (re-rank only)", f)
		}
	}
	if len(feats) != len(feature.Names())-2 || feats[0] != feature.RRFScore || !slices.Contains(feats, feature.PolicyBoost) {
		t.Errorf("features %v", feats)
	}
}

func TestMemStore(t *testing.T) {
	m := &memStore{items: map[string]features.Item{}}
	ctx := context.Background()
	if _, ok, _ := m.GetPropensity(ctx, "t"); ok {
		t.Error("empty")
	}
	_ = m.PutPropensity(ctx, "t", features.Propensity{Eta: 0.7})
	_ = m.PutItems(ctx, "t", map[string]features.Item{"p": {CTR7d: 0.2}})
	if p, ok, _ := m.GetPropensity(ctx, "t"); !ok || p.Eta != 0.7 || m.items["p"].CTR7d != 0.2 {
		t.Errorf("store %+v", m)
	}
}
