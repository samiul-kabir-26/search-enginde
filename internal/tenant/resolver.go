package tenant

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
)

// Resolved is everything the data plane needs about a tenant for one request: its active
// config version, compiled forms derived from it, and its plan.
type Resolved struct {
	Tenant        Tenant
	Plan          Plan
	ConfigVersion int
	Config        *Config
	Registry      *catalog.Registry
	// AlwaysFilters is the compiled AND of policy.always_filters.
	AlwaysFilters filter.Expr
	loadedAt      time.Time
}

// Source loads tenants, plans and active configs (implemented by Store).
type Source interface {
	Tenant(ctx context.Context, id ID) (Tenant, error)
	Plan(ctx context.Context, id string) (Plan, error)
	ActiveConfig(ctx context.Context, id ID) (int, *Config, error)
}

// Resolver caches Resolved per tenant. Entries expire after ttl, which bounds staleness until
// config-activation pub/sub lands in M4; Invalidate drops one immediately.
type Resolver struct {
	src Source
	ttl time.Duration
	now func() time.Time

	mu    sync.Mutex
	cache map[ID]*Resolved
	locks map[ID]*sync.Mutex
}

// NewResolver returns a resolver over src.
func NewResolver(src Source, ttl time.Duration) *Resolver {
	return &Resolver{src: src, ttl: ttl, now: time.Now, cache: map[ID]*Resolved{}, locks: map[ID]*sync.Mutex{}}
}

// Get returns the tenant's resolved config, loading it at most once per tenant at a time.
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

// Invalidate drops a tenant's cached entry.
func (r *Resolver) Invalidate(id ID) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

func (r *Resolver) load(ctx context.Context, id ID) (*Resolved, error) {
	t, err := r.src.Tenant(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.Status != "active" {
		return nil, fmt.Errorf("tenant %s is %s: %w", id, t.Status, ErrNotFound)
	}
	plan, err := r.src.Plan(ctx, t.Plan)
	if err != nil {
		return nil, err
	}
	version, cfg, err := r.src.ActiveConfig(ctx, id)
	if err != nil {
		return nil, err
	}
	reg, err := catalog.NewRegistry(cfg.Catalog.CustomFields)
	if err != nil {
		return nil, fmt.Errorf("tenant %s config v%d: %w", id, version, err)
	}
	always := make([]filter.Expr, 0, len(cfg.Policy.AlwaysFilters))
	for i, f := range cfg.Policy.AlwaysFilters {
		c, errs := filter.Compile(f, reg, fmt.Sprintf("/policy/always_filters/%d", i))
		if len(errs) > 0 {
			return nil, fmt.Errorf("tenant %s config v%d: %s: %s", id, version, errs[0].Path, errs[0].Message)
		}
		always = append(always, c)
	}
	return &Resolved{
		Tenant: t, Plan: plan, ConfigVersion: version, Config: cfg, Registry: reg,
		AlwaysFilters: filter.All(always...), loadedAt: r.now(),
	}, nil
}
