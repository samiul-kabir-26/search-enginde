package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/zavisoft/search-engine/internal/tenant"
)

const tid = tenant.ID("01JTENANT0000000000000000A")

func signer(t *testing.T, seed byte) *Signer {
	t.Helper()
	b := make([]byte, 32)
	b[0] = seed
	s, err := NewSigner(base64.StdEncoding.EncodeToString(b), "http://issuer")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestIssueAndVerifyViaJWKS(t *testing.T) {
	s := signer(t, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(s.JWKS())
	}))
	defer srv.Close()
	v := NewVerifier(srv.URL, "http://issuer", logger())
	v.minRefresh = 0

	tok, err := s.Issue(tid, "apikey:1", []string{ScopeSearchRead, ScopeSearchDebug}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if p.Tenant != tid || p.Subject != "apikey:1" || !p.Has(ScopeSearchDebug) || p.Has(ScopeEventsWrite) {
		t.Errorf("principal = %+v", p)
	}
}

func TestVerifyRejects(t *testing.T) {
	good := signer(t, 1)
	v := NewVerifier("http://unused", "http://issuer", logger()).WithKeys(good.JWKS())
	expired, _ := good.Issue(tid, "s", nil, -time.Hour)
	foreign, _ := signer(t, 2).Issue(tid, "s", nil, time.Minute)
	badTenant, _ := good.Issue("not-a-ulid", "s", nil, time.Minute)

	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{TenantID: string(tid), RegisteredClaims: jwt.RegisteredClaims{
		Issuer: "http://issuer", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute))}})
	hs.Header["kid"] = good.kid
	hmacTok, _ := hs.SignedString([]byte("secret"))

	otherIssuer := &Signer{key: good.key, kid: good.kid, issuer: "http://evil"}
	wrongIss, _ := otherIssuer.Issue(tid, "s", nil, time.Minute)

	for name, tok := range map[string]string{
		"expired": expired, "unknown key": foreign, "bad tid": badTenant, "hmac alg": hmacTok,
		"wrong issuer": wrongIss, "garbage": "a.b.c",
	} {
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}
}

func TestMiddleware(t *testing.T) {
	s := signer(t, 1)
	v := NewVerifier("http://unused", "http://issuer", logger()).WithKeys(s.JWKS())
	h := Middleware(v, ScopeSearchRead)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := FromContext(r.Context())
		_, _ = w.Write([]byte(p.Tenant))
	}))
	readTok, _ := s.Issue(tid, "s", []string{ScopeSearchRead}, time.Minute)
	writeTok, _ := s.Issue(tid, "s", []string{ScopeEventsWrite}, time.Minute)
	for _, tc := range []struct {
		header string
		code   int
	}{
		{"", http.StatusUnauthorized},
		{"Basic abc", http.StatusUnauthorized},
		{"Bearer nope", http.StatusUnauthorized},
		{"Bearer " + writeTok, http.StatusForbidden},
		{"Bearer " + readTok, http.StatusOK},
	} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search", nil)
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.code {
			t.Errorf("%q: code %d, want %d (%s)", tc.header, rec.Code, tc.code, rec.Body)
		}
		if tc.code == http.StatusOK && rec.Body.String() != string(tid) {
			t.Errorf("principal not in context: %s", rec.Body)
		}
		if tc.code != http.StatusOK && !strings.Contains(rec.Header().Get("Content-Type"), "problem+json") {
			t.Errorf("%q: want problem+json", tc.header)
		}
	}
}

func TestAPIKeys(t *testing.T) {
	key, prefix, hash, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	p, secret, err := ParseAPIKey(key)
	if err != nil || p != prefix || !CheckAPIKeySecret(secret, hash) {
		t.Fatalf("round trip failed: %v", err)
	}
	if CheckAPIKeySecret(secret+"x", hash) {
		t.Error("wrong secret accepted")
	}
	for _, bad := range []string{"", "sek_short_x", "pk_" + prefix + "_" + secret, "sek_" + prefix + "_"} {
		if _, _, err := ParseAPIKey(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, err := NewSigner("bm90IDMyIGJ5dGVz", "x"); err == nil {
		t.Error("short seed accepted")
	}
}
