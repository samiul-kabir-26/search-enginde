package searchapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/pipeline"
)

// SnapshotTTL is how long an ordered result list stays pageable (PLAN.md R4).
const SnapshotTTL = 10 * time.Minute

// degradedTTL keeps a degraded result briefly, so paging works without pinning a failure.
const degradedTTL = 30 * time.Second

// Snapshot is the ordered result list of one query, cached so every paging mode is stable.
type Snapshot struct {
	ConfigVersion int                              `json:"v"`
	Items         []SnapshotItem                   `json:"items"`
	Facets        map[string][]pipeline.FacetValue `json:"facets,omitempty"`
	TotalMatches  int64                            `json:"total_matches"`
	Degraded      bool                             `json:"degraded,omitempty"`
	Query         QueryInfo                        `json:"query"`
}

// SnapshotItem is one ordered result.
type SnapshotItem struct {
	ID        string  `json:"id"`
	Score     float64 `json:"s"`
	Sponsored bool    `json:"sp,omitempty"`
}

// QueryInfo echoes how the query was understood.
type QueryInfo struct {
	Normalized string                   `json:"normalized"`
	Attributes map[string][]string      `json:"attributes,omitempty"`
	Categories []pipeline.CategoryGuess `json:"categories,omitempty"`
}

// Snapshots stores snapshots in Redis under `snap:{tenant}:{key}`.
type Snapshots struct {
	rdb *redis.Client
}

// NewSnapshots returns a Redis-backed snapshot store.
func NewSnapshots(rdb *redis.Client) *Snapshots { return &Snapshots{rdb: rdb} }

func snapKey(tenant, key string) string { return "snap:{" + tenant + "}:" + key }

// Get returns the snapshot, or ok=false when it is missing or expired.
func (s *Snapshots) Get(ctx context.Context, tenant, key string) (*Snapshot, bool, error) {
	b, err := s.rdb.Get(ctx, snapKey(tenant, key)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, false, fmt.Errorf("decode snapshot: %w", err)
	}
	return &snap, true, nil
}

// Put stores a snapshot; degraded ones expire quickly.
func (s *Snapshots) Put(ctx context.Context, tenant, key string, snap *Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("encode snapshot: %w", err)
	}
	ttl := SnapshotTTL
	if snap.Degraded {
		ttl = degradedTTL
	}
	if err := s.rdb.Set(ctx, snapKey(tenant, key), b, ttl).Err(); err != nil {
		return fmt.Errorf("put snapshot: %w", err)
	}
	return nil
}

// SnapshotKey identifies a query's result list: everything that changes the order.
func SnapshotKey(parts ...any) (string, error) {
	b, err := json.Marshal(parts)
	if err != nil {
		return "", fmt.Errorf("snapshot key: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16]), nil
}

// Cursor is the signed position in a snapshot.
type Cursor struct {
	Snapshot      string `json:"s"`
	Offset        int    `json:"o"`
	ConfigVersion int    `json:"v"`
}

// Cursors signs and verifies cursors with HMAC-SHA256, bound to the tenant.
type Cursors struct {
	key []byte
}

// NewCursors returns a cursor codec for a secret key.
func NewCursors(key []byte) *Cursors { return &Cursors{key: key} }

// ErrInvalidCursor covers malformed, tampered and foreign cursors.
var ErrInvalidCursor = errors.New("invalid cursor")

func (c *Cursors) mac(tenant string, payload []byte) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(tenant))
	m.Write([]byte{0})
	m.Write(payload)
	return m.Sum(nil)[:16]
}

// Encode returns an opaque cursor.
func (c *Cursors) Encode(tenant string, cur Cursor) string {
	payload, _ := json.Marshal(cur)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(c.mac(tenant, payload))
}

// Decode verifies and parses a cursor issued to tenant.
func (c *Cursors) Decode(tenant, s string) (Cursor, error) {
	p, sig, ok := strings.Cut(s, ".")
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(p)
	mac, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || !hmac.Equal(mac, c.mac(tenant, payload)) {
		return Cursor{}, ErrInvalidCursor
	}
	var cur Cursor
	if err := json.Unmarshal(payload, &cur); err != nil || cur.Offset < 0 || cur.Snapshot == "" {
		return Cursor{}, ErrInvalidCursor
	}
	return cur, nil
}
