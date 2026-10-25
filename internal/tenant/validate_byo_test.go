package tenant

import (
	"context"
	"testing"

	"github.com/zavisoft/search-engine/internal/platform"
)

// fakeChecker answers like the control plane's live checker.
type fakeChecker struct {
	models      map[string]EmbedderInfo
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
