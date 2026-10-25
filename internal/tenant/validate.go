package tenant

import (
	"context"
	"fmt"
	"slices"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/platform"
)

// EmbedderInfo describes what an embedder can do, for retriever availability checks.
type EmbedderInfo struct {
	Dim          int
	Image        bool
	TextToImage  bool
	TextEmbedder bool
}

// ValidationDeps supplies what validation needs beyond the document. Live probes of BYO
// endpoints (rules 3–5) are added in M5.
type ValidationDeps struct {
	Plan Plan
	// PlatformEmbedder describes a platform model ID; ok is false for unknown IDs.
	PlatformEmbedder func(id string) (EmbedderInfo, bool)
}

// Limits on config values.
const (
	MaxRetrieverK  = 2000
	MaxWeight      = 5
	MaxFacets      = 20
	MaxFacetSize   = 100
	MinRankKeep    = 10
	MaxPolicyRules = 50
)

// Validate checks every rule that does not need a live dependency (docs/TENANT_CONFIG.md §5)
// and returns all failures, not just the first.
func (c *Config) Validate(_ context.Context, deps ValidationDeps) []platform.FieldError {
	v := validator{}
	c.validateBudgets(&v, deps.Plan)
	v.add(catalog.ValidateMapping(c.Catalog.FieldMapping)...)
	v.add(catalog.ValidateCustomFields(c.Catalog.CustomFields, deps.Plan.MaxCustomFields)...)
	reg, err := catalog.NewRegistry(c.Catalog.CustomFields)
	if err != nil {
		reg, _ = catalog.NewRegistry(nil)
	}
	text, image := c.validateEmbedders(&v, deps)
	c.validateRetrieval(&v, text, image)
	c.validateFacets(&v, reg)
	c.validatePolicy(&v, reg)
	c.validateScorers(&v)
	c.validateAds(&v)
	return v.errs
}

type validator struct{ errs []platform.FieldError }

func (v *validator) add(errs ...platform.FieldError) { v.errs = append(v.errs, errs...) }

func (v *validator) fail(path, code, format string, args ...any) {
	v.errs = append(v.errs, platform.FieldError{Path: path, Code: code, Message: fmt.Sprintf(format, args...)})
}

func (c *Config) validateBudgets(v *validator, plan Plan) {
	validateBudgetSet(v, "/budgets", c.Budgets, plan)
	if c.Budgets.Image != nil {
		validateBudgetSet(v, "/budgets/image", c.Budgets.ForImage(), plan)
	}
}

func validateBudgetSet(v *validator, base string, b Budgets, plan Plan) {
	if b.TotalMS <= 0 {
		v.fail(base+"/total_ms", "out_of_range", "total_ms must be > 0")
	}
	if plan.MaxTotalMS > 0 && b.TotalMS > plan.MaxTotalMS {
		v.fail(base+"/total_ms", "exceeds_plan", "total_ms %d exceeds the %s plan maximum of %d", b.TotalMS, plan.ID, plan.MaxTotalMS)
	}
	sum := 0
	for i, s := range b.Stages() {
		if s <= 0 {
			v.fail(fmt.Sprintf("%s/s%d_ms", base, i+1), "out_of_range", "stage budgets must be > 0")
		}
		sum += s
	}
	if sum > b.TotalMS {
		v.fail(base, "stages_exceed_total", "stage budgets sum to %d ms, more than total_ms %d", sum, b.TotalMS)
	}
}

func (c *Config) validateEmbedders(v *validator, deps ValidationDeps) (text, image EmbedderInfo) {
	check := func(path string, e EmbedderConfig, needImage bool) EmbedderInfo {
		switch e.Provider {
		case ProviderPlatform:
			if deps.PlatformEmbedder == nil {
				return EmbedderInfo{}
			}
			info, ok := deps.PlatformEmbedder(e.Model)
			if !ok {
				v.fail(path+"/model", "unknown_model", "%q is not a platform model", e.Model)
				return EmbedderInfo{}
			}
			if needImage && !info.Image {
				v.fail(path+"/model", "unsupported", "%q cannot embed images", e.Model)
			}
			return info
		case ProviderBYO:
			if !deps.Plan.BYOAllowed {
				v.fail(path+"/provider", "plan_forbids_byo", "the %s plan does not allow BYO providers", deps.Plan.ID)
			}
			if e.ModelID == "" {
				v.fail(path+"/model_id", "required", "a BYO embedder needs model_id")
			}
			return EmbedderInfo{TextEmbedder: !needImage, Image: needImage}
		}
		v.fail(path+"/provider", "invalid", "provider must be platform or byo")
		return EmbedderInfo{}
	}
	text = check("/embedders/text", c.Embedders.Text, false)
	image = check("/embedders/image", c.Embedders.Image, true)
	for path, s := range map[string]StoreConfig{"/stores/lexical": c.Stores.Lexical, "/stores/vector": c.Stores.Vector} {
		switch s.Provider {
		case ProviderPlatform:
		case ProviderBYO:
			if !deps.Plan.BYOAllowed {
				v.fail(path+"/provider", "plan_forbids_byo", "the %s plan does not allow BYO providers", deps.Plan.ID)
			}
			if s.ConnectionID == "" {
				v.fail(path+"/connection_id", "required", "a BYO store needs connection_id")
			}
		default:
			v.fail(path+"/provider", "invalid", "provider must be platform or byo")
		}
	}
	return text, image
}

func (c *Config) validateRetrieval(v *validator, text, image EmbedderInfo) {
	r := c.Retrieval
	if r.RRFK <= 0 {
		v.fail("/retrieval/rrf_k", "out_of_range", "rrf_k must be > 0")
	}
	if r.EF < 0 || r.EF > 4096 {
		v.fail("/retrieval/ef", "out_of_range", "ef must be within [0, 4096]")
	}
	for f, w := range r.FieldBoosts {
		if w <= 0 || w > 100 {
			v.fail("/retrieval/field_boosts/"+f, "out_of_range", "boosts must be within (0, 100]")
		}
	}
	allowed := map[string]map[string]bool{
		"text": {RetrieverLexicalBM25: true, RetrieverVectorText: true, RetrieverVectorTextToImage: true, RetrieverCategoryPopular: true},
		"image": {RetrieverVectorImage: true, RetrieverLexicalBM25: true, RetrieverVectorText: true,
			RetrieverVectorTextToImage: true, RetrieverCategoryPopular: true},
	}
	for qt, qr := range map[string]QueryTypeRetrieval{"text": r.Text, "image": r.Image} {
		base := "/retrieval/" + qt + "/retrievers"
		if len(qr.Retrievers) == 0 {
			v.fail(base, "required", "at least one retriever is required")
		}
		seen := map[string]bool{}
		for i, rt := range qr.Retrievers {
			p := fmt.Sprintf("%s/%d", base, i)
			if !allowed[qt][rt.Name] {
				v.fail(p+"/name", "unknown_retriever", "%q is not a %s retriever", rt.Name, qt)
				continue
			}
			if seen[rt.Name] {
				v.fail(p+"/name", "duplicate", "%q is listed twice", rt.Name)
			}
			seen[rt.Name] = true
			if rt.K < 1 || rt.K > MaxRetrieverK {
				v.fail(p+"/k", "out_of_range", "k must be within [1, %d]", MaxRetrieverK)
			}
			if rt.Weight <= 0 || rt.Weight > MaxWeight {
				v.fail(p+"/weight", "out_of_range", "weight must be within (0, %d]", MaxWeight)
			}
			if rt.Name == RetrieverVectorTextToImage && image.Dim > 0 && !image.TextToImage {
				v.fail(p+"/name", "unavailable", "the image embedder has no text tower, so vector_text_to_image is unavailable")
			}
		}
	}
}

func (c *Config) validateFacets(v *validator, reg *catalog.Registry) {
	if len(c.Facets) > MaxFacets {
		v.fail("/facets", "too_many", "at most %d facets", MaxFacets)
	}
	for i, f := range c.Facets {
		p := fmt.Sprintf("/facets/%d", i)
		fld, ok := reg.Field(f.Field)
		if !ok || !fld.Facetable {
			v.fail(p+"/field", "not_facetable", "%q is not a facetable field", f.Field)
			continue
		}
		numeric := fld.Type == catalog.TypeInt || fld.Type == catalog.TypeFloat
		if numeric && len(f.Ranges) == 0 {
			v.fail(p+"/ranges", "required", "numeric facets need ranges")
		}
		if !numeric && len(f.Ranges) > 0 {
			v.fail(p+"/ranges", "unsupported", "ranges only apply to numeric fields")
		}
		if f.Size < 0 || f.Size > MaxFacetSize {
			v.fail(p+"/size", "out_of_range", "size must be within [0, %d]", MaxFacetSize)
		}
	}
}

func (c *Config) validatePolicy(v *validator, reg *catalog.Registry) {
	for i, f := range c.Policy.AlwaysFilters {
		_, errs := filter.Compile(f, reg, fmt.Sprintf("/policy/always_filters/%d", i))
		v.add(errs...)
	}
	if len(c.Policy.Rules) > MaxPolicyRules {
		v.fail("/policy/rules", "too_many", "at most %d rules", MaxPolicyRules)
	}
	ids := map[string]bool{}
	for i, r := range c.Policy.Rules {
		p := fmt.Sprintf("/policy/rules/%d", i)
		if r.ID == "" || ids[r.ID] {
			v.fail(p+"/id", "invalid", "rule ids must be non-empty and unique")
		}
		ids[r.ID] = true
		if r.Kind != "filter" && r.Kind != "boost" {
			v.fail(p+"/kind", "invalid", "kind must be filter or boost")
		}
		if len(r.Expr) == 0 || len(r.Expr) > 2048 {
			v.fail(p+"/expr", "invalid", "expr must be 1 to 2048 bytes")
		}
	}
}

func (c *Config) validateScorers(v *validator) {
	cat := feature.Catalog()
	for path, s := range map[string]StageScorerConfig{"/prerank": c.Prerank, "/rank": c.Rank} {
		switch s.Scorer.Type {
		case ScorerDefaultLinear:
			for name, w := range s.Scorer.Weights {
				src, ok := cat[name]
				switch {
				case !ok:
					v.fail(path+"/scorer/weights/"+name, "unknown_feature", "%q is not in the feature catalog", name)
				case src == feature.SourceUser:
					v.fail(path+"/scorer/weights/"+name, "unavailable", "user features are only available to the re-ranker")
				case w < 0 || w > MaxWeight:
					v.fail(path+"/scorer/weights/"+name, "out_of_range", "weights must be within [0, %d]", MaxWeight)
				}
			}
		case ScorerONNX, ScorerKServeV2:
			if s.Scorer.ModelID == "" {
				v.fail(path+"/scorer/model_id", "required", "%s scorers need model_id", s.Scorer.Type)
			}
		default:
			v.fail(path+"/scorer/type", "invalid", "type must be default_linear, onnx or kserve_v2")
		}
	}
	if c.Rank.Keep < MinRankKeep {
		v.fail("/rank/keep", "out_of_range", "rank.keep must be >= %d", MinRankKeep)
	}
	if c.Prerank.Keep < c.Rank.Keep {
		v.fail("/prerank/keep", "out_of_range", "prerank.keep must be >= rank.keep")
	}
}

func (c *Config) validateAds(v *validator) {
	a := c.Ads
	if !slices.IsSorted(a.Slots) || len(slices.Compact(slices.Clone(a.Slots))) != len(a.Slots) {
		v.fail("/ads/slots", "invalid", "slots must be strictly increasing")
	}
	for i, s := range a.Slots {
		if s < 1 || s > c.Rank.Keep {
			v.fail(fmt.Sprintf("/ads/slots/%d", i), "out_of_range", "slots must be within [1, rank.keep]")
		}
	}
	if a.RelevanceFloor < 0 || a.RelevanceFloor > 1 {
		v.fail("/ads/relevance_floor", "out_of_range", "relevance_floor must be within [0, 1]")
	}
	if a.Auction != "" && a.Auction != "gsp" {
		v.fail("/ads/auction", "invalid", "auction must be gsp")
	}
}
