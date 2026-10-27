package ads

import (
	"context"
	"sync"
	"time"

	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// Store provides access to active ad creatives for ranking and blending.
type Store interface {
	ListActiveCreatives(ctx context.Context, tid tenant.ID) ([]ActiveCreative, error)
}

// DBStore implements Store using tenantdb.Queries with in-memory caching.
type DBStore struct {
	q   *tenantdb.Queries
	ttl time.Duration

	mu    sync.RWMutex
	cache map[tenant.ID]cacheEntry
}

type cacheEntry struct {
	items   []ActiveCreative
	expires time.Time
}

// NewDBStore returns a new DBStore caching active creatives for the given TTL.
func NewDBStore(q *tenantdb.Queries, ttl time.Duration) *DBStore {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &DBStore{
		q:     q,
		ttl:   ttl,
		cache: make(map[tenant.ID]cacheEntry),
	}
}

// Invalidate clears the cache for a tenant (e.g. after campaign edits).
func (s *DBStore) Invalidate(tid tenant.ID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, tid)
}

// ListActiveCreatives returns active creatives for the tenant, serving from cache if fresh.
func (s *DBStore) ListActiveCreatives(ctx context.Context, tid tenant.ID) ([]ActiveCreative, error) {
	now := time.Now()

	s.mu.RLock()
	entry, ok := s.cache[tid]
	s.mu.RUnlock()

	if ok && now.Before(entry.expires) {
		return entry.items, nil
	}

	rows, err := s.q.ListActiveCampaignCreatives(ctx, string(tid))
	if err != nil {
		// On error, if we have stale cache, return it rather than failing
		if ok {
			return entry.items, nil
		}
		return nil, err
	}

	out := make([]ActiveCreative, 0, len(rows))
	for _, r := range rows {
		var endAt *time.Time
		if r.EndAt.Valid {
			endAt = &r.EndAt.Time
		}
		out = append(out, ActiveCreative{
			CampaignID:       r.CampaignID,
			TenantID:         tenant.ID(r.TenantID),
			VendorID:         r.VendorID,
			DailyBudgetMinor: r.DailyBudgetMinor,
			Currency:         r.Currency,
			BidMinor:         r.BidMinor,
			Targeting:        TargetingFromRaw(r.Targeting),
			StartAt:          r.StartAt,
			EndAt:            endAt,
			CreativeID:       r.CreativeID,
			ProductID:        r.ProductID,
		})
	}

	s.mu.Lock()
	s.cache[tid] = cacheEntry{
		items:   out,
		expires: now.Add(s.ttl),
	}
	s.mu.Unlock()

	return out, nil
}
