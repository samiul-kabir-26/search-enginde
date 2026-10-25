package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
)

// Resolved is everything the data plane needs about a tenant for one request: its config
// version, compiled forms derived from it, and its plan.
type Resolved struct {
	Tenant        Tenant
	Plan          Plan
	ConfigVersion int
	Config        *Config
	Registry      *catalog.Registry
	// AlwaysFilters is the compiled AND of policy.always_filters.
	AlwaysFilters filter.Expr
	// Shadow is the shadow version, run on ShadowRate of requests and never returned.
	Shadow     *Resolved
	ShadowRate float64
	// Draft marks a playground request against the draft (ConfigVersion 0).
	Draft    bool
	loadedAt time.Time
}

// Source loads tenants, plans and configs (implemented by Store).
type Source interface {
	Tenant(ctx context.Context, id ID) (Tenant, error)
	Plan(ctx context.Context, id string) (Plan, error)
	ActiveConfig(ctx context.Context, id ID) (int, *Config, error)
	ShadowConfig(ctx context.Context, id ID) (int, float64, *Config, error)
	ConfigVersion(ctx context.Context, id ID, v int) (*Config, error)
	Draft(ctx context.Context, id ID) (*Config, error)
	LiveVersions(ctx context.Context) ([]LiveVersion, error)
}

// Resolver caches Resolved per tenant. Activation messages and a reconcile loop invalidate
// entries (docs/ARCHITECTURE.md §8); ttl is a last-resort bound on staleness.
type Resolver struct {
	src Source
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	cache    map[ID]*Resolved
	locks    map[ID]*sync.Mutex
	versions map[versionKey]*Resolved
}

type versionKey struct {
	tenant  ID
	version int
}

// maxPinned bounds the cache of pinned (playground) versions.
const maxPinned = 256

// NewResolver returns a resolver over src.
func NewResolver(src Source, ttl time.Duration) *Resolver {
	return &Resolver{src: src, ttl: ttl, now: time.Now, cache: map[ID]*Resolved{}, locks: map[ID]*sync.Mutex{}, versions: map[versionKey]*Resolved{}}
}

// Get returns the tenant's active config (with its shadow), loading it at most once per tenant
// at a time.
func (r *Resolver) Get(ctx context.Context, id ID) (*Resolved, error) {
	r.mu.Lock()
	if e, ok := r.cache[id]; ok && r.now().Sub(e.loadedAt) < r.ttl {
		r.mu.Unlock()
		return e, nil
	}
	l, ok := r.locks[id]
	if !ok {
		l = &sync.Mutex{}
		r.locks[id] = l
	}
	r.mu.Unlock()

	l.Lock()
	defer l.Unlock()
	r.mu.Lock()
	if e, ok := r.cache[id]; ok && r.now().Sub(e.loadedAt) < r.ttl {
		r.mu.Unlock()
		return e, nil
	}
	r.mu.Unlock()

	e, err := r.load(ctx, id)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[id] = e
	r.mu.Unlock()
	return e, nil
}

// Pinned resolves a specific published version, or the draft for ref "draft" (playground).
// Versions are immutable, so they are cached; drafts are always reloaded.
func (r *Resolver) Pinned(ctx context.Context, id ID, ref string) (*Resolved, error) {
	if ref == "draft" {
		t, plan, err := r.tenantPlan(ctx, id)
		if err != nil {
			return nil, err
		}
		cfg, err := r.src.Draft(ctx, id)
		if err != nil {
			return nil, err
		}
		res, err := compile(t, plan, 0, cfg, r.now())
		if err != nil {
			return nil, err
		}
		res.Draft = true
		return res, nil
	}
	var v int
	if _, err := fmt.Sscanf(ref, "%d", &v); err != nil || v < 1 {
		return nil, fmt.Errorf("config version %q: %w", ref, ErrNotFound)
	}
	k := versionKey{id, v}
	r.mu.Lock()
	if e, ok := r.versions[k]; ok {
		r.mu.Unlock()
		return e, nil
	}
	r.mu.Unlock()
	t, plan, err := r.tenantPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	cfg, err := r.src.ConfigVersion(ctx, id, v)
	if err != nil {
		return nil, err
	}
	res, err := compile(t, plan, v, cfg, r.now())
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	if len(r.versions) >= maxPinned {
		for key := range r.versions {
			delete(r.versions, key)
			break
		}
	}
	r.versions[k] = res
	r.mu.Unlock()
	return res, nil
}

// Invalidate drops a tenant's cached entry.
func (r *Resolver) Invalidate(id ID) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// Cached reports the active and shadow versions currently cached for a tenant.
func (r *Resolver) Cached(id ID) (active, shadow int, rate float64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.cache[id]
	if !ok {
		return 0, 0, 0, false
	}
	if e.Shadow != nil {
		shadow, rate = e.Shadow.ConfigVersion, e.ShadowRate
	}
	return e.ConfigVersion, shadow, rate, true
}

func (r *Resolver) tenantPlan(ctx context.Context, id ID) (Tenant, Plan, error) {
	t, err := r.src.Tenant(ctx, id)
	if err != nil {
		return Tenant{}, Plan{}, err
	}
	if t.Status != "active" {
		return Tenant{}, Plan{}, fmt.Errorf("tenant %s is %s: %w", id, t.Status, ErrNotFound)
	}
	plan, err := r.src.Plan(ctx, t.Plan)
	if err != nil {
		return Tenant{}, Plan{}, err
	}
	return t, plan, nil
}

func (r *Resolver) load(ctx context.Context, id ID) (*Resolved, error) {
	t, plan, err := r.tenantPlan(ctx, id)
	if err != nil {
		return nil, err
	}
	version, cfg, err := r.src.ActiveConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	res, err := compile(t, plan, version, cfg, r.now())
	if err != nil {
		return nil, err
	}
	sv, rate, scfg, err := r.src.ShadowConfig(ctx, id)
	switch {
	case err == nil:
		shadow, err := compile(t, plan, sv, scfg, r.now())
		if err != nil {
			return nil, err
		}
		res.Shadow, res.ShadowRate = shadow, rate
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}
	return res, nil
}

func compile(t Tenant, plan Plan, version int, cfg *Config, now time.Time) (*Resolved, error) {
	reg, err := catalog.NewRegistry(cfg.Catalog.CustomFields)
	if err != nil {
		return nil, fmt.Errorf("tenant %s config v%d: %w", t.ID, version, err)
	}
	always := make([]filter.Expr, 0, len(cfg.Policy.AlwaysFilters))
	for i, f := range cfg.Policy.AlwaysFilters {
		c, errs := filter.Compile(f, reg, fmt.Sprintf("/policy/always_filters/%d", i))
		if len(errs) > 0 {
			return nil, fmt.Errorf("tenant %s config v%d: %s: %s", t.ID, version, errs[0].Path, errs[0].Message)
		}
		always = append(always, c)
	}
	return &Resolved{
		Tenant: t, Plan: plan, ConfigVersion: version, Config: cfg, Registry: reg,
		AlwaysFilters: filter.All(always...), loadedAt: now,
	}, nil
}

// Reconcile compares cached entries with the live versions in MySQL and invalidates the ones
// that drifted (a lost pub/sub message). It returns the invalidated tenants.
func (r *Resolver) Reconcile(ctx context.Context) ([]ID, error) {
	live, err := r.src.LiveVersions(ctx)
	if err != nil {
		return nil, err
	}
	type want struct {
		active, shadow int
		rate           float64
	}
	byTenant := map[ID]*want{}
	for _, lv := range live {
		w, ok := byTenant[lv.Tenant]
		if !ok {
			w = &want{}
			byTenant[lv.Tenant] = w
		}
		if lv.Shadow {
			w.shadow, w.rate = lv.Version, lv.Rate
		} else {
			w.active = lv.Version
		}
	}
	r.mu.Lock()
	var stale []ID
	for id, e := range r.cache {
		w := byTenant[id]
		shadow, rate := 0, 0.0
		if e.Shadow != nil {
			shadow, rate = e.Shadow.ConfigVersion, e.ShadowRate
		}
		if w == nil || w.active != e.ConfigVersion || w.shadow != shadow || w.rate != rate {
			stale = append(stale, id)
			delete(r.cache, id)
		}
	}
	r.mu.Unlock()
	return stale, nil
}

// Watch applies activation messages and runs Reconcile every interval until ctx ends.
// onChange is called for every invalidated tenant (to retire superseded providers).
func (r *Resolver) Watch(ctx context.Context, rdb *redis.Client, interval time.Duration, log *slog.Logger, onChange func(ID)) {
	sub := rdb.Subscribe(ctx, ChannelConfigActivated)
	defer sub.Close()
	msgs := sub.Channel()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case m, ok := <-msgs:
			if !ok {
				return
			}
			var a Activation
			if err := json.Unmarshal([]byte(m.Payload), &a); err != nil || a.TenantID == "" {
				log.Warn("bad activation message", "payload", m.Payload)
				continue
			}
			r.Invalidate(ID(a.TenantID))
			log.Info("config change applied", "tenant", a.TenantID, "version", a.Version, "state", a.State)
			if onChange != nil {
				onChange(ID(a.TenantID))
			}
		case <-t.C:
			stale, err := r.Reconcile(ctx)
			if err != nil {
				log.Warn("config reconcile failed", "err", err)
				continue
			}
			for _, id := range stale {
				log.Info("config reconciled", "tenant", id)
				if onChange != nil {
					onChange(id)
				}
			}
		}
	}
}
