package security

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

type memRows struct {
	mu   sync.Mutex
	rows map[string]tenantdb.TenantSecret
}

func (m *memRows) CreateTenantSecret(_ context.Context, a tenantdb.CreateTenantSecretParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[a.TenantID+"/"+a.ID] = tenantdb.TenantSecret{ID: a.ID, TenantID: a.TenantID, Kind: a.Kind, Ciphertext: a.Ciphertext, DekWrapped: a.DekWrapped}
	return nil
}

func (m *memRows) GetTenantSecret(_ context.Context, a tenantdb.GetTenantSecretParams) (tenantdb.TenantSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[a.TenantID+"/"+a.ID]
	if !ok {
		return r, sql.ErrNoRows
	}
	return r, nil
}

const (
	tenantA = "01JTESTTENANT00000000000AA"
	tenantB = "01JTESTTENANT00000000000BB"
)

func TestDBSecretStoreEnvelope(t *testing.T) {
	master, err := NewSealer("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		t.Fatal(err)
	}
	rows := &memRows{rows: map[string]tenantdb.TenantSecret{}}
	s := NewDBSecretStore(rows, master)
	ctx := context.Background()

	ref, err := s.Put(ctx, tenantA, "api_key", "sk-very-secret")
	if err != nil {
		t.Fatal(err)
	}
	row := rows.rows[tenantA+"/"+ref]
	if strings.Contains(string(row.Ciphertext), "sk-very-secret") || len(row.DekWrapped) == 0 {
		t.Fatal("the value must be stored encrypted under a wrapped data key")
	}
	got, err := s.Get(ctx, tenantA, ref)
	if err != nil || got.Reveal() != "sk-very-secret" {
		t.Fatalf("Get = %q, %v", got.Reveal(), err)
	}
	if _, err := s.Get(ctx, tenantB, ref); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("another tenant's ref: %v", err)
	}
	if _, err := s.Get(ctx, tenantA, "../../etc"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("malformed ref: %v", err)
	}
	// A row copied to another tenant cannot be opened: the keys are bound to tenant and ID.
	moved := row
	moved.TenantID = tenantB
	rows.rows[tenantB+"/"+ref] = moved
	if _, err := s.Get(ctx, tenantB, ref); err == nil {
		t.Error("ciphertext moved to another tenant must not decrypt")
	}
	if _, err := s.Put(ctx, tenantA, "api_key", ""); err == nil {
		t.Error("empty values are rejected")
	}
	if _, err := s.Put(ctx, tenantA, "api_key", strings.Repeat("x", MaxSecretBytes+1)); err == nil {
		t.Error("oversized values are rejected")
	}
}

func TestSecretIsRedacted(t *testing.T) {
	s := NewSecret("hunter2")
	var buf strings.Builder
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "secret", s, "wrapped", struct{ S Secret }{s})
	js, _ := json.Marshal(map[string]any{"s": s})
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%+v %#v", s, s), buf.String(), string(js)} {
		if strings.Contains(out, "hunter2") {
			t.Errorf("secret leaked: %s", out)
		}
	}
	if s.Reveal() != "hunter2" || s.IsZero() || !NewSecret("").IsZero() {
		t.Error("Reveal/IsZero")
	}
}

// fakeVault implements the KV v2 read and write calls.
type fakeVault struct {
	mu   sync.Mutex
	data map[string]map[string]string
}

func (f *fakeVault) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Vault-Token") != "root" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Data map[string]string `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.data[r.URL.Path] = body.Data
		_, _ = w.Write([]byte(`{"data":{"version":1}}`))
	case http.MethodGet:
		d, ok := f.data[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": d}})
	}
}

func TestVaultSecretStore(t *testing.T) {
	fv := &fakeVault{data: map[string]map[string]string{}}
	srv := httptest.NewServer(fv)
	defer srv.Close()
	if _, err := NewVaultSecretStore(VaultConfig{Addr: srv.URL}); err == nil {
		t.Error("a token is required")
	}
	s, err := NewVaultSecretStore(VaultConfig{Addr: srv.URL, Token: "root"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := s.Put(ctx, tenantA, "password", "pg-secret")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fv.data["/v1/secret/data/search-engine/tenants/"+tenantA+"/"+ref]; !ok {
		t.Errorf("unexpected vault paths %v", fv.data)
	}
	got, err := s.Get(ctx, tenantA, ref)
	if err != nil || got.Reveal() != "pg-secret" {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if _, err := s.Get(ctx, tenantB, ref); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("other tenant: %v", err)
	}
	bad, _ := NewVaultSecretStore(VaultConfig{Addr: srv.URL, Token: "wrong"})
	if _, err := bad.Put(ctx, tenantA, "k", "v"); err == nil {
		t.Error("a rejected write must fail")
	}
	if _, err := bad.Get(ctx, tenantA, ref); err == nil || errors.Is(err, ErrSecretNotFound) {
		t.Errorf("a rejected read is an error, not a missing secret: %v", err)
	}
}
