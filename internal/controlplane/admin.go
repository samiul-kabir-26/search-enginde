package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

func itoa(i int) string { return strconv.Itoa(i) }

func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/admin/login", s.handleAdminLogin)
	mux.Handle("GET /v1/admin/settings", s.adminRoute(s.handleGetSettings))
	mux.Handle("PUT /v1/admin/settings", s.adminRoute(s.handlePutSettings))
	mux.Handle("GET /v1/admin/plans", s.adminRoute(s.handleListPlans))
	mux.Handle("PUT /v1/admin/plans/{plan_id}", s.adminRoute(s.handlePutPlan))
	mux.Handle("GET /v1/admin/keys", s.adminRoute(s.handleListKeys))
	mux.Handle("POST /v1/admin/keys/rotate", s.adminRoute(s.handleRotateKey))
	mux.Handle("POST /v1/admin/keys/{kid}/revoke", s.adminRoute(s.handleRevokeKey))
	mux.Handle("GET /v1/admin/tenants", s.adminRoute(s.handleListTenants))
}

func (s *Server) newAdminSession(ctx context.Context, w http.ResponseWriter, adminID, family string) (SessionResponse, error) {
	if _, err := s.Q.GetPlatformAdmin(ctx, adminID); err != nil {
		return SessionResponse{}, err
	}
	ttl := time.Duration(s.Settings.Get().Auth.AccessTokenTTL)
	access, err := s.Keys.IssueClaims(auth.Claims{Scope: auth.ScopePlatformAdmin, RegisteredClaims: jwt.RegisteredClaims{Subject: "admin:" + adminID}}, ttl)
	if err != nil {
		return SessionResponse{}, err
	}
	if err := s.setRefresh(ctx, w, tenantdb.RefreshTokensSubjectTypePlatformAdmin, adminID, "", family); err != nil {
		return SessionResponse{}, err
	}
	return SessionResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(ttl.Seconds())}, nil
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	ctx := r.Context()
	set := s.Settings.Get().Auth
	a, aerr := s.Q.GetPlatformAdminByEmail(ctx, normalizeEmail(req.Email))
	hash := dummyHash
	if aerr == nil {
		hash = a.PasswordHash
	}
	ok, rehash, err := auth.VerifyPassword(req.Password, hash, set.Password)
	if aerr != nil || err != nil || !ok {
		problem(w, http.StatusUnauthorized, "the email or password is wrong")
		return
	}
	if rehash {
		if h, err := auth.HashPassword(req.Password, set.Password); err == nil {
			_ = s.Q.UpdatePlatformAdminPassword(ctx, tenantdb.UpdatePlatformAdminPasswordParams{PasswordHash: h, ID: a.ID})
		}
	}
	resp, err := s.newAdminSession(ctx, w, a.ID, "")
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, resp)
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	platform.WriteJSON(w, http.StatusOK, s.Settings.Get())
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var next PlatformSettings
	if !decodeOrFail(w, r, &next) {
		return
	}
	if errs := s.Settings.Update(r.Context(), next, actor(r)); len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	platform.WriteJSON(w, http.StatusOK, s.Settings.Get())
}

// PlanView is a plan as the admin API shows it.
type PlanView struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	MaxTotalMS      int      `json:"max_total_ms"`
	SearchRPS       int      `json:"search_rps"`
	SearchBurst     int      `json:"search_burst"`
	EventsRPS       int      `json:"events_rps"`
	BYOAllowed      bool     `json:"byo_allowed"`
	EgressAllowlist []string `json:"egress_allowlist"`
	MaxCustomFields int      `json:"max_custom_fields"`
	MaxPageDepth    int      `json:"max_page_depth"`
}

func (p PlanView) validate() []platform.FieldError {
	var errs []platform.FieldError
	check := func(path string, v, lo, hi int) {
		if v < lo || v > hi {
			errs = append(errs, fieldErr(path, "out_of_range", "must be within ["+itoa(lo)+", "+itoa(hi)+"]"))
		}
	}
	if n := len(strings.TrimSpace(p.Name)); n == 0 || n > 64 {
		errs = append(errs, fieldErr("/name", "invalid", "1 to 64 characters"))
	}
	check("/max_total_ms", p.MaxTotalMS, 50, 5000)
	check("/search_rps", p.SearchRPS, 1, 100000)
	check("/search_burst", p.SearchBurst, p.SearchRPS, 1000000)
	check("/events_rps", p.EventsRPS, 1, 1000000)
	check("/max_custom_fields", p.MaxCustomFields, 0, 500)
	check("/max_page_depth", p.MaxPageDepth, 24, 10000)
	for i, h := range p.EgressAllowlist {
		if h == "" || strings.ContainsAny(h, "/: ") {
			errs = append(errs, fieldErr("/egress_allowlist/"+itoa(i), "invalid", "a bare host name"))
		}
	}
	return errs
}

func (s *Server) handleListPlans(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListPlans(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	items := make([]PlanView, len(rows))
	for i, p := range rows {
		items[i] = PlanView{ID: p.ID, Name: p.Name, MaxTotalMS: int(p.MaxTotalMs), SearchRPS: int(p.SearchRps), SearchBurst: int(p.SearchBurst),
			EventsRPS: int(p.EventsRps), BYOAllowed: p.ByoAllowed, MaxCustomFields: int(p.MaxCustomFields), MaxPageDepth: int(p.MaxPageDepth)}
		_ = json.Unmarshal(p.EgressAllowlist, &items[i].EgressAllowlist)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handlePutPlan(w http.ResponseWriter, r *http.Request) {
	var p PlanView
	if !decodeOrFail(w, r, &p) {
		return
	}
	p.ID = r.PathValue("plan_id")
	if p.EgressAllowlist == nil {
		p.EgressAllowlist = []string{}
	}
	errs := p.validate()
	if !slugRe.MatchString(p.ID) && len(p.ID) > 32 {
		errs = append(errs, fieldErr("/id", "invalid", "a short lowercase id"))
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	allow, _ := json.Marshal(p.EgressAllowlist)
	if err := s.Q.UpsertPlan(r.Context(), tenantdb.UpsertPlanParams{
		ID: p.ID, Name: p.Name, MaxTotalMs: int32(p.MaxTotalMS), SearchRps: int32(p.SearchRPS), SearchBurst: int32(p.SearchBurst), //nolint:gosec // bounded by validate
		EventsRps: int32(p.EventsRPS), ByoAllowed: p.BYOAllowed, EgressAllowlist: allow, MaxCustomFields: int32(p.MaxCustomFields), MaxPageDepth: int32(p.MaxPageDepth), //nolint:gosec // bounded by validate
	}); err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, p)
}

func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Keys.Keys(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": keys})
}

func (s *Server) handleRotateKey(w http.ResponseWriter, r *http.Request) {
	kid, err := s.Keys.Rotate(r.Context(), time.Duration(s.Settings.Get().Auth.KeyRetention))
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]string{"active_kid": kid})
}

func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	err := s.Keys.Revoke(r.Context(), r.PathValue("kid"))
	if errors.Is(err, auth.ErrKeyNotFound) {
		problem(w, http.StatusNotFound, "no such key")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListTenants(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Q.ListTenants(r.Context(), tenantdb.ListTenantsParams{Limit: int32(limit), Offset: int32(max(offset, 0))}) //nolint:gosec // bounded above
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": rows})
}
