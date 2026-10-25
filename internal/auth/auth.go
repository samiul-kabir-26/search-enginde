package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/zavisoft/search-engine/internal/tenant"
)

// Scopes (api/openapi/control.yaml Scope).
const (
	ScopeSearchRead  = "search:read"
	ScopeSearchDebug = "search:debug"
	ScopeEventsWrite = "events:write"
)

// Claims are the JWT claims (docs/API.md): tid, sub, scope (space separated), exp.
type Claims struct {
	TenantID string `json:"tid"`
	Scope    string `json:"scope"`
	jwt.RegisteredClaims
}

// Principal is the authenticated caller.
type Principal struct {
	Tenant  tenant.ID
	Subject string
	Scopes  map[string]bool
}

// Has reports whether the principal holds scope.
func (p Principal) Has(scope string) bool { return p.Scopes[scope] }

type ctxKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the authenticated principal, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Signer issues EdDSA (Ed25519) tokens. Key rotation and storage move to the control plane in
// M4 (PLAN.md R7); until then the key comes from configuration.
type Signer struct {
	key    ed25519.PrivateKey
	kid    string
	issuer string
}

// NewSigner builds a signer from a base64 32-byte Ed25519 seed.
func NewSigner(seedB64, issuer string) (*Signer, error) {
	seed, err := base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("signing key: want a %d-byte seed, got %d", ed25519.SeedSize, len(seed))
	}
	key := ed25519.NewKeyFromSeed(seed)
	return &Signer{key: key, kid: KeyID(key.Public().(ed25519.PublicKey)), issuer: issuer}, nil
}

// KeyID derives a stable key ID from a public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base64.RawURLEncoding.EncodeToString(sum[:12])
}

// Issue signs a token for a tenant subject with the given scopes and lifetime.
func (s *Signer) Issue(t tenant.ID, subject string, scopes []string, ttl time.Duration) (string, error) {
	now := time.Now()
	c := Claims{
		TenantID: string(t),
		Scope:    strings.Join(scopes, " "),
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: s.issuer, Subject: subject,
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, c)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// JWK is an OKP public key (RFC 8037).
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
}

// JWKS is a JSON Web Key Set.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWKS returns the signer's public key set.
func (s *Signer) JWKS() JWKS {
	pub := s.key.Public().(ed25519.PublicKey)
	return JWKS{Keys: []JWK{{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub), Kid: s.kid, Alg: "EdDSA", Use: "sig"}}}
}

// API keys look like `sek_<prefix>_<secret>`. Only the prefix (for lookup) and the SHA-256 of
// the secret are stored; secrets are 256-bit random, so a fast hash is sufficient.
const apiKeyTag = "sek"

// NewAPIKey returns a fresh key, its lookup prefix and the hash to store.
func NewAPIKey() (key, prefix string, hash []byte, err error) {
	p := make([]byte, 6)
	sec := make([]byte, 32)
	if _, err := rand.Read(p); err != nil {
		return "", "", nil, fmt.Errorf("api key: %w", err)
	}
	if _, err := rand.Read(sec); err != nil {
		return "", "", nil, fmt.Errorf("api key: %w", err)
	}
	prefix = hex.EncodeToString(p)
	secret := base64.RawURLEncoding.EncodeToString(sec)
	sum := sha256.Sum256([]byte(secret))
	return apiKeyTag + "_" + prefix + "_" + secret, prefix, sum[:], nil
}

// ErrInvalidAPIKey is returned for malformed, unknown or revoked keys.
var ErrInvalidAPIKey = errors.New("invalid api key")

// ParseAPIKey splits a key into its prefix and secret.
func ParseAPIKey(key string) (prefix, secret string, err error) {
	parts := strings.SplitN(key, "_", 3)
	if len(parts) != 3 || parts[0] != apiKeyTag || len(parts[1]) != 12 || parts[2] == "" {
		return "", "", ErrInvalidAPIKey
	}
	return parts[1], parts[2], nil
}

// CheckAPIKeySecret compares a secret with a stored hash in constant time.
func CheckAPIKeySecret(secret string, hash []byte) bool {
	sum := sha256.Sum256([]byte(secret))
	return subtle.ConstantTimeCompare(sum[:], hash) == 1
}
