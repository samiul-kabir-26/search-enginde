package controlplane

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// Server is control-api: tenants, auth, config lifecycle, onboarding, catalog and index jobs,
// the playground proxy and the platform admin API.
type Server struct {
	DB       *sql.DB
	Q        *tenantdb.Queries
	Tenants  *tenant.Store
	Keys     *auth.KeyRing
	Verifier *auth.Verifier
	Settings *Settings
	Mailer   Mailer
	Redis    *redis.Client
	Idem     *Idempotency
	// Presign creates catalog upload URLs; UploadBucket holds the uploads.
	Presign      *s3.PresignClient
	UploadBucket string
	// SearchURL is search-api's base URL, used by the playground proxy.
	SearchURL  string
	HTTPClient *http.Client
	// AppBaseURL prefixes links in emails; SecureCookies sets the Secure cookie flag.
	AppBaseURL    string
	SecureCookies bool
	// PlatformEmbedder describes platform models for config validation.
	PlatformEmbedder func(id string) (tenant.EmbedderInfo, bool)
	// BYO builds and probes tenant endpoints (secrets, egress) for models and connections.
	BYO *registry.BYO
	// Models holds learned scorer artifacts (platform manifests, tenant ONNX uploads) in
	// ModelBucket, where POST /models/uploads presigns.
	Models      ModelStore
	ModelBucket string
	// ONNXReady reports that ONNX Runtime loaded, so uploaded models can be dry-run.
	ONNXReady  bool
	ClickHouse driver.Conn
	Log        *slog.Logger

	probes probeCache
}

// Routes registers every control-api route.
func (s *Server) Routes(mux *http.ServeMux) {
	s.authRoutes(mux)
	s.accessRoutes(mux)
	s.configRoutes(mux)
	s.onboardingRoutes(mux)
	s.catalogRoutes(mux)
	s.playgroundRoutes(mux)
	s.byoRoutes(mux)
	s.adminRoutes(mux)
	s.adsRoutes(mux)
}

func problem(w http.ResponseWriter, status int, detail string, errs ...platform.FieldError) {
	platform.WriteProblem(w, platform.Problem{Status: status, Detail: detail, Errors: errs})
}

func invalid(w http.ResponseWriter, errs ...platform.FieldError) {
	problem(w, http.StatusBadRequest, "the request is invalid", errs...)
}

func (s *Server) internalError(w http.ResponseWriter, err error) {
	s.Log.Error("control-api request failed", "err", err)
	problem(w, http.StatusInternalServerError, "")
}

// decode reads a JSON body strictly (unknown fields rejected), up to max bytes.
func decode(r *http.Request, dst any, maxBytes int64) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

func decodeOrFail(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := decode(r, dst, 1<<20); err != nil {
		invalid(w, platform.FieldError{Path: "", Code: "invalid_json", Message: err.Error()})
		return false
	}
	return true
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// authenticate verifies the bearer token and stores the principal in the request context.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*http.Request, auth.Principal, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		problem(w, http.StatusUnauthorized, "a Bearer token is required")
		return r, auth.Principal{}, false
	}
	p, err := s.Verifier.Verify(r.Context(), token)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
		problem(w, http.StatusUnauthorized, "the token is invalid or expired")
		return r, auth.Principal{}, false
	}
	return r.WithContext(auth.WithPrincipal(r.Context(), p)), p, true
}

// tenantRoute guards /v1/tenants/{tid}/... routes: the token's tenant must be the path tenant
// (platform admins may act on any tenant) and hold scope.
func (s *Server) tenantRoute(scope string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, p, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if p.Has(auth.ScopePlatformAdmin) {
			h(w, r)
			return
		}
		if string(p.Tenant) != r.PathValue("tid") {
			problem(w, http.StatusNotFound, "no such tenant")
			return
		}
		if !p.Has(scope) {
			problem(w, http.StatusForbidden, "this action needs "+scope)
			return
		}
		h(w, r)
	})
}

// adminRoute guards /v1/admin/... routes.
func (s *Server) adminRoute(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, p, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if !p.Has(auth.ScopePlatformAdmin) {
			problem(w, http.StatusForbidden, "platform admins only")
			return
		}
		h(w, r)
	})
}

func pathTenant(r *http.Request) tenant.ID { return tenant.ID(r.PathValue("tid")) }

func principal(r *http.Request) auth.Principal {
	p, _ := auth.FromContext(r.Context())
	return p
}

// actor is the user ID recorded in audit columns.
func actor(r *http.Request) string {
	sub := principal(r).Subject
	if _, id, ok := strings.Cut(sub, ":"); ok {
		return id
	}
	return ""
}
