package tenant

import (
	"context"
	"fmt"
	"slices"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/policy"
)

// EmbedderInfo describes what an embedder can do, for retriever availability checks.
type EmbedderInfo struct {
	Dim          int
	Image        bool
	TextToImage  bool
	TextEmbedder bool
}

// ValidationDeps supplies what validation needs beyond the document.
type ValidationDeps struct {
	Plan Plan
	// PlatformEmbedder describes a platform model ID; ok is false for unknown IDs.
	PlatformEmbedder func(id string) (EmbedderInfo, bool)
	// BYO resolves and live-probes registered models and connections (rules 3–5). When nil,
	// only the checks that need no lookup run.
	BYO BYOChecker
	// PlatformScorer describes a platform scorer model (docs/MODELS.md §2); it fails for
	// unknown IDs and for models not trained yet.
	PlatformScorer func(ctx context.Context, id string) (ScorerInfo, error)
}

// Limits on config values.
const (
	MaxRetrieverK  = 2000
	MaxWeight      = 5
	MaxFacets      = 20
	MaxFacetSize   = 100
	MinRankKeep    = 10
	MaxPolicyRules = policy.MaxRules
)

// Validate checks the rules of docs/TENANT_CONFIG.md §5, including the live BYO probes when
// deps.BYO is set, and returns all failures, not just the first.
func (c *Config) Validate(ctx context.Context, deps ValidationDeps) []platform.FieldError {
	v := validator{}
	c.validateBudgets(&v, deps.Plan)
	v.add(catalog.ValidateMapping(c.Catalog.FieldMapping)...)
	v.add(catalog.ValidateCustomFields(c.Catalog.CustomFields, deps.Plan.MaxCustomFields)...)
	v.add(catalog.ValidateVendorMapping(c.Catalog.VendorMapping)...)
	reg, err := catalog.NewRegistry(c.Catalog.CustomFields)
	if err != nil {
		reg, _ = catalog.NewRegistry(nil)
	}
	text, image := c.validateEmbedders(ctx, &v, deps)
	c.validateStores(ctx, &v, deps, text, image)
	c.validateRetrieval(&v, text, image)
	c.validateFacets(&v, reg)
	c.validatePolicy(&v, reg)
	c.validateScorers(ctx, &v, deps)
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

func prefixed(base string, errs []platform.FieldError) []platform.FieldError {
	out := make([]platform.FieldError, len(errs))
	for i, e := range errs {
		e.Path = base + e.Path
		out[i] = e
	}
	return out
}

func (c *Config) validateEmbedders(ctx context.Context, v *validator, deps ValidationDeps) (text, image EmbedderInfo) {
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
				return EmbedderInfo{}
			}
			if deps.BYO == nil {
				return EmbedderInfo{TextEmbedder: !needImage, Image: needImage}
			}
			info, errs := deps.BYO.Embedder(ctx, e.ModelID)
			v.add(prefixed(path, errs)...)
			if len(errs) > 0 {
				return EmbedderInfo{}
			}
			if needImage && !info.Image {
				v.fail(path+"/model_id", "unsupported", "model %s does not embed images", e.ModelID)
			}
			if !needImage && !info.TextEmbedder {
				v.fail(path+"/model_id", "unsupported", "model %s does not embed text", e.ModelID)
			}
			return info
		}
		v.fail(path+"/provider", "invalid", "provider must be platform or byo")
		return EmbedderInfo{}
	}
	text = check("/embedders/text", c.Embedders.Text, false)
	image = check("/embedders/image", c.Embedders.Image, true)
	return text, image
}

// Metrics BYO vectors may declare; adapters check what they support (rule 4).
var vectorMetrics = []string{"cosine", "dot", "l2"}

// neededVectors are the named vectors the configured retrievers search.
func (c *Config) neededVectors() map[string]bool {
	need := map[string]bool{}
	for _, list := range [][]RetrieverConfig{c.Retrieval.Text.Retrievers, c.Retrieval.Image.Retrievers} {
		for _, r := range list {
			switch r.Name {
			case RetrieverVectorText:
				need["text"] = true
			case RetrieverVectorImage, RetrieverVectorTextToImage:
				need["image"] = true
			}
		}
	}
	return need
}

// PushdownFields are the canonical fields a BYO vector store must hold so the always-on
// filters can be pushed down (rule 8).
func (c *Config) PushdownFields() []string {
	seen := map[string]bool{}
	var out []string
	var walk func(e filter.Expr)
	walk = func(e filter.Expr) {
		for _, k := range append(append([]filter.Expr{}, e.And...), e.Or...) {
			walk(k)
		}
		if e.Cond != nil && !seen[e.Cond.Field] {
			seen[e.Cond.Field] = true
			out = append(out, e.Cond.Field)
		}
	}
	for _, f := range c.Policy.AlwaysFilters {
		walk(f)
	}
	slices.Sort(out)
	return out
}

func (c *Config) validateStores(ctx context.Context, v *validator, deps ValidationDeps, text, image EmbedderInfo) {
	switch c.Stores.Lexical.Provider {
	case ProviderPlatform:
	case ProviderBYO:
		v.fail("/stores/lexical/provider", "byo_unavailable", "BYO lexical stores are not supported in v1; the lexical index is platform-hosted")
	default:
		v.fail("/stores/lexical/provider", "invalid", "provider must be platform or byo")
	}
	s := c.Stores.Vector
	switch s.Provider {
	case ProviderPlatform:
		return
	case ProviderBYO:
	default:
		v.fail("/stores/vector/provider", "invalid", "provider must be platform or byo")
		return
	}
	base := "/stores/vector"
	if !deps.Plan.BYOAllowed {
		v.fail(base+"/provider", "plan_forbids_byo", "the %s plan does not allow BYO providers", deps.Plan.ID)
	}
	if s.ConnectionID == "" {
		v.fail(base+"/connection_id", "required", "a BYO store needs connection_id")
	}
	if s.Collection == "" {
		v.fail(base+"/collection", "required", "a BYO store needs the collection (or table) name")
	}
	embedders := map[string]EmbedderInfo{"text": text, "image": image}
	need := c.neededVectors()
	for _, name := range []string{"text", "image"} {
		spec, ok := s.Vectors[name]
		p := base + "/vectors/" + name
		if !ok {
			if need[name] {
				v.fail(p, "required", "the configured retrievers search the %s vector, so it must be declared", name)
			}
			continue
		}
		if spec.Dim <= 0 {
			v.fail(p+"/dim", "out_of_range", "dim must be > 0")
		}
		if !slices.Contains(vectorMetrics, spec.Metric) {
			v.fail(p+"/metric", "unsupported_metric", "metric must be cosine, dot or l2")
		}
		if e := embedders[name]; e.Dim > 0 && spec.Dim > 0 && e.Dim != spec.Dim {
			v.fail(p+"/dim", "dim_mismatch", "the %s embedder outputs %d dimensions, the store declares %d", name, e.Dim, spec.Dim)
		}
	}
	for k := range s.PayloadMapping {
		if k == "" || s.PayloadMapping[k] == "" {
			v.fail(base+"/payload_mapping", "invalid", "payload_mapping keys and values must be non-empty")
			break
		}
	}
	if deps.BYO != nil && s.ConnectionID != "" && s.Collection != "" {
		v.add(prefixed(base, deps.BYO.VectorStore(ctx, s, c.PushdownFields()))...)
	}
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
	_, errs := policy.Compile(c.Policy.Rules, c.Catalog.CustomFieldNames())
	for _, e := range errs {
		p := "/policy/rules"
		if e.Index >= 0 {
			p = fmt.Sprintf("/policy/rules/%d/%s", e.Index, e.Field)
		}
		v.fail(p, e.Code, "%s", e.Message)
	}
}

func (c *Config) validateScorers(ctx context.Context, v *validator, deps ValidationDeps) {
	cat := feature.Catalog()
	for _, stage := range []struct {
		path, kind string
		cfg        StageScorerConfig
	}{{"/prerank", ModelKindPrerank, c.Prerank}, {"/rank", ModelKindRank, c.Rank}} {
		path, s := stage.path, stage.cfg.Scorer
		switch s.Type {
		case ScorerDefaultLinear:
			if s.Model != "" || s.ModelID != "" {
				v.fail(path+"/scorer", "invalid", "default_linear takes weights, not a model")
			}
			for name, w := range s.Weights {
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
			c.validateLearned(ctx, v, deps, path, stage.kind, s)
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

// validateLearned checks a learned scorer (rule 9): exactly one of a platform model or a
// tenant model of the stage's kind and the declared type, validated, whose features all
// resolve in the feature catalog and exclude user features (re-rank only).
func (c *Config) validateLearned(ctx context.Context, v *validator, deps ValidationDeps, path, kind string, s ScorerConfig) {
	if s.Weights != nil {
		v.fail(path+"/scorer/weights", "invalid", "%s scorers take a model, not weights", s.Type)
	}
	var info ScorerInfo
	switch {
	case (s.Model == "") == (s.ModelID == ""):
		v.fail(path+"/scorer", "invalid", "set exactly one of model (a platform model) or model_id (a registered model)")
		return
	case s.Model != "":
		if deps.PlatformScorer == nil {
			return
		}
		got, err := deps.PlatformScorer(ctx, s.Model)
		if err != nil {
			v.fail(path+"/scorer/model", "model_unavailable", "%v", err)
			return
		}
		info = got
	default:
		if !deps.Plan.BYOAllowed {
			v.fail(path+"/scorer/model_id", "plan_forbids_byo", "the %s plan does not allow BYO models", deps.Plan.ID)
		}
		if deps.BYO == nil {
			return
		}
		got, errs := deps.BYO.Scorer(ctx, s.ModelID, kind)
		v.add(prefixed(path+"/scorer", errs)...)
		if len(errs) > 0 {
			return
		}
		info = got
	}
	field := path + "/scorer/model"
	if s.ModelID != "" {
		field = path + "/scorer/model_id"
	}
	if info.Type != s.Type {
		v.fail(field, "wrong_type", "the model is %s, not %s", info.Type, s.Type)
	}
	cat := feature.Catalog()
	for _, f := range info.Features {
		switch src, ok := cat[f]; {
		case !ok:
			v.fail(field, "unknown_feature", "the model's feature %q is not in the feature catalog", f)
		case src == feature.SourceUser:
			v.fail(field, "unavailable", "the model uses %q; user features are only available to the re-ranker", f)
		}
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
