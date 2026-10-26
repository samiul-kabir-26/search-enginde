package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// Onboarding steps (docs/ONBOARDING.md §4), in order.
const (
	StepAccount         = "account"
	StepEngineTour      = "engine_tour"
	StepCatalog         = "catalog"
	StepRetrieval       = "retrieval"
	StepPolicies        = "policies"
	StepRanking         = "ranking"
	StepPersonalization = "personalization"
	StepIndexBuild      = "index_build"
	StepPlayground      = "playground"
	StepGoLive          = "go_live"
	StepDone            = "done"
)

// Steps is the wizard order.
var Steps = []string{StepAccount, StepEngineTour, StepCatalog, StepRetrieval, StepPolicies, StepRanking,
	StepPersonalization, StepIndexBuild, StepPlayground, StepGoLive}

// stepSections are the config paths each step owns; their errors block the step.
var stepSections = map[string][]string{
	StepCatalog:         {"/catalog"},
	StepRetrieval:       {"/embedders", "/stores", "/retrieval"},
	StepPolicies:        {"/policy"},
	StepRanking:         {"/prerank", "/rank"},
	StepPersonalization: {"/rerank", "/ads"},
}

func (s *Server) onboardingRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/tenants/{tid}/onboarding", s.tenantRoute(auth.ScopeTenantRead, s.handleGetOnboarding))
	mux.Handle("PUT /v1/tenants/{tid}/onboarding/steps/{step}", s.tenantRoute(auth.ScopeTenantWrite, s.handlePutStep))
	mux.HandleFunc("GET /v1/onboarding/engine-explainer", s.handleExplainer)
	mux.Handle("GET /v1/tenants/{tid}/engine-explainer", s.tenantRoute(auth.ScopeTenantRead, s.handleTenantExplainer))
}

// OnboardingView is the wizard state.
type OnboardingView struct {
	CurrentStep    string   `json:"current_step"`
	CompletedSteps []string `json:"completed_steps"`
	Steps          []string `json:"steps"`
}

func (s *Server) onboardingState(ctx context.Context, tid string) (OnboardingView, error) {
	row, err := s.Q.GetOnboardingState(ctx, tid)
	if err != nil {
		return OnboardingView{}, fmt.Errorf("onboarding state: %w", err)
	}
	v := OnboardingView{CurrentStep: row.CurrentStep, Steps: Steps}
	_ = json.Unmarshal(row.CompletedSteps, &v.CompletedSteps)
	if v.CompletedSteps == nil {
		v.CompletedSteps = []string{}
	}
	return v, nil
}

func (s *Server) handleGetOnboarding(w http.ResponseWriter, r *http.Request) {
	v, err := s.onboardingState(r.Context(), r.PathValue("tid"))
	if err != nil {
		problem(w, http.StatusNotFound, "no onboarding state")
		return
	}
	platform.WriteJSON(w, http.StatusOK, v)
}

// StepResult reports a saved step.
type StepResult struct {
	Step     string                `json:"step"`
	NextStep string                `json:"next_step"`
	Warnings []platform.FieldError `json:"warnings"`
	Data     any                   `json:"data,omitempty"`
	State    OnboardingView        `json:"state"`
}

// stepError is a blocking validation failure.
type stepError struct{ errs []platform.FieldError }

func (e stepError) Error() string { return "step is invalid" }

func block(errs ...platform.FieldError) error { return stepError{errs} }

func (s *Server) handlePutStep(w http.ResponseWriter, r *http.Request) {
	ctx, tid, step := r.Context(), pathTenant(r), r.PathValue("step")
	idx := slices.Index(Steps, step)
	if idx <= 0 {
		problem(w, http.StatusNotFound, "no such step")
		return
	}
	state, err := s.onboardingState(ctx, string(tid))
	if err != nil {
		problem(w, http.StatusNotFound, "no onboarding state")
		return
	}
	cur := slices.Index(Steps, state.CurrentStep)
	if state.CurrentStep == StepDone {
		cur = len(Steps)
	}
	if idx > cur && !slices.Contains(state.CompletedSteps, step) {
		problem(w, http.StatusConflict, "complete "+state.CurrentStep+" first")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		invalid(w, fieldErr("", "invalid_body", err.Error()))
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	useDefaults := r.URL.Query().Get("use_defaults") == "true"
	if useDefaults && (idx < slices.Index(Steps, StepRetrieval) || idx > slices.Index(Steps, StepPersonalization)) {
		invalid(w, fieldErr("/use_defaults", "invalid", "use_defaults applies to the steps between retrieval and personalization"))
		return
	}

	res := StepResult{Step: step, Warnings: []platform.FieldError{}}
	if useDefaults {
		res.Data = map[string]string{"applied": "platform defaults for the remaining configuration steps"}
	} else if res.Warnings, res.Data, err = s.runStep(r, tid, step, body); err != nil {
		var se stepError
		if errors.As(err, &se) {
			invalid(w, se.errs...)
			return
		}
		s.internalError(w, err)
		return
	}
	completed := state.CompletedSteps
	mark := []string{step}
	if useDefaults {
		mark = Steps[idx : slices.Index(Steps, StepPersonalization)+1]
	}
	for _, m := range mark {
		if !slices.Contains(completed, m) {
			completed = append(completed, m)
		}
	}
	next := StepDone
	for _, st := range Steps {
		if !slices.Contains(completed, st) {
			next = st
			break
		}
	}
	cb, _ := json.Marshal(completed)
	if err := s.Q.UpdateOnboardingState(ctx, tenantdb.UpdateOnboardingStateParams{CurrentStep: next, CompletedSteps: cb, TenantID: string(tid)}); err != nil {
		s.internalError(w, err)
		return
	}
	res.NextStep = next
	res.State = OnboardingView{CurrentStep: next, CompletedSteps: completed, Steps: Steps}
	platform.WriteJSON(w, http.StatusOK, res)
}

func strictJSON(body []byte, dst any) error {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return block(fieldErr("", "invalid_json", err.Error()))
	}
	return nil
}

// editDraft applies fn to the parsed draft, saves it, and returns the validation errors in the
// step's sections (blocking) and elsewhere (warnings).
func (s *Server) editDraft(r *http.Request, tid tenant.ID, step string, fn func(*tenant.Config) error) ([]platform.FieldError, error) {
	ctx := r.Context()
	d, err := s.Q.GetConfigDraft(ctx, string(tid))
	if err != nil {
		return nil, fmt.Errorf("draft: %w", err)
	}
	cfg, err := tenant.ParseConfig(d.Document)
	if err != nil {
		return nil, block(fieldErr("", "invalid_draft", err.Error()))
	}
	if err := fn(cfg); err != nil {
		return nil, err
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode draft: %w", err)
	}
	_, errs, err := s.validateDocument(ctx, tid, doc)
	if err != nil {
		return nil, err
	}
	var blocking, warnings []platform.FieldError
	for _, e := range errs {
		mine := false
		for _, p := range stepSections[step] {
			mine = mine || strings.HasPrefix(e.Path, p)
		}
		if mine {
			blocking = append(blocking, e)
		} else {
			warnings = append(warnings, e)
		}
	}
	if len(blocking) > 0 {
		return nil, block(blocking...)
	}
	if err := s.saveDraft(ctx, tid, doc, actor(r)); err != nil {
		return nil, err
	}
	if warnings == nil {
		warnings = []platform.FieldError{}
	}
	return warnings, nil
}

func (s *Server) runStep(r *http.Request, tid tenant.ID, step string, body []byte) ([]platform.FieldError, any, error) {
	switch step {
	case StepEngineTour, StepPlayground:
		return []platform.FieldError{}, nil, nil
	case StepCatalog:
		return s.stepCatalog(r, tid, body)
	case StepRetrieval:
		return s.stepRetrieval(r, tid, body)
	case StepPolicies:
		return s.stepPolicies(r, tid, body)
	case StepRanking:
		return s.stepRanking(r, tid, body)
	case StepPersonalization:
		return s.stepPersonalization(r, tid, body)
	case StepIndexBuild:
		return s.stepIndexBuild(r, tid, body)
	case StepGoLive:
		return s.stepGoLive(r, tid, body)
	}
	return nil, nil, block(fieldErr("/step", "unknown", "no such step"))
}

// onboardingRequired are the fields the wizard needs mapped beyond the catalog's mandatory set.
var onboardingRequired = []string{"vendor_id", "image_urls"}

func (s *Server) stepCatalog(r *http.Request, tid tenant.ID, body []byte) ([]platform.FieldError, any, error) {
	var req struct {
		Ingestion    string                `json:"ingestion"`
		FieldMapping catalog.FieldMapping  `json:"field_mapping"`
		CustomFields []catalog.CustomField `json:"custom_fields"`
		// VendorMapping optionally reads vendor features from product payloads (PLAN.md R9).
		VendorMapping catalog.VendorMapping `json:"vendor_mapping"`
		Samples       []json.RawMessage     `json:"samples"`
	}
	if err := strictJSON(body, &req); err != nil {
		return nil, nil, err
	}
	errs := catalog.ValidateVendorMapping(req.VendorMapping)
	for i := range errs {
		errs[i].Path = strings.TrimPrefix(errs[i].Path, "/catalog")
	}
	if req.Ingestion != "events" && req.Ingestion != "upload" {
		errs = append(errs, fieldErr("/ingestion", "invalid", "ingestion must be events or upload"))
	}
	for _, f := range onboardingRequired {
		if _, ok := req.FieldMapping[f]; !ok {
			errs = append(errs, fieldErr("/field_mapping/"+f, "required", f+" must be mapped"))
		}
	}
	if len(req.Samples) == 0 || len(req.Samples) > 20 {
		errs = append(errs, fieldErr("/samples", "invalid", "paste 1 to 20 sample products"))
	}
	if len(errs) > 0 {
		return nil, nil, block(errs...)
	}
	mapper, err := catalog.NewMapper(req.FieldMapping, req.CustomFields)
	if err != nil {
		msgs := append(catalog.ValidateMapping(req.FieldMapping), catalog.ValidateCustomFields(req.CustomFields, 0)...)
		return nil, nil, block(msgs...)
	}
	previews := mapper.Preview(string(tid), req.Samples)
	for i, p := range previews {
		for _, e := range p.Errors {
			errs = append(errs, fieldErr(fmt.Sprintf("/samples/%d%s", i, e.Path), e.Code, e.Message))
		}
		if p.Product.VendorID == string(tid) || p.Product.VendorID == "" {
			errs = append(errs, fieldErr(fmt.Sprintf("/samples/%d/vendor_id", i), "missing", "the mapping selected no vendor_id"))
		}
		if len(p.Product.ImageURLs) == 0 {
			errs = append(errs, fieldErr(fmt.Sprintf("/samples/%d/image_urls", i), "missing", "at least one image URL is required"))
		}
	}
	if len(errs) > 0 {
		return nil, previews, block(errs...)
	}
	warnings, err := s.editDraft(r, tid, StepCatalog, func(c *tenant.Config) error {
		c.Catalog = tenant.CatalogConfig{Ingestion: req.Ingestion, FieldMapping: req.FieldMapping,
			CustomFields: req.CustomFields, VendorMapping: req.VendorMapping}
		return nil
	})
	return warnings, map[string]any{"previews": previews}, err
}

// RetrievalPresets are the retriever mixes offered in the wizard.
func RetrievalPresets() map[string][]tenant.RetrieverConfig {
	mix := func(lex, vt, t2i float64) []tenant.RetrieverConfig {
		return []tenant.RetrieverConfig{
			{Name: tenant.RetrieverLexicalBM25, K: 500, Weight: lex},
			{Name: tenant.RetrieverVectorText, K: 500, Weight: vt},
			{Name: tenant.RetrieverVectorTextToImage, K: 200, Weight: t2i},
		}
	}
	return map[string][]tenant.RetrieverConfig{
		"balanced": mix(1.0, 1.0, 0.5),
		"keyword":  mix(1.5, 0.5, 0.25),
		"semantic": mix(0.5, 1.5, 0.75),
		"visual":   mix(0.75, 0.75, 1.5),
	}
}

func (s *Server) stepRetrieval(r *http.Request, tid tenant.ID, body []byte) ([]platform.FieldError, any, error) {
	var req struct {
		Embedders *tenant.EmbeddersConfig `json:"embedders"`
		Stores    *tenant.StoresConfig    `json:"stores"`
		Preset    string                  `json:"preset"`
	}
	if err := strictJSON(body, &req); err != nil {
		return nil, nil, err
	}
	if req.Preset == "" {
		req.Preset = "balanced"
	}
	mix, ok := RetrievalPresets()[req.Preset]
	if !ok {
		return nil, nil, block(fieldErr("/preset", "invalid", "preset must be balanced, keyword, semantic or visual"))
	}
	warnings, err := s.editDraft(r, tid, StepRetrieval, func(c *tenant.Config) error {
		if req.Embedders != nil {
			c.Embedders = *req.Embedders
		}
		if req.Stores != nil {
			c.Stores = *req.Stores
		}
		c.Retrieval.Text.Retrievers = mix
		return nil
	})
	return warnings, map[string]any{"retrievers": mix}, err
}

func cond(field string, op filter.Op, v any) filter.Expr {
	return filter.Expr{Cond: &filter.Condition{Field: field, Op: op, Value: v}}
}

func (s *Server) stepPolicies(r *http.Request, tid tenant.ID, body []byte) ([]platform.FieldError, any, error) {
	var req struct {
		InStock         bool                `json:"in_stock"`
		ShipToRegion    string              `json:"ship_to_region"`
		VerifiedVendors bool                `json:"verified_vendors"`
		Rules           []tenant.PolicyRule `json:"rules"`
	}
	if err := strictJSON(body, &req); err != nil {
		return nil, nil, err
	}
	always := []filter.Expr{cond("status", filter.Eq, "active")}
	if req.InStock {
		always = append(always, cond("stock", filter.Gt, json.Number("0")))
	}
	if req.ShipToRegion != "" {
		always = append(always, cond("ship_regions", filter.Contains, req.ShipToRegion))
	}
	rules := req.Rules
	if req.VerifiedVendors {
		rules = append([]tenant.PolicyRule{{ID: "template-verified-vendors", Kind: "filter", Expr: "item.vendor.verified"}}, rules...)
	}
	if rules == nil {
		rules = []tenant.PolicyRule{}
	}
	warnings, err := s.editDraft(r, tid, StepPolicies, func(c *tenant.Config) error {
		c.Policy = tenant.PolicyConfig{AlwaysFilters: always, Rules: rules}
		return nil
	})
	if err == nil && len(rules) > 0 {
		warnings = append(warnings, fieldErr("/policy/rules", "deferred", "CEL rules are compiled and enforced from M7; they are saved now"))
	}
	return warnings, nil, err
}

func (s *Server) stepRanking(r *http.Request, tid tenant.ID, body []byte) ([]platform.FieldError, any, error) {
	var req struct {
		Mode    string             `json:"mode"`
		Weights map[string]float64 `json:"weights"`
	}
	if err := strictJSON(body, &req); err != nil {
		return nil, nil, err
	}
	switch req.Mode {
	case "", "default":
	case "learned":
		return nil, nil, block(fieldErr("/mode", "not_available", "the platform learned model needs interaction events and arrives in M7"))
	case "byo":
		return nil, nil, block(fieldErr("/mode", "not_available", "BYO ranking models arrive in M7"))
	default:
		return nil, nil, block(fieldErr("/mode", "invalid", "mode must be default, learned or byo"))
	}
	warnings, err := s.editDraft(r, tid, StepRanking, func(c *tenant.Config) error {
		c.Rank.Scorer.Type = tenant.ScorerDefaultLinear
		if len(req.Weights) > 0 {
			c.Rank.Scorer.Weights = req.Weights
		}
		return nil
	})
	return warnings, nil, err
}

func (s *Server) stepPersonalization(r *http.Request, tid tenant.ID, body []byte) ([]platform.FieldError, any, error) {
	var req struct {
		Personalization *tenant.PersonalizationConfig `json:"personalization"`
		Diversity       *tenant.DiversityConfig       `json:"diversity"`
		Ads             *tenant.AdsConfig             `json:"ads"`
	}
	if err := strictJSON(body, &req); err != nil {
		return nil, nil, err
	}
	var errs []platform.FieldError
	if p := req.Personalization; p != nil && (p.Alpha < 0 || p.Alpha > 1 || p.Beta < 0 || p.Beta > 1) {
		errs = append(errs, fieldErr("/personalization", "out_of_range", "alpha and beta must be within [0, 1]"))
	}
	if d := req.Diversity; d != nil && (d.MaxPerVendor < 1 || d.Window < d.MaxPerVendor) {
		errs = append(errs, fieldErr("/diversity", "out_of_range", "max_per_vendor must be >= 1 and window >= max_per_vendor"))
	}
	if len(errs) > 0 {
		return nil, nil, block(errs...)
	}
	warnings, err := s.editDraft(r, tid, StepPersonalization, func(c *tenant.Config) error {
		if req.Personalization != nil {
			c.Rerank.Personalization = req.Personalization
		}
		if req.Diversity != nil {
			c.Rerank.Diversity = req.Diversity
		}
		if req.Ads != nil {
			c.Ads = *req.Ads
		}
		return nil
	})
	return warnings, nil, err
}

func (s *Server) stepIndexBuild(r *http.Request, tid tenant.ID, body []byte) ([]platform.FieldError, any, error) {
	var req struct {
		UploadID string `json:"upload_id"`
	}
	if err := strictJSON(body, &req); err != nil {
		return nil, nil, err
	}
	v, err := s.publishDraft(r.Context(), tid, actor(r))
	var inv errInvalidConfig
	if errors.As(err, &inv) {
		return nil, nil, block(inv.errs...)
	}
	if err != nil {
		return nil, nil, err
	}
	job, err := s.createIndexJob(r.Context(), tid, v.Version, req.UploadID)
	if err != nil {
		return nil, nil, err
	}
	return []platform.FieldError{}, map[string]any{"version": v, "job": job}, nil
}

func (s *Server) stepGoLive(r *http.Request, tid tenant.ID, body []byte) ([]platform.FieldError, any, error) {
	var req struct {
		Mode       string  `json:"mode"`
		Version    int     `json:"version"`
		SampleRate float64 `json:"sample_rate"`
	}
	if err := strictJSON(body, &req); err != nil {
		return nil, nil, err
	}
	p := principal(r)
	if !p.EmailVerified && !p.Has(auth.ScopePlatformAdmin) {
		return nil, nil, block(fieldErr("", "email_unverified", "verify your email address before going live"))
	}
	ctx := r.Context()
	if req.Version == 0 {
		rows, err := s.Q.ListConfigVersions(ctx, string(tid))
		if err != nil || len(rows) == 0 {
			return nil, nil, block(fieldErr("/version", "required", "publish and build a version first"))
		}
		req.Version = int(rows[0].Version)
	}
	v := int32(req.Version) //nolint:gosec // versions are small
	if _, err := s.Q.GetConfigVersion(ctx, tenantdb.GetConfigVersionParams{TenantID: string(tid), Version: v}); err != nil {
		return nil, nil, block(fieldErr("/version", "unknown", "no such version"))
	}
	switch req.Mode {
	case "", "activate":
		if err := s.activate(ctx, tid, v); err != nil {
			return nil, nil, err
		}
	case "shadow":
		return nil, nil, block(fieldErr("/mode", "invalid", "start shadow mode from the config API (POST .../shadow), then activate here"))
	default:
		return nil, nil, block(fieldErr("/mode", "invalid", "mode must be activate"))
	}
	data := map[string]any{"version": req.Version, "state": "active"}
	n, err := s.Tenants.CountActiveAPIKeys(ctx, tid)
	if err != nil {
		return nil, nil, err
	}
	if n == 0 {
		key, view, err := s.createAPIKey(r, tid, []string{auth.ScopeSearchRead, auth.ScopeEventsWrite})
		if err != nil {
			return nil, nil, err
		}
		data["api_key"], data["key"] = key, view
	}
	return []platform.FieldError{}, data, nil
}
