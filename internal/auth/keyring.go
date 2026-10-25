package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// KeyRing holds the control plane's Ed25519 signing keys in MySQL (private keys sealed with the
// master key). One key is active; retired keys stay in the JWKS until their publish_until so
// tokens they signed keep verifying, and revoked keys disappear at once (PLAN.md R7).
type KeyRing struct {
	db     *sql.DB
	q      *tenantdb.Queries
	sealer *security.Sealer
	issuer string
	log    *slog.Logger
	now    func() time.Time

	mu        sync.RWMutex
	active    *Signer
	activeAt  time.Time
	published map[string]ed25519.PublicKey
}

// NewKeyRing returns a key ring; call Load before use.
func NewKeyRing(db *sql.DB, sealer *security.Sealer, issuer string, log *slog.Logger) *KeyRing {
	return &KeyRing{db: db, q: tenantdb.New(db), sealer: sealer, issuer: issuer, log: log, now: time.Now}
}

// KeyInfo describes a key for the admin API.
type KeyInfo struct {
	Kid          string     `json:"kid"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	RetiredAt    *time.Time `json:"retired_at,omitempty"`
	PublishUntil *time.Time `json:"publish_until,omitempty"`
}

const rotationLock = "search_engine_signing_key_rotation"

// withLock serializes key changes across control-api replicas with a MySQL advisory lock.
func (k *KeyRing) withLock(ctx context.Context, fn func() error) error {
	conn, err := k.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("key lock: %w", err)
	}
	defer conn.Close()
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 10)", rotationLock).Scan(&got); err != nil {
		return fmt.Errorf("key lock: %w", err)
	}
	if got.Int64 != 1 {
		return errors.New("key lock: timed out")
	}
	defer func() { _, _ = conn.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK(?)", rotationLock) }()
	return fn()
}

// Load reads the keys, creating the first active key when there is none.
func (k *KeyRing) Load(ctx context.Context) error {
	if err := k.reload(ctx); err != nil {
		return err
	}
	k.mu.RLock()
	ok := k.active != nil
	k.mu.RUnlock()
	if ok {
		return nil
	}
	return k.withLock(ctx, func() error {
		if err := k.reload(ctx); err != nil {
			return err
		}
		k.mu.RLock()
		ok := k.active != nil
		k.mu.RUnlock()
		if ok {
			return nil
		}
		if _, err := k.create(ctx); err != nil {
			return err
		}
		return k.reload(ctx)
	})
}

func (k *KeyRing) reload(ctx context.Context) error {
	rows, err := k.q.ListSigningKeys(ctx)
	if err != nil {
		return fmt.Errorf("list signing keys: %w", err)
	}
	now := k.now()
	published := map[string]ed25519.PublicKey{}
	var active *Signer
	var activeAt time.Time
	for _, r := range rows {
		switch r.Status {
		case tenantdb.SigningKeysStatusActive:
			if active != nil {
				published[r.Kid] = r.PublicKey
				continue
			}
			seed, err := k.sealer.Open(r.PrivateSealed, []byte(r.Kid))
			if err != nil {
				return fmt.Errorf("signing key %s: %w", r.Kid, err)
			}
			active = &Signer{key: ed25519.NewKeyFromSeed(seed), kid: r.Kid, issuer: k.issuer}
			activeAt = r.CreatedAt
			published[r.Kid] = r.PublicKey
		case tenantdb.SigningKeysStatusRetired:
			if r.PublishUntil.Valid && r.PublishUntil.Time.After(now) {
				published[r.Kid] = r.PublicKey
			}
		}
	}
	k.mu.Lock()
	k.active, k.activeAt, k.published = active, activeAt, published
	k.mu.Unlock()
	return nil
}

func (k *KeyRing) create(ctx context.Context) (string, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return "", fmt.Errorf("signing key: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	kid := KeyID(pub)
	sealed, err := k.sealer.Seal(seed, []byte(kid))
	if err != nil {
		return "", err
	}
	if err := k.q.InsertSigningKey(ctx, tenantdb.InsertSigningKeyParams{Kid: kid, PublicKey: pub, PrivateSealed: sealed}); err != nil {
		return "", fmt.Errorf("insert signing key: %w", err)
	}
	k.log.Info("signing key created", "kid", kid)
	return kid, nil
}

// Rotate makes a new key active; the previous one keeps verifying for retain.
func (k *KeyRing) Rotate(ctx context.Context, retain time.Duration) (string, error) {
	var kid string
	err := k.withLock(ctx, func() error {
		k.mu.RLock()
		old := k.active
		k.mu.RUnlock()
		var err error
		if kid, err = k.create(ctx); err != nil {
			return err
		}
		if old != nil {
			if err := k.q.RetireSigningKey(ctx, tenantdb.RetireSigningKeyParams{
				PublishUntil: sql.NullTime{Time: k.now().Add(retain), Valid: true}, Kid: old.kid,
			}); err != nil {
				return fmt.Errorf("retire signing key: %w", err)
			}
		}
		return k.reload(ctx)
	})
	return kid, err
}

// ErrKeyNotFound is returned when revoking an unknown key.
var ErrKeyNotFound = errors.New("signing key not found")

// Revoke removes a key from the JWKS immediately (tokens it signed stop verifying once
// verifiers refresh). Revoking the active key rotates first.
func (k *KeyRing) Revoke(ctx context.Context, kid string) error {
	return k.withLock(ctx, func() error {
		k.mu.RLock()
		isActive := k.active != nil && k.active.kid == kid
		k.mu.RUnlock()
		if isActive {
			if _, err := k.create(ctx); err != nil {
				return err
			}
		}
		n, err := k.q.RevokeSigningKey(ctx, kid)
		if err != nil {
			return fmt.Errorf("revoke signing key: %w", err)
		}
		if n == 0 {
			return ErrKeyNotFound
		}
		k.log.Warn("signing key revoked", "kid", kid)
		return k.reload(ctx)
	})
}

// Keys lists every key for the admin API.
func (k *KeyRing) Keys(ctx context.Context) ([]KeyInfo, error) {
	rows, err := k.q.ListSigningKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("list signing keys: %w", err)
	}
	out := make([]KeyInfo, len(rows))
	for i, r := range rows {
		out[i] = KeyInfo{Kid: r.Kid, Status: string(r.Status), CreatedAt: r.CreatedAt}
		if r.RetiredAt.Valid {
			out[i].RetiredAt = &r.RetiredAt.Time
		}
		if r.PublishUntil.Valid {
			out[i].PublishUntil = &r.PublishUntil.Time
		}
	}
	return out, nil
}

// ErrNoSigningKey means Load was not called or failed.
var ErrNoSigningKey = errors.New("no active signing key")

// IssueClaims signs with the active key.
func (k *KeyRing) IssueClaims(c Claims, ttl time.Duration) (string, error) {
	k.mu.RLock()
	s := k.active
	k.mu.RUnlock()
	if s == nil {
		return "", ErrNoSigningKey
	}
	return s.IssueClaims(c, ttl)
}

// PublicKey returns a published key (KeySource).
func (k *KeyRing) PublicKey(kid string) (ed25519.PublicKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	pub, ok := k.published[kid]
	return pub, ok
}

// JWKS returns the published public keys.
func (k *KeyRing) JWKS() JWKS {
	k.mu.RLock()
	defer k.mu.RUnlock()
	set := JWKS{Keys: []JWK{}}
	for kid, pub := range k.published {
		set.Keys = append(set.Keys, JWK{Kty: "OKP", Crv: "Ed25519", X: base64.RawURLEncoding.EncodeToString(pub), Kid: kid, Alg: "EdDSA", Use: "sig"})
	}
	return set
}

// Run reloads the keys every interval (other replicas may rotate) and rotates when the active
// key is older than rotateEvery (read from settings each time).
func (k *KeyRing) Run(ctx context.Context, interval time.Duration, policy func() (rotateEvery, retain time.Duration)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if err := k.reload(ctx); err != nil {
			k.log.Warn("reload signing keys", "err", err)
			continue
		}
		every, retain := policy()
		k.mu.RLock()
		due := k.active != nil && k.now().Sub(k.activeAt) >= every
		k.mu.RUnlock()
		if due {
			if kid, err := k.Rotate(ctx, retain); err != nil {
				k.log.Error("scheduled key rotation failed", "err", err)
			} else {
				k.log.Info("signing key rotated on schedule", "kid", kid)
			}
		}
	}
}
