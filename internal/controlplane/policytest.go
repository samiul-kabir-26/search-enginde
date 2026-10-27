package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/policy"
	"github.com/zavisoft/search-engine/internal/stages/filter"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// PolicyTestResult is one rule's outcome in POST /policy/test.
type PolicyTestResult struct {
	ID      string                `json:"id"`
	Kind    string                `json:"kind"`
	Errors  []platform.FieldError `json:"errors,omitempty"`
	Results []PolicyTestItem      `json:"results"`
}

// PolicyTestItem is a rule evaluated on one sample.
type PolicyTestItem struct {
	ProductID    string   `json:"product_id"`
	Keep         *bool    `json:"keep,omitempty"`
	Boost        *float64 `json:"boost,omitempty"`
	Failed       bool     `json:"failed,omitempty"`
	MappingError bool     `json:"mapping_error,omitempty"`
}

// handlePolicyTest evaluates each CEL rule on its own against the samples mapped with the
// draft's catalog mapping, the way S3 runs it (onboarding step 4, "Test against sample
// products"). Vendor fields are the declared defaults; nothing is saved.
func (s *Server) handlePolicyTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Rules   []policy.Rule     `json:"rules"`
		Samples []json.RawMessage `json:"samples"`
		Query   string            `json:"query"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	var errs []platform.FieldError
	if len(req.Rules) == 0 || len(req.Rules) > policy.MaxRules {
		errs = append(errs, fieldErr("/rules", "invalid", fmt.Sprintf("1 to %d rules", policy.MaxRules)))
	}
	if len(req.Samples) == 0 || len(req.Samples) > 20 {
		errs = append(errs, fieldErr("/samples", "invalid", "1 to 20 samples"))
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	d, err := s.Q.GetConfigDraft(r.Context(), r.PathValue("tid"))
	if err != nil {
		s.internalError(w, err)
		return
	}
	cfg, err := tenant.ParseConfig(d.Document)
	if err != nil {
		s.internalError(w, err)
		return
	}
	mapper, err := catalog.NewMapper(cfg.Catalog.FieldMapping, cfg.Catalog.CustomFields)
	if err != nil {
		invalid(w, fieldErr("/samples", "mapping_invalid", "the draft's catalog mapping is invalid; complete the catalog step first"))
		return
	}
	previews := mapper.Preview(r.PathValue("tid"), req.Samples)
	query := filter.QueryVars(&pipeline.Query{Text: req.Query, Type: pipeline.QueryText})
	custom := cfg.Catalog.CustomFieldNames()

	out := make([]PolicyTestResult, len(req.Rules))
	for i, rule := range req.Rules {
		res := PolicyTestResult{ID: rule.ID, Kind: rule.Kind, Results: []PolicyTestItem{}}
		set, cerrs := policy.Compile([]policy.Rule{rule}, custom)
		for _, e := range cerrs {
			res.Errors = append(res.Errors, fieldErr(fmt.Sprintf("/rules/%d/%s", i, e.Field), e.Code, e.Message))
		}
		if len(cerrs) == 0 {
			for _, p := range previews {
				item := PolicyTestItem{ProductID: p.Product.ProductID}
				if len(p.Errors) > 0 {
					item.MappingError = true
					res.Results = append(res.Results, item)
					continue
				}
				keep, boost, n := set.Eval(filter.ItemVars(docOf(p.Product), p.Product.ProductID, nil), query)
				item.Failed = n > 0
				if rule.Kind == policy.KindFilter {
					item.Keep = &keep
				} else {
					item.Boost = &boost
				}
				res.Results = append(res.Results, item)
			}
		}
		out[i] = res
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"rules": out})
}

// docOf is the ranking doc a retriever would return for a mapped product.
func docOf(p catalog.Product) *pipeline.Doc {
	d := &pipeline.Doc{
		Title: p.Title, Brand: p.Brand, VendorID: p.VendorID, PriceMinor: p.PriceMinor, Currency: p.Currency,
		CategoryIDs: p.CategoryIDs, Rating: p.Rating, RatingCount: int64(p.RatingCount),
		ListingCompleteness: p.ListingCompleteness, ImageCount: len(p.ImageURLs), CreatedAt: p.CreatedAt,
		Status: p.Status, ShipRegions: p.ShipRegions, Custom: p.Custom,
	}
	if len(p.ImageURLs) > 0 {
		d.ImageURL = p.ImageURLs[0]
	}
	if p.Stock != nil {
		stock := int64(*p.Stock)
		d.Stock = &stock
	}
	return d
}
