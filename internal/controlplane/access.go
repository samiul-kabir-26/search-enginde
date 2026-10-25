package controlplane

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

func (s *Server) tenantIdem(h http.HandlerFunc) http.Handler {
	return s.Idem.Wrap(func(r *http.Request) string { return r.PathValue("tid") }, h)
}

func (s *Server) accessRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/tenants/{tid}/users", s.tenantRoute(auth.ScopeTenantRead, s.handleListUsers))
	mux.Handle("POST /v1/tenants/{tid}/users", s.tenantRoute(auth.ScopeTenantAdmin, s.tenantIdem(s.handleCreateUser).ServeHTTP))
	mux.Handle("PATCH /v1/tenants/{tid}/users/{user_id}", s.tenantRoute(auth.ScopeTenantAdmin, s.handleUpdateUser))
	mux.Handle("DELETE /v1/tenants/{tid}/users/{user_id}", s.tenantRoute(auth.ScopeTenantAdmin, s.handleDeleteUser))
	mux.Handle("GET /v1/tenants/{tid}/api-keys", s.tenantRoute(auth.ScopeTenantRead, s.handleListAPIKeys))
	mux.Handle("POST /v1/tenants/{tid}/api-keys", s.tenantRoute(auth.ScopeTenantAdmin, s.tenantIdem(s.handleCreateAPIKey).ServeHTTP))
	mux.Handle("DELETE /v1/tenants/{tid}/api-keys/{key_id}", s.tenantRoute(auth.ScopeTenantAdmin, s.handleRevokeAPIKey))
}

var roles = []string{"owner", "admin", "developer", "viewer"}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListUsers(r.Context(), r.PathValue("tid"))
	if err != nil {
		s.internalError(w, err)
		return
	}
	items := make([]UserView, len(rows))
	for i, u := range rows {
		items[i] = userView(u)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// mayGrant reports whether the caller can assign role: only owners and platform admins can
// create or promote owners.
func mayGrant(p auth.Principal, role string) bool {
	return role != "owner" || p.Role == "owner" || p.Has(auth.ScopePlatformAdmin)
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	req.Email = normalizeEmail(req.Email)
	set := s.Settings.Get().Auth
	var errs []platform.FieldError
	if !emailRe.MatchString(req.Email) || len(req.Email) > 320 {
		errs = append(errs, fieldErr("/email", "invalid", "a valid email address is required"))
	}
	if !slices.Contains(roles, req.Role) {
		errs = append(errs, fieldErr("/role", "invalid", "role must be owner, admin, developer or viewer"))
	} else if !mayGrant(principal(r), req.Role) {
		errs = append(errs, fieldErr("/role", "forbidden", "only owners can add owners"))
	}
	if err := auth.CheckPasswordPolicy(req.Password, set.PasswordMinLength); err != nil {
		errs = append(errs, fieldErr("/password", "weak", err.Error()))
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	hash, err := auth.HashPassword(req.Password, set.Password)
	if err != nil {
		s.internalError(w, err)
		return
	}
	ctx := r.Context()
	id := tenant.NewID().String()
	if err := s.Q.CreateUser(ctx, tenantdb.CreateUserParams{ID: id, TenantID: r.PathValue("tid"), Email: req.Email, PasswordHash: hash, Role: tenantdb.TenantUsersRole(req.Role)}); err != nil {
		problem(w, http.StatusConflict, "a user with this email exists", fieldErr("/email", "taken", "already a member"))
		return
	}
	u, err := s.Q.GetUser(ctx, id)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.sendVerification(ctx, u)
	platform.WriteJSON(w, http.StatusCreated, userView(u))
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role string `json:"role"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	ctx := r.Context()
	tid, uid := r.PathValue("tid"), r.PathValue("user_id")
	if !slices.Contains(roles, req.Role) {
		invalid(w, fieldErr("/role", "invalid", "role must be owner, admin, developer or viewer"))
		return
	}
	u, err := s.Q.GetUser(ctx, uid)
	if err != nil || u.TenantID != tid {
		problem(w, http.StatusNotFound, "no such user")
		return
	}
	if !mayGrant(principal(r), req.Role) || (u.Role == tenantdb.TenantUsersRoleOwner && !mayGrant(principal(r), "owner")) {
		problem(w, http.StatusForbidden, "only owners can change owners")
		return
	}
	if u.Role == tenantdb.TenantUsersRoleOwner && req.Role != "owner" && !s.hasOtherOwner(r, tid) {
		problem(w, http.StatusConflict, "a tenant needs at least one owner")
		return
	}
	if _, err := s.Q.UpdateUserRole(ctx, tenantdb.UpdateUserRoleParams{Role: tenantdb.TenantUsersRole(req.Role), ID: uid, TenantID: tid}); err != nil {
		s.internalError(w, err)
		return
	}
	u.Role = tenantdb.TenantUsersRole(req.Role)
	platform.WriteJSON(w, http.StatusOK, userView(u))
}

func (s *Server) hasOtherOwner(r *http.Request, tid string) bool {
	n, err := s.Q.CountOwners(r.Context(), tid)
	return err == nil && n > 1
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tid, uid := r.PathValue("tid"), r.PathValue("user_id")
	u, err := s.Q.GetUser(ctx, uid)
	if err != nil || u.TenantID != tid {
		problem(w, http.StatusNotFound, "no such user")
		return
	}
	if u.Role == tenantdb.TenantUsersRoleOwner && (!mayGrant(principal(r), "owner") || !s.hasOtherOwner(r, tid)) {
		problem(w, http.StatusConflict, "a tenant needs at least one owner, and only owners can remove owners")
		return
	}
	if _, err := s.Q.DeleteUser(ctx, tenantdb.DeleteUserParams{ID: uid, TenantID: tid}); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// APIKeyView is an API key without its secret.
type APIKeyView struct {
	ID         string     `json:"id"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListAPIKeys(r.Context(), r.PathValue("tid"))
	if err != nil {
		s.internalError(w, err)
		return
	}
	items := make([]APIKeyView, 0, len(rows))
	for _, k := range rows {
		v := APIKeyView{ID: k.ID, Prefix: k.Prefix, CreatedAt: k.CreatedAt}
		_ = json.Unmarshal(k.Scopes, &v.Scopes)
		if k.LastUsedAt.Valid {
			v.LastUsedAt = &k.LastUsedAt.Time
		}
		if k.RevokedAt.Valid {
			v.RevokedAt = &k.RevokedAt.Time
		}
		items = append(items, v)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createAPIKey(r *http.Request, tid tenant.ID, scopes []string) (string, APIKeyView, error) {
	key, prefix, hash, err := auth.NewAPIKey()
	if err != nil {
		return "", APIKeyView{}, err
	}
	id := tenant.NewID().String()
	if err := s.Tenants.CreateAPIKey(r.Context(), tenant.APIKey{ID: id, TenantID: tid, Prefix: prefix, Hash: hash, Scopes: scopes}); err != nil {
		return "", APIKeyView{}, err
	}
	return key, APIKeyView{ID: id, Prefix: prefix, Scopes: scopes, CreatedAt: time.Now().UTC()}, nil
}

func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Scopes []string `json:"scopes"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	if len(req.Scopes) == 0 {
		req.Scopes = []string{auth.ScopeSearchRead, auth.ScopeEventsWrite}
	}
	for i, sc := range req.Scopes {
		if !slices.Contains(auth.DataPlaneScopes(), sc) {
			invalid(w, fieldErr("/scopes/"+itoa(i), "invalid", "API keys can hold search:read, search:debug and events:write"))
			return
		}
	}
	key, view, err := s.createAPIKey(r, pathTenant(r), req.Scopes)
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusCreated, map[string]any{"api_key": key, "key": view})
}

func (s *Server) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	n, err := s.Q.RevokeAPIKey(r.Context(), tenantdb.RevokeAPIKeyParams{ID: r.PathValue("key_id"), TenantID: r.PathValue("tid")})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n == 0 {
		problem(w, http.StatusNotFound, "no such active key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
