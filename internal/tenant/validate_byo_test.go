package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/zavisoft/search-engine/internal/platform"
)

// fakeChecker answers like the control plane's live checker.
type fakeChecker struct {
	models      map[string]EmbedderInfo
	scorers     map[string]ScorerInfo
	storeErrs   []platform.FieldError
	gotFields   []string
	storeCalled bool
}

func (f *fakeChecker) Embedder(_ context.Context, id string) (EmbedderInfo, []platform.FieldError) {
	info, ok := f.models[id]
	if !ok {
		return EmbedderInfo{}, []platform.FieldError{{Path: "/model_id", Code: "not_found", Message: "no such model"}}
	}
	return info, nil
}

func (f *fakeChecker) VectorStore(_ context.Context, _ StoreConfig, fields []string) []platform.FieldError {
	f.storeCalled, f.gotFields = true, fields
	return f.storeErrs
}

func (f *fakeChecker) Scorer(_ context.Context, id, kind string) (ScorerInfo, []platform.FieldError) {
	info, ok := f.scorers[kind+"/"+id]
	if !ok {
		return ScorerInfo{}, []platform.FieldError{{Path: "/model_id", Code: "not_found", Message: "no such " + kind + " model"}}
	}
	return info, nil
}

func byoConfig(t *testing.T) *Config {
	c := defaultConfig(t)
	c.Embedders.Text = EmbedderConfig{Provider: ProviderBYO, ModelID: "text768"}
	c.Embedders.Image = EmbedderConfig{Provider: ProviderBYO, ModelID: "img512"}
	c.Stores.Vector = StoreConfig{Provider: ProviderBYO, ConnectionID: "conn", Collection: "products",
		Vectors:        map[string]VectorSpec{"text": {Name: "t", Dim: 768, Metric: "cosine"}, "image": {Name: "i", Dim: 512, Metric: "dot"}},
		PayloadMapping: map[string]string{"product_id": "pid"}}
	return c
}

func TestValidateBYOProviders(t *testing.T) {
	checker := &fakeChecker{models: map[string]EmbedderInfo{
		"text768":      {Dim: 768, TextEmbedder: true},
		"img512":       {Dim: 512, Image: true, TextToImage: true},
		"img-no-tower": {Dim: 512, Image: true},
	}}
	deps := ValidationDeps{Plan: growth, PlatformEmbedder: platformModels, BYO: checker}
	if errs := byoConfig(t).Validate(context.Background(), deps); len(errs) > 0 {
		t.Fatalf("valid BYO config: %+v", errs)
	}
	if !checker.storeCalled || len(checker.gotFields) != 2 || checker.gotFields[0] != "status" || checker.gotFields[1] != "stock" {
		t.Errorf("always_filters fields must be pushed down: %v", checker.gotFields)
	}

	c := byoConfig(t)
	c.Embedders.Text.ModelID = "missing"
	c.Embedders.Image.ModelID = "img-no-tower"
	c.Stores.Vector.Vectors = map[string]VectorSpec{"text": {Dim: 384, Metric: "hamming"}}
	c.Stores.Vector.Collection = ""
	c.Stores.Lexical.Provider = ProviderBYO
	checker.storeErrs = []platform.FieldError{{Path: "/vectors/text/dim", Code: "dim_mismatch"}}
	got := codes(c.Validate(context.Background(), deps))
	for path, code := range map[string]string{
		"/embedders/text/model_id":           "not_found",
		"/stores/vector/collection":          "required",
		"/stores/vector/vectors/text/metric": "unsupported_metric",
		"/stores/vector/vectors/image":       "required",
		"/stores/lexical/provider":           "byo_unavailable",
		"/retrieval/text/retrievers/2/name":  "unavailable",
	} {
		if got[path] != code {
			t.Errorf("%s = %q, want %q (all: %v)", path, got[path], code, got)
		}
	}

	c = byoConfig(t)
	c.Stores.Vector.Vectors["text"] = VectorSpec{Dim: 384, Metric: "cosine"}
	if got := codes(c.Validate(context.Background(), deps)); got["/stores/vector/vectors/text/dim"] != "dim_mismatch" {
		t.Errorf("rule 3: embedder dim must equal store dim: %v", got)
	}
	c = byoConfig(t)
	c.Embedders.Image.ModelID = "text768"
	if got := codes(c.Validate(context.Background(), deps)); got["/embedders/image/model_id"] != "unsupported" {
		t.Errorf("a text model cannot be the image embedder: %v", got)
	}
	checker.storeErrs = []platform.FieldError{{Path: "/connection_id", Code: "connection_not_validated"}}
	if got := codes(byoConfig(t).Validate(context.Background(), deps)); got["/stores/vector/connection_id"] != "connection_not_validated" {
		t.Errorf("checker errors are prefixed with the block path: %v", got)
	}
	starter := deps
	starter.Plan = Plan{ID: "starter", MaxTotalMS: 300, BYOAllowed: false}
	if got := codes(byoConfig(t).Validate(context.Background(), starter)); got["/stores/vector/provider"] != "plan_forbids_byo" || got["/embedders/text/provider"] != "plan_forbids_byo" {
		t.Errorf("plans without BYO: %v", got)
	}
}

func TestValidateLearnedScorers(t *testing.T) {
	platformScorers := func(_ context.Context, id string) (ScorerInfo, error) {
		switch id {
		case "prerank-gbdt-v1":
			return ScorerInfo{Type: ScorerONNX, Features: []string{"rrf_score", "ctr_7d"}}, nil
		case "rank-gbdt-v1":
			return ScorerInfo{Type: ScorerKServeV2, Features: []string{"rrf_score", "vendor_score"}}, nil
		}
		return ScorerInfo{}, errors.New(id + " has not been trained yet")
	}
	checker := &fakeChecker{scorers: map[string]ScorerInfo{
		"prerank/up1":  {Type: ScorerONNX, Features: []string{"bm25_norm"}},
		"rank/remote1": {Type: ScorerKServeV2, Features: []string{"rrf_score", "user_price_band_match"}},
	}}
	deps := ValidationDeps{Plan: growth, PlatformEmbedder: platformModels, PlatformScorer: platformScorers, BYO: checker}
	c := defaultConfig(t)
	c.Prerank.Scorer = ScorerConfig{Type: ScorerONNX, Model: "prerank-gbdt-v1"}
	c.Rank.Scorer = ScorerConfig{Type: ScorerKServeV2, Model: "rank-gbdt-v1"}
	if errs := c.Validate(context.Background(), deps); len(errs) > 0 {
		t.Fatalf("platform learned scorers: %+v", errs)
	}
	c.Prerank.Scorer = ScorerConfig{Type: ScorerONNX, ModelID: "up1"}
	if errs := c.Validate(context.Background(), deps); len(errs) > 0 {
		t.Fatalf("tenant ONNX prerank: %+v", errs)
	}

	for name, tc := range map[string]struct {
		prerank, rank ScorerConfig
		plan          Plan
		want          map[string]string
	}{
		"both set":        {prerank: ScorerConfig{Type: ScorerONNX, Model: "prerank-gbdt-v1", ModelID: "up1"}, want: map[string]string{"/prerank/scorer": "invalid"}},
		"neither":         {rank: ScorerConfig{Type: ScorerKServeV2}, want: map[string]string{"/rank/scorer": "invalid"}},
		"wrong type":      {prerank: ScorerConfig{Type: ScorerKServeV2, Model: "prerank-gbdt-v1"}, want: map[string]string{"/prerank/scorer/model": "wrong_type"}},
		"untrained":       {rank: ScorerConfig{Type: ScorerKServeV2, Model: "rank-gbdt-v2"}, want: map[string]string{"/rank/scorer/model": "model_unavailable"}},
		"wrong stage":     {rank: ScorerConfig{Type: ScorerONNX, ModelID: "up1"}, want: map[string]string{"/rank/scorer/model_id": "not_found"}},
		"user features":   {rank: ScorerConfig{Type: ScorerKServeV2, ModelID: "remote1"}, want: map[string]string{"/rank/scorer/model_id": "unavailable"}},
		"weights":         {rank: ScorerConfig{Type: ScorerKServeV2, Model: "rank-gbdt-v1", Weights: map[string]float64{"x": 1}}, want: map[string]string{"/rank/scorer/weights": "invalid"}},
		"starter plan":    {prerank: ScorerConfig{Type: ScorerONNX, ModelID: "up1"}, plan: Plan{ID: "starter", MaxTotalMS: 300, MaxPageDepth: 500}, want: map[string]string{"/prerank/scorer/model_id": "plan_forbids_byo"}},
		"linear w/ model": {rank: ScorerConfig{Type: ScorerDefaultLinear, Model: "rank-gbdt-v1"}, want: map[string]string{"/rank/scorer": "invalid"}},
	} {
		c := defaultConfig(t)
		if tc.prerank.Type != "" {
			c.Prerank.Scorer = tc.prerank
		}
		if tc.rank.Type != "" {
			c.Rank.Scorer = tc.rank
		}
		d := deps
		if tc.plan.ID != "" {
			d.Plan = tc.plan
		}
		got := map[string]string{}
		for _, e := range c.Validate(context.Background(), d) {
			got[e.Path] = e.Code
		}
		for path, code := range tc.want {
			if got[path] != code {
				t.Errorf("%s: %s = %q, want %q (all: %v)", name, path, got[path], code, got)
			}
		}
	}
}
