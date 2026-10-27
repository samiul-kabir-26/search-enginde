package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// MaxShadowRate caps shadow sampling (docs/API.md: 0–20%).
const MaxShadowRate = 0.2

func (s *Server) configRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/tenants/{tid}/config/draft", s.tenantRoute(auth.ScopeTenantRead, s.handleGetDraft))
	mux.Handle("PUT /v1/tenants/{tid}/config/draft", s.tenantRoute(auth.ScopeTenantWrite, s.handlePutDraft))
	mux.Handle("POST /v1/tenants/{tid}/config/draft/validate", s.tenantRoute(auth.ScopeTenantRead, s.handleValidateDraft))
	mux.Handle("POST /v1/tenants/{tid}/config/versions", s.tenantRoute(auth.ScopeTenantWrite, s.tenantIdem(s.handlePublish).ServeHTTP))
	mux.Handle("GET /v1/tenants/{tid}/config/versions", s.tenantRoute(auth.ScopeTenantRead, s.handleListVersions))
	mux.Handle("GET /v1/tenants/{tid}/config/versions/{v}", s.tenantRoute(auth.ScopeTenantRead, s.handleGetVersion))
	mux.Handle("POST /v1/tenants/{tid}/config/versions/{v}/activate", s.tenantRoute(auth.ScopeTenantAdmin, s.handleActivate))
	mux.Handle("POST /v1/tenants/{tid}/config/versions/{v}/shadow", s.tenantRoute(auth.ScopeTenantAdmin, s.handleShadow))
	mux.Handle("GET /v1/tenants/{tid}/config/diff", s.tenantRoute(auth.ScopeTenantRead, s.handleDiff))
}

// ValidationResult lists every validation error.
type ValidationResult struct {
	Valid  bool                  `json:"valid"`
	Errors []platform.FieldError `json:"errors"`
}

// validateDocument parses a document strictly and runs every rule, probing BYO endpoints.
func (s *Server) validateDocument(ctx context.Context, tid tenant.ID, doc []byte) (*tenant.Config, []platform.FieldError, error) {
	cfg, err := tenant.ParseConfig(doc)
	if err != nil {
		return nil, []platform.FieldError{fieldErr("", "invalid_document", err.Error())}, nil
	}
	t, err := s.Tenants.Tenant(ctx, tid)
	if err != nil {
		return nil, nil, err
	}
	plan, err := s.Tenants.Plan(ctx, t.Plan)
	if err != nil {
		return nil, nil, err
	}
	deps := tenant.ValidationDeps{Plan: plan, PlatformEmbedder: s.PlatformEmbedder, PlatformScorer: s.PlatformScorer}
	if s.BYO != nil {
		deps.BYO = byoChecker{s: s, tid: tid, plan: plan}
	}
	if s.Settings != nil {
		a := s.Settings.Get().Ads
		deps.Ads = tenant.AdsLimits{MaxSlots: a.MaxSlots, MinRelevanceFloor: a.MinRelevanceFloor,
			MinReserveMinor: a.ReserveBounds.MinMinor, MaxReserveMinor: a.ReserveBounds.MaxMinor}
	}
	errs := cfg.Validate(ctx, deps)
	if errs == nil {
		errs = []platform.FieldError{}
	}
	return cfg, errs, nil
}

// DraftView is the draft document with its metadata.
type DraftView struct {
	Document      json.RawMessage   `json:"document"`
	SchemaVersion int               `json:"schema_version"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Validation    *ValidationResult `json:"validation,omitempty"`
}

func (s *Server) handleGetDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.Q.GetConfigDraft(r.Context(), r.PathValue("tid"))
	if err != nil {
		problem(w, http.StatusNotFound, "no draft")
		return
	}
	platform.WriteJSON(w, http.StatusOK, DraftView{Document: d.Document, SchemaVersion: int(d.SchemaVersion), UpdatedAt: d.UpdatedAt})
}

// saveDraft stores a parseable document as the draft. Invalid drafts are allowed (the wizard
// saves partial work); publish enforces validity.
func (s *Server) saveDraft(ctx context.Context, tid tenant.ID, doc []byte, by string) error {
	if err := s.Q.UpsertConfigDraft(ctx, tenantdb.UpsertConfigDraftParams{
		TenantID: string(tid), Document: doc, SchemaVersion: tenant.CurrentSchemaVersion, UpdatedBy: nullString(by),
	}); err != nil {
		return fmt.Errorf("save draft: %w", err)
	}
	return nil
}

func (s *Server) handlePutDraft(w http.ResponseWriter, r *http.Request) {
	doc, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		invalid(w, fieldErr("", "invalid_body", err.Error()))
		return
	}
	ctx := r.Context()
	cfg, errs, err := s.validateDocument(ctx, pathTenant(r), doc)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if cfg == nil {
		invalid(w, errs...)
		return
	}
	canonical, err := json.Marshal(cfg)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.saveDraft(ctx, pathTenant(r), canonical, actor(r)); err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, DraftView{Document: canonical, SchemaVersion: tenant.CurrentSchemaVersion, UpdatedAt: time.Now().UTC(),
		Validation: &ValidationResult{Valid: len(errs) == 0, Errors: errs}})
}

func (s *Server) handleValidateDraft(w http.ResponseWriter, r *http.Request) {
	d, err := s.Q.GetConfigDraft(r.Context(), r.PathValue("tid"))
	if err != nil {
		problem(w, http.StatusNotFound, "no draft")
		return
	}
	_, errs, err := s.validateDocument(r.Context(), pathTenant(r), d.Document)
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, ValidationResult{Valid: len(errs) == 0, Errors: errs})
}

// VersionView summarizes a config version; Document is set for single-version reads.
type VersionView struct {
	Version          int             `json:"version"`
	State            string          `json:"state"`
	ShadowSampleRate *float64        `json:"shadow_sample_rate,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	CreatedBy        string          `json:"created_by,omitempty"`
	Document         json.RawMessage `json:"document,omitempty"`
}

func rate(n sql.NullString) *float64 {
	if !n.Valid {
		return nil
	}
	f, err := strconv.ParseFloat(n.String, 64)
	if err != nil {
		return nil
	}
	return &f
}

// errInvalidConfig carries publish-time validation errors.
type errInvalidConfig struct{ errs []platform.FieldError }

func (e errInvalidConfig) Error() string { return "config is invalid" }

// publishDraft validates the draft and stores it as the next immutable version.
func (s *Server) publishDraft(ctx context.Context, tid tenant.ID, by string) (VersionView, error) {
	d, err := s.Q.GetConfigDraft(ctx, string(tid))
	if err != nil {
		return VersionView{}, fmt.Errorf("draft: %w", err)
	}
	_, errs, err := s.validateDocument(ctx, tid, d.Document)
	if err != nil {
		return VersionView{}, err
	}
	if len(errs) > 0 {
		return VersionView{}, errInvalidConfig{errs}
	}
	var v int
	err = s.inTx(ctx, func(q *tenantdb.Queries) error {
		maxV, err := q.LockMaxConfigVersion(ctx, string(tid))
		if err != nil {
			return fmt.Errorf("lock versions: %w", err)
		}
		v = int(maxV) + 1
		return q.CreateConfigVersion(ctx, tenantdb.CreateConfigVersionParams{
			ID: tenant.NewID().String(), TenantID: string(tid), Version: int32(v), Document: d.Document, //nolint:gosec // versions are small
			SchemaVersion: d.SchemaVersion, State: tenantdb.TenantConfigVersionsStatePublished, CreatedBy: nullString(by),
		})
	})
	if err != nil {
		return VersionView{}, err
	}
	return VersionView{Version: v, State: "published", CreatedAt: time.Now().UTC(), CreatedBy: by}, nil
}

func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	v, err := s.publishDraft(r.Context(), pathTenant(r), actor(r))
	var inv errInvalidConfig
	if errors.As(err, &inv) {
		problem(w, http.StatusUnprocessableEntity, "the draft is invalid; fix every error before publishing", inv.errs...)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, v)
}

func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListConfigVersions(r.Context(), r.PathValue("tid"))
	if err != nil {
		s.internalError(w, err)
		return
	}
	items := make([]VersionView, len(rows))
	for i, v := range rows {
		items[i] = VersionView{Version: int(v.Version), State: string(v.State), ShadowSampleRate: rate(v.ShadowSampleRate), CreatedAt: v.CreatedAt, CreatedBy: v.CreatedBy.String}
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func versionParam(w http.ResponseWriter, r *http.Request) (int32, bool) {
	v, err := strconv.Atoi(r.PathValue("v"))
	if err != nil || v < 1 {
		invalid(w, fieldErr("/v", "invalid", "version must be a positive integer"))
		return 0, false
	}
	return int32(v), true //nolint:gosec // parsed from a path, checked positive
}

func (s *Server) getVersion(r *http.Request, v int32) (tenantdb.TenantConfigVersion, error) {
	row, err := s.Q.GetConfigVersion(r.Context(), tenantdb.GetConfigVersionParams{TenantID: r.PathValue("tid"), Version: v})
	if err != nil {
		return row, fmt.Errorf("version %d: %w", v, err)
	}
	return row, nil
}

func (s *Server) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	v, ok := versionParam(w, r)
	if !ok {
		return
	}
	row, err := s.getVersion(r, v)
	if err != nil {
		problem(w, http.StatusNotFound, "no such version")
		return
	}
	platform.WriteJSON(w, http.StatusOK, VersionView{Version: int(row.Version), State: string(row.State), ShadowSampleRate: rate(row.ShadowSampleRate),
		CreatedAt: row.CreatedAt, CreatedBy: row.CreatedBy.String, Document: row.Document})
}

// activate makes version v active (retiring the previous one) and announces it.
func (s *Server) activate(ctx context.Context, tid tenant.ID, v int32) error {
	err := s.inTx(ctx, func(q *tenantdb.Queries) error {
		if err := q.RetireActiveConfig(ctx, string(tid)); err != nil {
			return fmt.Errorf("retire active: %w", err)
		}
		n, err := q.SetConfigVersionState(ctx, tenantdb.SetConfigVersionStateParams{
			State: tenantdb.TenantConfigVersionsStateActive, TenantID: string(tid), Version: v,
		})
		if err != nil {
			return fmt.Errorf("activate: %w", err)
		}
		if n == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tenant.PublishActivation(ctx, s.Redis, tenant.Activation{TenantID: string(tid), Version: int(v), State: "active"}); err != nil {
		s.Log.Warn("activation not published; search-api reconciles within 30 s", "tenant", tid, "err", err)
	}
	return nil
}

// requireVerified blocks changes to live search behaviour until the user's email is verified.
func requireVerified(w http.ResponseWriter, r *http.Request) bool {
	p := principal(r)
	if p.EmailVerified || p.Has(auth.ScopePlatformAdmin) {
		return true
	}
	problem(w, http.StatusForbidden, "verify your email address before changing live search")
	return false
}

func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	v, ok := versionParam(w, r)
	if !ok || !requireVerified(w, r) {
		return
	}
	row, err := s.getVersion(r, v)
	if err != nil {
		problem(w, http.StatusNotFound, "no such version")
		return
	}
	if row.State == tenantdb.TenantConfigVersionsStateActive {
		platform.WriteJSON(w, http.StatusOK, VersionView{Version: int(v), State: "active", CreatedAt: row.CreatedAt})
		return
	}
	if err := s.activate(r.Context(), pathTenant(r), v); err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, VersionView{Version: int(v), State: "active", CreatedAt: row.CreatedAt})
}

func (s *Server) handleShadow(w http.ResponseWriter, r *http.Request) {
	v, ok := versionParam(w, r)
	if !ok || !requireVerified(w, r) {
		return
	}
	var req struct {
		SampleRate float64 `json:"sample_rate"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	if req.SampleRate < 0 || req.SampleRate > MaxShadowRate {
		invalid(w, fieldErr("/sample_rate", "out_of_range", "sample_rate must be within [0, 0.2]"))
		return
	}
	row, err := s.getVersion(r, v)
	if err != nil {
		problem(w, http.StatusNotFound, "no such version")
		return
	}
	if row.State == tenantdb.TenantConfigVersionsStateActive {
		problem(w, http.StatusConflict, "the active version cannot be its own shadow")
		return
	}
	ctx, tid := r.Context(), pathTenant(r)
	state := "shadow"
	err = s.inTx(ctx, func(q *tenantdb.Queries) error {
		if err := q.ClearShadowConfig(ctx, string(tid)); err != nil {
			return fmt.Errorf("clear shadow: %w", err)
		}
		if req.SampleRate == 0 {
			state = "published"
			return nil
		}
		_, err := q.SetConfigVersionState(ctx, tenantdb.SetConfigVersionStateParams{
			State: tenantdb.TenantConfigVersionsStateShadow, ShadowSampleRate: nullString(strconv.FormatFloat(req.SampleRate, 'f', 3, 64)),
			TenantID: string(tid), Version: v,
		})
		if err != nil {
			return fmt.Errorf("set shadow: %w", err)
		}
		return nil
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	msg := tenant.Activation{TenantID: string(tid), Version: int(v), State: "shadow"}
	if state == "published" {
		msg.State = "shadow_cleared"
	}
	if err := tenant.PublishActivation(ctx, s.Redis, msg); err != nil {
		s.Log.Warn("shadow change not published", "tenant", tid, "err", err)
	}
	view := VersionView{Version: int(v), State: state, CreatedAt: row.CreatedAt}
	if state == "shadow" {
		view.ShadowSampleRate = &req.SampleRate
	}
	platform.WriteJSON(w, http.StatusOK, view)
}

func (s *Server) documentFor(r *http.Request, ref string) (json.RawMessage, error) {
	if ref == "draft" {
		d, err := s.Q.GetConfigDraft(r.Context(), r.PathValue("tid"))
		if err != nil {
			return nil, fmt.Errorf("draft: %w", err)
		}
		return d.Document, nil
	}
	v, err := strconv.Atoi(ref)
	if err != nil || v < 1 {
		return nil, fmt.Errorf("invalid version %q", ref)
	}
	row, err := s.getVersion(r, int32(v)) //nolint:gosec // checked positive
	if err != nil {
		return nil, err
	}
	return row.Document, nil
}

func (s *Server) handleDiff(w http.ResponseWriter, r *http.Request) {
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	a, err := s.documentFor(r, from)
	if err != nil {
		invalid(w, fieldErr("/from", "invalid", "a version number or draft"))
		return
	}
	b, err := s.documentFor(r, to)
	if err != nil {
		invalid(w, fieldErr("/to", "invalid", "a version number or draft"))
		return
	}
	changes, err := tenant.Diff(a, b)
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "changes": changes})
}
