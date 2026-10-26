package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/platform"
)

var growth = Plan{ID: "growth", MaxTotalMS: 600, MaxCustomFields: 2, BYOAllowed: true, MaxPageDepth: 1000}

func platformModels(id string) (EmbedderInfo, bool) {
	switch id {
	case "bge-small-en-v1.5":
		return EmbedderInfo{Dim: 384, TextEmbedder: true}, true
	case "clip-vit-b32":
		return EmbedderInfo{Dim: 512, Image: true, TextEmbedder: true, TextToImage: true}, true
	case "image-only":
		return EmbedderInfo{Dim: 512, Image: true}, true
	}
	return EmbedderInfo{}, false
}

func defaultConfig(t *testing.T) *Config {
	t.Helper()
	c, err := ParseConfig(DefaultDocument())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func codes(errs []platform.FieldError) map[string]string {
	m := map[string]string{}
	for _, e := range errs {
		m[e.Path] = e.Code
	}
	return m
}

func TestDefaultConfigIsValid(t *testing.T) {
	errs := defaultConfig(t).Validate(context.Background(), ValidationDeps{Plan: growth, PlatformEmbedder: platformModels})
	if len(errs) > 0 {
		t.Fatalf("default config invalid: %+v", errs)
	}
}

func TestValidateReportsEveryRule(t *testing.T) {
	c := defaultConfig(t)
	c.Budgets.TotalMS = 900
	c.Budgets.S2MS = 800
	c.Embedders.Image.Model = "image-only"
	c.Embedders.Text = EmbedderConfig{Provider: ProviderBYO}
	c.Retrieval.RRFK = 0
	c.Retrieval.Text.Retrievers = append(c.Retrieval.Text.Retrievers,
		RetrieverConfig{Name: "magic", K: 10, Weight: 1}, RetrieverConfig{Name: RetrieverLexicalBM25, K: 5000, Weight: 0})
	c.Facets = []FacetConfig{{Field: "title"}, {Field: "price_minor"}}
	c.Policy.AlwaysFilters[0].Cond.Field = "colour"
	c.Policy.Rules = []PolicyRule{{ID: "r", Kind: "magic", Expr: "true"}, {ID: "r", Kind: "filter", Expr: ""}}
	c.Rank.Scorer.Weights["user_category_affinity"] = 1
	c.Rank.Scorer.Weights["made_up"] = 1
	c.Rank.Scorer.Weights["rating"] = 9
	c.Rank.Keep = 5
	c.Prerank.Scorer = ScorerConfig{Type: ScorerONNX}
	c.Ads.Slots = []int{5, 1, 500}
	c.Ads.RelevanceFloor = 2
	c.Catalog.CustomFields = []catalog.CustomField{
		{Name: "a", Type: catalog.TypeKeyword, Source: "$.a"}, {Name: "b", Type: catalog.TypeKeyword, Source: "$.b"},
		{Name: "c", Type: catalog.TypeKeyword, Source: "$.c"},
	}

	got := codes(c.Validate(context.Background(), ValidationDeps{Plan: Plan{ID: "starter", MaxTotalMS: 300, MaxCustomFields: 2}, PlatformEmbedder: platformModels}))
	want := map[string]string{
		"/budgets/total_ms":                           "exceeds_plan",
		"/budgets":                                    "stages_exceed_total",
		"/embedders/text/provider":                    "plan_forbids_byo",
		"/embedders/text/model_id":                    "required",
		"/retrieval/rrf_k":                            "out_of_range",
		"/retrieval/text/retrievers/2/name":           "unavailable",
		"/retrieval/text/retrievers/3/name":           "unknown_retriever",
		"/retrieval/text/retrievers/4/name":           "duplicate",
		"/retrieval/text/retrievers/4/k":              "out_of_range",
		"/retrieval/text/retrievers/4/weight":         "out_of_range",
		"/facets/0/field":                             "not_facetable",
		"/facets/1/ranges":                            "required",
		"/policy/always_filters/0/field":              "unknown_field",
		"/policy/rules/0/kind":                        "invalid",
		"/policy/rules/1/id":                          "invalid",
		"/policy/rules/1/expr":                        "invalid",
		"/rank/scorer/weights/user_category_affinity": "unavailable",
		"/rank/scorer/weights/made_up":                "unknown_feature",
		"/rank/scorer/weights/rating":                 "out_of_range",
		"/rank/keep":                                  "out_of_range",
		"/prerank/scorer":                             "invalid",
		"/ads/slots":                                  "invalid",
		"/ads/slots/2":                                "out_of_range",
		"/ads/relevance_floor":                        "out_of_range",
		"/catalog/custom_fields":                      "too_many",
	}
	for path, code := range want {
		if got[path] != code {
			t.Errorf("%s: got %q, want %q", path, got[path], code)
		}
	}
}

func TestEffectiveDefaults(t *testing.T) {
	c := defaultConfig(t)
	f := c.EffectiveFacets()
	if len(f) != 4 || f[2].Field != "price_minor" || len(f[2].Ranges) != 6 || *f[2].Ranges[0].To != 2500 {
		t.Errorf("default facets = %+v", f)
	}
	if c.EffectiveEF() != 128 {
		t.Error("default ef must be 128")
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseConfig(b); err != nil {
		t.Errorf("a parsed config must round-trip: %v", err)
	}
}

type fakeSource struct {
	loads   int
	tenant  Tenant
	version int
	shadow  int
	err     error
}

func (f *fakeSource) Tenant(context.Context, ID) (Tenant, error) { f.loads++; return f.tenant, f.err }
func (f *fakeSource) Plan(context.Context, string) (Plan, error) { return growth, nil }
func (f *fakeSource) ActiveConfig(context.Context, ID) (int, *Config, error) {
	c, err := ParseConfig(DefaultDocument())
	return f.version, c, err
}
func (f *fakeSource) ShadowConfig(context.Context, ID) (int, float64, *Config, error) {
	if f.shadow == 0 {
		return 0, 0, nil, ErrNotFound
	}
	c, err := ParseConfig(DefaultDocument())
	return f.shadow, 0.1, c, err
}
func (f *fakeSource) ConfigVersion(_ context.Context, _ ID, v int) (*Config, error) {
	if v > f.version {
		return nil, ErrNotFound
	}
	return ParseConfig(DefaultDocument())
}
func (f *fakeSource) Draft(context.Context, ID) (*Config, error) {
	return ParseConfig(DefaultDocument())
}
func (f *fakeSource) LiveVersions(context.Context) ([]LiveVersion, error) {
	out := []LiveVersion{{Tenant: "T", Version: f.version}}
	if f.shadow > 0 {
		out = append(out, LiveVersion{Tenant: "T", Version: f.shadow, Shadow: true, Rate: 0.1})
	}
	return out, nil
}

func TestResolverCachesAndInvalidates(t *testing.T) {
	src := &fakeSource{tenant: Tenant{ID: "T", Plan: "growth", Status: "active"}, version: 1}
	r := NewResolver(src, time.Minute)
	now := time.Now()
	r.now = func() time.Time { return now }
	res, err := r.Get(context.Background(), "T")
	if err != nil {
		t.Fatal(err)
	}
	if res.AlwaysFilters.IsZero() || res.Registry == nil || res.Plan.ID != "growth" {
		t.Errorf("resolved = %+v", res)
	}
	_, _ = r.Get(context.Background(), "T")
	if src.loads != 1 {
		t.Errorf("loads = %d, want 1 (cached)", src.loads)
	}
	now = now.Add(2 * time.Minute)
	src.version = 2
	res, _ = r.Get(context.Background(), "T")
	if src.loads != 2 || res.ConfigVersion != 2 {
		t.Errorf("expired entry must reload: loads=%d v=%d", src.loads, res.ConfigVersion)
	}
	r.Invalidate("T")
	_, _ = r.Get(context.Background(), "T")
	if src.loads != 3 {
		t.Error("Invalidate must force a reload")
	}

	src.tenant.Status = "suspended"
	r.Invalidate("T")
	if _, err := r.Get(context.Background(), "T"); !errors.Is(err, ErrNotFound) {
		t.Errorf("inactive tenant: %v", err)
	}
}

func TestImageBudgets(t *testing.T) {
	c := defaultConfig(t)
	img := c.Budgets.ForImage()
	if img.S1MS != 130 || img.S2MS != 80 || img.TotalMS != 300 || img.Image != nil {
		t.Errorf("image budgets = %+v", img)
	}
	if b := (Budgets{TotalMS: 100, S1MS: 10}).ForImage(); b.S1MS != 10 || b.TotalMS != 100 {
		t.Errorf("without overrides the text budgets apply: %+v", b)
	}
	c.Budgets.Image.S1MS = 400
	got := codes(c.Validate(context.Background(), ValidationDeps{Plan: growth, PlatformEmbedder: platformModels}))
	if got["/budgets/image"] != "stages_exceed_total" {
		t.Errorf("image stage budgets above the total must fail: %v", got)
	}
	c.Budgets.Image.TotalMS = 900
	got = codes(c.Validate(context.Background(), ValidationDeps{Plan: growth, PlatformEmbedder: platformModels}))
	if got["/budgets/image/total_ms"] != "exceeds_plan" {
		t.Errorf("image total above the plan maximum must fail: %v", got)
	}
}

func TestResolverShadowPinnedAndReconcile(t *testing.T) {
	src := &fakeSource{tenant: Tenant{ID: "T", Plan: "growth", Status: "active"}, version: 2, shadow: 3}
	r := NewResolver(src, time.Hour)
	ctx := context.Background()
	res, err := r.Get(ctx, "T")
	if err != nil || res.Shadow == nil || res.Shadow.ConfigVersion != 3 || res.ShadowRate != 0.1 {
		t.Fatalf("shadow not resolved: %+v %v", res, err)
	}
	if a, sh, rate, ok := r.Cached("T"); !ok || a != 2 || sh != 3 || rate != 0.1 {
		t.Errorf("cached = %d %d %v %v", a, sh, rate, ok)
	}
	if stale, err := r.Reconcile(ctx); err != nil || len(stale) != 0 {
		t.Errorf("nothing changed, nothing is stale: %v %v", stale, err)
	}
	src.version, src.shadow = 3, 0
	stale, _ := r.Reconcile(ctx)
	if len(stale) != 1 {
		t.Fatalf("activation of v3 must be reconciled: %v", stale)
	}
	res, _ = r.Get(ctx, "T")
	if res.ConfigVersion != 3 || res.Shadow != nil {
		t.Errorf("after reconcile = v%d shadow %v", res.ConfigVersion, res.Shadow)
	}

	pinned, err := r.Pinned(ctx, "T", "2")
	if err != nil || pinned.ConfigVersion != 2 || pinned.Draft {
		t.Errorf("pinned v2 = %+v %v", pinned, err)
	}
	draft, err := r.Pinned(ctx, "T", "draft")
	if err != nil || !draft.Draft || draft.ConfigVersion != 0 {
		t.Errorf("draft = %+v %v", draft, err)
	}
	if _, err := r.Pinned(ctx, "T", "9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown version: %v", err)
	}
	if _, err := r.Pinned(ctx, "T", "x"); err == nil {
		t.Error("a bad reference must fail")
	}
}
