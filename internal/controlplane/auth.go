package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// KeyStore looks up API keys and their tenants.
type KeyStore interface {
	APIKeyByPrefix(ctx context.Context, prefix string) (tenant.APIKey, error)
	TouchAPIKey(ctx context.Context, id string) error
	Tenant(ctx context.Context, id tenant.ID) (tenant.Tenant, error)
}

// Auth serves the token endpoints: the JWKS used by search-api and the API key exchange
// (`POST /v1/auth/token`). Sign-up, login and key rotation follow in M4 (PLAN.md R7).
type Auth struct {
	Keys     KeyStore
	Signer   *auth.Signer
	TokenTTL time.Duration
	Log      *slog.Logger
}

// Routes registers the auth routes.
func (a *Auth) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/jwks.json", a.handleJWKS)
	mux.HandleFunc("POST /v1/auth/token", a.handleToken)
}

func (a *Auth) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	platform.WriteJSON(w, http.StatusOK, a.Signer.JWKS())
}

type tokenRequest struct {
	APIKey string `json:"api_key"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

func (a *Auth) handleToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.APIKey == "" {
		platform.WriteProblem(w, platform.Problem{Status: http.StatusBadRequest, Detail: "api_key is required",
			Errors: []platform.FieldError{{Path: "/api_key", Code: "required", Message: "api_key is required"}}})
		return
	}
	unauthorized := func() {
		platform.WriteProblem(w, platform.Problem{Status: http.StatusUnauthorized, Detail: "the API key is invalid or revoked"})
	}
	prefix, secret, err := auth.ParseAPIKey(req.APIKey)
	if err != nil {
		unauthorized()
		return
	}
	ctx := r.Context()
	key, err := a.Keys.APIKeyByPrefix(ctx, prefix)
	if errors.Is(err, tenant.ErrNotFound) || (err == nil && !auth.CheckAPIKeySecret(secret, key.Hash)) {
		unauthorized()
		return
	}
	if err != nil {
		a.Log.Error("api key lookup", "err", err)
		platform.WriteProblem(w, platform.Problem{Status: http.StatusServiceUnavailable})
		return
	}
	t, err := a.Keys.Tenant(ctx, key.TenantID)
	if err != nil || t.Status != "active" {
		platform.WriteProblem(w, platform.Problem{Status: http.StatusForbidden, Detail: "the tenant is unknown or inactive"})
		return
	}
	token, err := a.Signer.Issue(key.TenantID, "apikey:"+key.ID, key.Scopes, a.TokenTTL)
	if err != nil {
		a.Log.Error("issue token", "err", err)
		platform.WriteProblem(w, platform.Problem{Status: http.StatusInternalServerError})
		return
	}
	if err := a.Keys.TouchAPIKey(ctx, key.ID); err != nil {
		a.Log.Warn("touch api key", "err", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, tokenResponse{AccessToken: token, TokenType: "Bearer", ExpiresIn: int(a.TokenTTL.Seconds())})
}
