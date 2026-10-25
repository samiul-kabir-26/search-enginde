package auth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Verifier checks EdDSA tokens against a JWKS that it fetches, caches and refreshes: on a
// schedule, and on an unknown kid (at most once per minRefresh) so key rotation needs no restart.
type Verifier struct {
	url        string
	issuer     string
	client     *http.Client
	minRefresh time.Duration
	log        *slog.Logger

	mu        sync.RWMutex
	keys      map[string]ed25519.PublicKey
	fetchedAt time.Time
}

// NewVerifier returns a verifier for tokens issued by issuer and signed by keys at jwksURL.
func NewVerifier(jwksURL, issuer string, log *slog.Logger) *Verifier {
	return &Verifier{
		url: jwksURL, issuer: issuer, client: &http.Client{Timeout: 5 * time.Second},
		minRefresh: 30 * time.Second, log: log, keys: map[string]ed25519.PublicKey{},
	}
}

// WithKeys preloads keys (tests, or an in-process issuer).
func (v *Verifier) WithKeys(set JWKS) *Verifier {
	keys, _ := parseJWKS(set)
	v.mu.Lock()
	v.keys, v.fetchedAt = keys, time.Now()
	v.mu.Unlock()
	return v
}

// Refresh fetches the JWKS now.
func (v *Verifier) Refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return fmt.Errorf("jwks request: %w", err)
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch jwks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch jwks: %s", resp.Status)
	}
	var set JWKS
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys, err := parseJWKS(set)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys, v.fetchedAt = keys, time.Now()
	v.mu.Unlock()
	return nil
}

// Run refreshes the JWKS every interval until ctx ends.
func (v *Verifier) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := v.Refresh(ctx); err != nil && ctx.Err() == nil {
				v.log.Warn("jwks refresh failed", "err", err)
			}
		}
	}
}

func parseJWKS(set JWKS) (map[string]ed25519.PublicKey, error) {
	keys := map[string]ed25519.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "OKP" || k.Crv != "Ed25519" {
			continue
		}
		x, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("jwks key %q: invalid x", k.Kid)
		}
		keys[k.Kid] = ed25519.PublicKey(x)
	}
	return keys, nil
}

func (v *Verifier) key(ctx context.Context, kid string) (ed25519.PublicKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	stale := time.Since(v.fetchedAt) >= v.minRefresh
	v.mu.RUnlock()
	if ok {
		return k, nil
	}
	if !stale {
		return nil, fmt.Errorf("unknown signing key %q", kid)
	}
	if err := v.Refresh(ctx); err != nil {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("unknown signing key %q", kid)
}

// ErrUnauthenticated wraps every token verification failure.
var ErrUnauthenticated = errors.New("unauthenticated")

// Verify parses and validates a token: EdDSA only, known kid, issuer, expiry, tid present.
func (v *Verifier) Verify(ctx context.Context, token string) (Principal, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.key(ctx, kid)
	}, jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}), jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(), jwt.WithLeeway(30*time.Second))
	if err != nil {
		return Principal{}, fmt.Errorf("%w: %w", ErrUnauthenticated, err)
	}
	id, err := tenant.ParseID(c.TenantID)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: tid: %w", ErrUnauthenticated, err)
	}
	scopes := map[string]bool{}
	for _, s := range strings.Fields(c.Scope) {
		scopes[s] = true
	}
	return Principal{Tenant: id, Subject: c.Subject, Scopes: scopes}, nil
}

// Middleware authenticates the bearer token and requires scope. The tenant comes only from the
// token's tid claim.
func Middleware(v *Verifier, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || token == "" {
				w.Header().Set("WWW-Authenticate", `Bearer`)
				platform.WriteProblem(w, platform.Problem{Status: http.StatusUnauthorized, Detail: "a Bearer token is required"})
				return
			}
			p, err := v.Verify(r.Context(), token)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
				platform.WriteProblem(w, platform.Problem{Status: http.StatusUnauthorized, Detail: "the token is invalid or expired"})
				return
			}
			if !p.Has(scope) {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer error="insufficient_scope", scope=%q`, scope))
				platform.WriteProblem(w, platform.Problem{Status: http.StatusForbidden, Detail: "the token lacks scope " + scope})
				return
			}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
		})
	}
}
