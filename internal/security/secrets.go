package security

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// Secret is a credential value. It prints, logs and marshals as "[redacted]"; only Reveal
// returns the value.
type Secret struct{ v string }

// NewSecret wraps a value.
func NewSecret(v string) Secret { return Secret{v: v} }

// Reveal returns the value, for the one place that sends it to the endpoint.
func (s Secret) Reveal() string { return s.v }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.v == "" }

const redacted = "[redacted]"

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }

// MaxSecretBytes bounds a secret value.
const MaxSecretBytes = 4096

// ErrSecretNotFound means the reference does not exist or belongs to another tenant.
var ErrSecretNotFound = errors.New("secret not found")

// SecretStore keeps tenant credentials for BYO providers (docs/ARCHITECTURE.md §9). Config
// documents and connections hold only the returned secret_ref; values are never returned by
// an API or logged.
type SecretStore interface {
	Put(ctx context.Context, tenantID, kind, value string) (ref string, err error)
	Get(ctx context.Context, tenantID, ref string) (Secret, error)
}

// validRef checks a reference before it is used in a query or a Vault path.
func validRef(ref string) bool {
	_, err := ulid.ParseStrict(ref)
	return err == nil
}

// DBSecretStore is the dev SecretStore: envelope encryption in MySQL `tenant_secrets`. Each
// secret has its own random 256-bit data key (AES-256-GCM, bound to tenant and ID); the data
// key is stored wrapped by the master key, so rotating the master key re-wraps keys only.
type DBSecretStore struct {
	q      SecretRows
	master *Sealer
}

// SecretRows is the part of tenantdb.Queries the store uses.
type SecretRows interface {
	CreateTenantSecret(ctx context.Context, arg tenantdb.CreateTenantSecretParams) error
	GetTenantSecret(ctx context.Context, arg tenantdb.GetTenantSecretParams) (tenantdb.TenantSecret, error)
}

// NewDBSecretStore returns the MySQL-backed store.
func NewDBSecretStore(q SecretRows, master *Sealer) *DBSecretStore {
	return &DBSecretStore{q: q, master: master}
}

func secretAAD(tenantID, id string) []byte { return []byte("secret:" + tenantID + "/" + id) }
func dekAAD(tenantID, id string) []byte    { return []byte("dek:" + tenantID + "/" + id) }

// Put encrypts and stores value, returning its reference.
func (s *DBSecretStore) Put(ctx context.Context, tenantID, kind, value string) (string, error) {
	if len(value) == 0 || len(value) > MaxSecretBytes {
		return "", fmt.Errorf("secret value must be 1 to %d bytes", MaxSecretBytes)
	}
	id := ulid.Make().String()
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return "", fmt.Errorf("data key: %w", err)
	}
	data, err := sealerFromKey(dek)
	if err != nil {
		return "", fmt.Errorf("data key: %w", err)
	}
	ct, err := data.Seal([]byte(value), secretAAD(tenantID, id))
	if err != nil {
		return "", err
	}
	wrapped, err := s.master.Seal(dek, dekAAD(tenantID, id))
	if err != nil {
		return "", err
	}
	if err := s.q.CreateTenantSecret(ctx, tenantdb.CreateTenantSecretParams{
		ID: id, TenantID: tenantID, Kind: kind, Ciphertext: ct, DekWrapped: wrapped,
	}); err != nil {
		return "", fmt.Errorf("store secret: %w", err)
	}
	return id, nil
}

// Get decrypts the tenant's secret ref.
func (s *DBSecretStore) Get(ctx context.Context, tenantID, ref string) (Secret, error) {
	if !validRef(ref) {
		return Secret{}, ErrSecretNotFound
	}
	row, err := s.q.GetTenantSecret(ctx, tenantdb.GetTenantSecretParams{TenantID: tenantID, ID: ref})
	if errors.Is(err, sql.ErrNoRows) {
		return Secret{}, ErrSecretNotFound
	}
	if err != nil {
		return Secret{}, fmt.Errorf("load secret: %w", err)
	}
	dek, err := s.master.Open(row.DekWrapped, dekAAD(tenantID, ref))
	if err != nil {
		return Secret{}, fmt.Errorf("unwrap data key: %w", err)
	}
	data, err := sealerFromKey(dek)
	if err != nil {
		return Secret{}, fmt.Errorf("data key: %w", err)
	}
	v, err := data.Open(row.Ciphertext, secretAAD(tenantID, ref))
	if err != nil {
		return Secret{}, fmt.Errorf("decrypt secret: %w", err)
	}
	return Secret{v: string(v)}, nil
}

// VaultConfig points at a Vault KV v2 mount.
type VaultConfig struct {
	Addr  string
	Token string
	// Mount is the KV v2 mount (default "secret"); Prefix is the path under it (default
	// "search-engine/tenants").
	Mount  string
	Prefix string
	Client *http.Client
}

// VaultSecretStore is the production SecretStore: tenant secrets live in a Vault KV v2 mount
// at {mount}/data/{prefix}/{tenant}/{ref}, so the tenant is part of every path.
type VaultSecretStore struct {
	base   *url.URL
	token  string
	mount  string
	prefix string
	client *http.Client
}

// NewVaultSecretStore validates cfg and returns the store.
func NewVaultSecretStore(cfg VaultConfig) (*VaultSecretStore, error) {
	u, err := url.Parse(cfg.Addr)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("vault addr %q is not a URL", cfg.Addr)
	}
	if cfg.Token == "" {
		return nil, errors.New("vault token is required")
	}
	s := &VaultSecretStore{base: u, token: cfg.Token, mount: cfg.Mount, prefix: cfg.Prefix, client: cfg.Client}
	if s.mount == "" {
		s.mount = "secret"
	}
	if s.prefix == "" {
		s.prefix = "search-engine/tenants"
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: 5 * time.Second}
	}
	return s, nil
}

func (s *VaultSecretStore) path(tenantID, ref string) string {
	return s.base.JoinPath("v1", s.mount, "data", strings.Trim(s.prefix, "/"), tenantID, ref).String()
}

func (s *VaultSecretStore) do(ctx context.Context, method, u string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("vault request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("vault request: %w", err)
	}
	req.Header.Set("X-Vault-Token", s.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault %s: %w", method, err)
	}
	return resp, nil
}

// Put writes value under a new reference.
func (s *VaultSecretStore) Put(ctx context.Context, tenantID, kind, value string) (string, error) {
	if len(value) == 0 || len(value) > MaxSecretBytes {
		return "", fmt.Errorf("secret value must be 1 to %d bytes", MaxSecretBytes)
	}
	if !validRef(tenantID) {
		return "", fmt.Errorf("tenant id %q is not a ULID", tenantID)
	}
	ref := ulid.Make().String()
	resp, err := s.do(ctx, http.MethodPost, s.path(tenantID, ref), map[string]any{
		"options": map[string]any{"cas": 0},
		"data":    map[string]string{"kind": kind, "value": value},
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("vault write: %s", resp.Status)
	}
	return ref, nil
}

// Get reads the tenant's secret ref.
func (s *VaultSecretStore) Get(ctx context.Context, tenantID, ref string) (Secret, error) {
	if !validRef(ref) || !validRef(tenantID) {
		return Secret{}, ErrSecretNotFound
	}
	resp, err := s.do(ctx, http.MethodGet, s.path(tenantID, ref), nil)
	if err != nil {
		return Secret{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Secret{}, ErrSecretNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return Secret{}, fmt.Errorf("vault read: %s", resp.Status)
	}
	var out struct {
		Data struct {
			Data struct {
				Value string `json:"value"`
			} `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return Secret{}, fmt.Errorf("vault read: %w", err)
	}
	if out.Data.Data.Value == "" {
		return Secret{}, ErrSecretNotFound
	}
	return Secret{v: out.Data.Data.Value}, nil
}

// SecretBackend selects and configures the SecretStore.
type SecretBackend struct {
	// Backend is "mysql" (dev: envelope encryption under MasterKey) or "vault".
	Backend   string
	MasterKey string
	Vault     VaultConfig
}

// SecretBackendFromEnv reads SECRETS_BACKEND (mysql or vault) and the settings it needs:
// SECRETS_MASTER_KEY, or VAULT_ADDR, VAULT_TOKEN, VAULT_KV_MOUNT and VAULT_PREFIX.
func SecretBackendFromEnv(env *platform.Env) SecretBackend {
	b := SecretBackend{Backend: env.String("SECRETS_BACKEND", "mysql")}
	if b.Backend == "vault" {
		b.Vault = VaultConfig{Addr: env.Required("VAULT_ADDR"), Token: env.Required("VAULT_TOKEN"),
			Mount: env.String("VAULT_KV_MOUNT", "secret"), Prefix: env.String("VAULT_PREFIX", "search-engine/tenants")}
	} else {
		b.MasterKey = env.Required("SECRETS_MASTER_KEY")
	}
	return b
}

// NewSecretStore builds the configured SecretStore; rows serves the mysql backend.
func NewSecretStore(cfg SecretBackend, rows SecretRows) (SecretStore, error) {
	switch cfg.Backend {
	case "", "mysql":
		master, err := NewSealer(cfg.MasterKey)
		if err != nil {
			return nil, err
		}
		return NewDBSecretStore(rows, master), nil
	case "vault":
		return NewVaultSecretStore(cfg.Vault)
	}
	return nil, fmt.Errorf("secrets backend %q is not mysql or vault", cfg.Backend)
}

// Hosts splits a comma-separated host list and adds the hosts of the given URLs (the
// platform's own endpoints, which egress trusts).
func Hosts(list string, urls ...string) []string {
	var out []string
	for _, h := range strings.Split(list, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	for _, raw := range urls {
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			out = append(out, u.Host)
		}
	}
	return out
}
