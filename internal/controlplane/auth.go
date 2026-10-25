package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// RefreshCookie holds the rotating refresh token; it is scoped to the auth endpoints.
const RefreshCookie = "se_refresh"

func fieldErr(path, code, msg string) platform.FieldError {
	return platform.FieldError{Path: path, Code: code, Message: msg}
}

func (s *Server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
	mux.Handle("POST /v1/signup", s.Idem.Wrap(func(*http.Request) string { return "signup" }, http.HandlerFunc(s.handleSignup)))
	mux.HandleFunc("POST /v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /v1/auth/refresh", s.handleRefresh)
	mux.HandleFunc("POST /v1/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /v1/auth/verify-email", s.handleVerifyEmail)
	mux.Handle("POST /v1/auth/resend-verification", http.HandlerFunc(s.handleResendVerification))
	mux.HandleFunc("POST /v1/auth/token", s.handleToken)
}

func (s *Server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=60")
	platform.WriteJSON(w, http.StatusOK, s.Keys.JWKS())
}

// UserView is a tenant user as returned by the API.
type UserView struct {
	ID            string    `json:"id"`
	Email         string    `json:"email"`
	Role          string    `json:"role"`
	EmailVerified bool      `json:"email_verified"`
	CreatedAt     time.Time `json:"created_at"`
}

func userView(u tenantdb.TenantUser) UserView {
	return UserView{ID: u.ID, Email: u.Email, Role: string(u.Role), EmailVerified: u.EmailVerifiedAt.Valid, CreatedAt: u.CreatedAt}
}

// SessionResponse carries a console access token; the refresh token is an httpOnly cookie.
type SessionResponse struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresIn   int       `json:"expires_in"`
	TenantID    string    `json:"tenant_id,omitempty"`
	User        *UserView `json:"user,omitempty"`
}

type signupRequest struct {
	CompanyName string `json:"company_name"`
	Slug        string `json:"slug"`
	Region      string `json:"region"`
	Plan        string `json:"plan"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

var (
	slugRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	var req signupRequest
	if !decodeOrFail(w, r, &req) {
		return
	}
	ctx := r.Context()
	req.Email = normalizeEmail(req.Email)
	set := s.Settings.Get().Auth
	var errs []platform.FieldError
	if n := len(strings.TrimSpace(req.CompanyName)); n == 0 || n > 255 {
		errs = append(errs, fieldErr("/company_name", "invalid", "1 to 255 characters"))
	}
	if !slugRe.MatchString(req.Slug) {
		errs = append(errs, fieldErr("/slug", "invalid", "3 to 63 lowercase letters, digits or hyphens, not starting or ending with a hyphen"))
	}
	if req.Region == "" || len(req.Region) > 32 {
		errs = append(errs, fieldErr("/region", "invalid", "1 to 32 characters"))
	}
	if !emailRe.MatchString(req.Email) || len(req.Email) > 320 {
		errs = append(errs, fieldErr("/email", "invalid", "a valid email address is required"))
	}
	if err := auth.CheckPasswordPolicy(req.Password, set.PasswordMinLength); err != nil {
		errs = append(errs, fieldErr("/password", "weak", err.Error()))
	}
	if req.Plan == "" {
		req.Plan = "starter"
	}
	if _, err := s.Tenants.Plan(ctx, req.Plan); err != nil {
		errs = append(errs, fieldErr("/plan", "unknown", "no such plan"))
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	if _, err := s.Tenants.BySlug(ctx, req.Slug); err == nil {
		problem(w, http.StatusConflict, "the slug is taken", fieldErr("/slug", "taken", "choose another slug"))
		return
	}
	hash, err := auth.HashPassword(req.Password, set.Password)
	if err != nil {
		s.internalError(w, err)
		return
	}
	tid, userID := tenant.NewID(), tenant.NewID().String()
	err = s.inTx(ctx, func(q *tenantdb.Queries) error {
		steps := []func() error{
			func() error {
				return q.CreateTenant(ctx, tenantdb.CreateTenantParams{ID: string(tid), Slug: req.Slug, Name: strings.TrimSpace(req.CompanyName), Plan: req.Plan, Region: req.Region, Status: "active"})
			},
			func() error {
				return q.CreateUser(ctx, tenantdb.CreateUserParams{ID: userID, TenantID: string(tid), Email: req.Email, PasswordHash: hash, Role: tenantdb.TenantUsersRoleOwner})
			},
			func() error {
				return q.UpsertConfigDraft(ctx, tenantdb.UpsertConfigDraftParams{TenantID: string(tid), Document: tenant.DefaultDocument(), SchemaVersion: tenant.CurrentSchemaVersion, UpdatedBy: nullString(userID)})
			},
			func() error {
				completed, _ := json.Marshal([]string{StepAccount})
				return q.CreateOnboardingState(ctx, tenantdb.CreateOnboardingStateParams{TenantID: string(tid), CurrentStep: StepEngineTour, CompletedSteps: completed})
			},
		}
		for _, f := range steps {
			if err := f(); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "Duplicate entry") {
			problem(w, http.StatusConflict, "the slug is taken", fieldErr("/slug", "taken", "choose another slug"))
			return
		}
		s.internalError(w, err)
		return
	}
	user, err := s.Q.GetUser(ctx, userID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.sendVerification(context.WithoutCancel(ctx), user)
	resp, err := s.newUserSession(ctx, w, user, "")
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, resp)
}

func (s *Server) inTx(ctx context.Context, fn func(q *tenantdb.Queries) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(s.Q.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func randomToken() (string, []byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("token: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(tok))
	return tok, sum[:], nil
}

func hashToken(tok string) []byte {
	sum := sha256.Sum256([]byte(tok))
	return sum[:]
}

// newUserSession issues an access token and a refresh token in family (a new family when empty).
func (s *Server) newUserSession(ctx context.Context, w http.ResponseWriter, u tenantdb.TenantUser, family string) (SessionResponse, error) {
	set := s.Settings.Get().Auth
	access, err := s.Keys.IssueClaims(auth.Claims{
		TenantID: u.TenantID, Scope: strings.Join(auth.RoleScopes(string(u.Role)), " "), Role: string(u.Role),
		EmailVerified: u.EmailVerifiedAt.Valid, RegisteredClaims: jwt.RegisteredClaims{Subject: "user:" + u.ID},
	}, time.Duration(set.AccessTokenTTL))
	if err != nil {
		return SessionResponse{}, err
	}
	if err := s.setRefresh(ctx, w, tenantdb.RefreshTokensSubjectTypeTenantUser, u.ID, u.TenantID, family); err != nil {
		return SessionResponse{}, err
	}
	v := userView(u)
	return SessionResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(time.Duration(set.AccessTokenTTL).Seconds()), TenantID: u.TenantID, User: &v}, nil
}

func (s *Server) setRefresh(ctx context.Context, w http.ResponseWriter, kind tenantdb.RefreshTokensSubjectType, subject, tid, family string) error {
	tok, hash, err := randomToken()
	if err != nil {
		return err
	}
	if family == "" {
		family = tenant.NewID().String()
	}
	ttl := time.Duration(s.Settings.Get().Auth.RefreshTokenTTL)
	if err := s.Q.CreateRefreshToken(ctx, tenantdb.CreateRefreshTokenParams{
		ID: tenant.NewID().String(), FamilyID: family, SubjectType: kind, SubjectID: subject,
		TenantID: nullString(tid), TokenHash: hash, ExpiresAt: time.Now().Add(ttl),
	}); err != nil {
		return fmt.Errorf("store refresh token: %w", err)
	}
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is configuration: off only for plain-HTTP local dev
		Name: RefreshCookie, Value: tok, Path: "/v1/", MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: s.SecureCookies, SameSite: http.SameSiteStrictMode,
	})
	return nil
}

type loginRequest struct {
	Tenant   string `json:"tenant"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

// dummyHash keeps login timing the same whether or not the account exists.
var dummyHash, _ = auth.HashPassword("not-a-real-password", auth.DefaultPasswordParams())

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeOrFail(w, r, &req) {
		return
	}
	ctx := r.Context()
	fail := func() { problem(w, http.StatusUnauthorized, "the tenant, email or password is wrong") }
	set := s.Settings.Get().Auth
	t, terr := s.Tenants.BySlug(ctx, req.Tenant)
	var u tenantdb.TenantUser
	uerr := terr
	if terr == nil {
		u, uerr = s.Q.GetUserByEmail(ctx, tenantdb.GetUserByEmailParams{TenantID: string(t.ID), Email: normalizeEmail(req.Email)})
	}
	hash := dummyHash
	if uerr == nil {
		hash = u.PasswordHash
	}
	ok, rehash, err := auth.VerifyPassword(req.Password, hash, set.Password)
	if uerr != nil || err != nil || !ok || t.Status != "active" {
		fail()
		return
	}
	if rehash {
		if h, err := auth.HashPassword(req.Password, set.Password); err == nil {
			_ = s.Q.UpdateUserPassword(ctx, tenantdb.UpdateUserPasswordParams{PasswordHash: h, ID: u.ID})
		}
	}
	resp, err := s.newUserSession(ctx, w, u, "")
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusOK, resp)
}

func clearRefresh(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: RefreshCookie, Value: "", Path: "/v1/", MaxAge: -1, HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode}) //nolint:gosec // see setRefresh
}

// handleRefresh rotates the refresh token. Presenting an already-rotated token means it leaked:
// the whole family is revoked and the caller must log in again.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := r.Cookie(RefreshCookie)
	deny := func() {
		clearRefresh(w, s.SecureCookies)
		problem(w, http.StatusUnauthorized, "the session has expired; log in again")
	}
	if err != nil || c.Value == "" {
		deny()
		return
	}
	rt, err := s.Q.GetRefreshToken(ctx, hashToken(c.Value))
	if err != nil || rt.RevokedAt.Valid || time.Now().After(rt.ExpiresAt) {
		deny()
		return
	}
	if n, err := s.Q.RotateRefreshToken(ctx, rt.ID); err != nil || n == 0 || rt.RotatedAt.Valid {
		_ = s.Q.RevokeRefreshFamily(ctx, rt.FamilyID)
		s.Log.Warn("refresh token reuse; session family revoked", "family", rt.FamilyID, "subject", rt.SubjectID)
		deny()
		return
	}
	switch rt.SubjectType {
	case tenantdb.RefreshTokensSubjectTypeTenantUser:
		u, err := s.Q.GetUser(ctx, rt.SubjectID)
		if err != nil {
			deny()
			return
		}
		resp, err := s.newUserSession(ctx, w, u, rt.FamilyID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		platform.WriteJSON(w, http.StatusOK, resp)
	default:
		resp, err := s.newAdminSession(ctx, w, rt.SubjectID, rt.FamilyID)
		if err != nil {
			s.internalError(w, err)
			return
		}
		platform.WriteJSON(w, http.StatusOK, resp)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(RefreshCookie); err == nil && c.Value != "" {
		if rt, err := s.Q.GetRefreshToken(r.Context(), hashToken(c.Value)); err == nil {
			_ = s.Q.RevokeRefreshFamily(r.Context(), rt.FamilyID)
		}
	}
	clearRefresh(w, s.SecureCookies)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sendVerification(ctx context.Context, u tenantdb.TenantUser) {
	tok, hash, err := randomToken()
	if err == nil {
		err = s.Q.CreateEmailVerification(ctx, tenantdb.CreateEmailVerificationParams{
			TokenHash: hash, UserID: u.ID, ExpiresAt: time.Now().Add(time.Duration(s.Settings.Get().Auth.EmailVerificationTTL)),
		})
	}
	if err == nil {
		link := strings.TrimRight(s.AppBaseURL, "/") + "/verify-email?token=" + tok
		err = s.Mailer.Send(ctx, u.Email, "Verify your email", "Confirm your email address for the search console:\n\n"+link+"\n\nThe link expires in "+time.Duration(s.Settings.Get().Auth.EmailVerificationTTL).String()+".\n")
	}
	if err != nil {
		s.Log.Warn("verification email not sent", "user", u.ID, "err", err)
	}
}

func (s *Server) handleVerifyEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	ctx := r.Context()
	ev, err := s.Q.GetEmailVerification(ctx, hashToken(req.Token))
	if err != nil || ev.UsedAt.Valid || time.Now().After(ev.ExpiresAt) {
		problem(w, http.StatusBadRequest, "the verification link is invalid or expired", fieldErr("/token", "invalid", "request a new link"))
		return
	}
	if n, err := s.Q.UseEmailVerification(ctx, ev.TokenHash); err != nil || n == 0 {
		problem(w, http.StatusBadRequest, "the verification link was already used", fieldErr("/token", "invalid", "request a new link"))
		return
	}
	if err := s.Q.MarkEmailVerified(ctx, ev.UserID); err != nil {
		s.internalError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

//nolint:contextcheck // r is rebound to the authenticated request's context
func (s *Server) handleResendVerification(w http.ResponseWriter, r *http.Request) {
	r, p, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	_, id, _ := strings.Cut(p.Subject, ":")
	u, err := s.Q.GetUser(r.Context(), id)
	if err != nil {
		problem(w, http.StatusNotFound, "no such user")
		return
	}
	if !u.EmailVerifiedAt.Valid {
		s.sendVerification(context.WithoutCancel(r.Context()), u)
	}
	w.WriteHeader(http.StatusAccepted)
}

type tokenRequest struct {
	APIKey string `json:"api_key"`
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// handleToken exchanges a tenant API key for a short-lived data-plane JWT.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	var req tokenRequest
	if err := decode(r, &req, 4<<10); err != nil || req.APIKey == "" {
		invalid(w, fieldErr("/api_key", "required", "api_key is required"))
		return
	}
	unauthorized := func() { problem(w, http.StatusUnauthorized, "the API key is invalid or revoked") }
	prefix, secret, err := auth.ParseAPIKey(req.APIKey)
	if err != nil {
		unauthorized()
		return
	}
	ctx := r.Context()
	key, err := s.Tenants.APIKeyByPrefix(ctx, prefix)
	if errors.Is(err, tenant.ErrNotFound) || (err == nil && !auth.CheckAPIKeySecret(secret, key.Hash)) {
		unauthorized()
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	t, err := s.Tenants.Tenant(ctx, key.TenantID)
	if err != nil || t.Status != "active" {
		problem(w, http.StatusForbidden, "the tenant is unknown or inactive")
		return
	}
	ttl := time.Duration(s.Settings.Get().Auth.APITokenTTL)
	token, err := s.Keys.IssueClaims(auth.Claims{TenantID: string(key.TenantID), Scope: strings.Join(key.Scopes, " "),
		RegisteredClaims: jwt.RegisteredClaims{Subject: "apikey:" + key.ID}}, ttl)
	if err != nil {
		s.internalError(w, err)
		return
	}
	if err := s.Tenants.TouchAPIKey(ctx, key.ID); err != nil {
		s.Log.Warn("touch api key", "err", err)
	}
	w.Header().Set("Cache-Control", "no-store")
	platform.WriteJSON(w, http.StatusOK, tokenResponse{AccessToken: token, TokenType: "Bearer", ExpiresIn: int(ttl.Seconds())})
}
