package resilience

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Key identifies one breaker: a provider used by a stage for a tenant.
type Key struct {
	Tenant   string
	Stage    string
	Provider string
}

func (k Key) String() string { return k.Tenant + "/" + k.Stage + "/" + k.Provider }

// Fallback reasons, recorded in the trace and in search_stage_fallback_total.
const (
	ReasonError       = "error"
	ReasonTimeout     = "timeout"
	ReasonBreakerOpen = "breaker_open"
)

// Outcome says whether the fallback was used, and why.
type Outcome struct {
	Fallback bool
	Reason   string
}

// Settings tune the breakers.
type Settings struct {
	// FailureRatio and MinRequests trip a breaker within one Interval.
	FailureRatio float64
	MinRequests  uint32
	// ConsecutiveFailures trips a breaker regardless of volume.
	ConsecutiveFailures uint32
	Interval            time.Duration
	// OpenTimeout is how long a tripped breaker stays open before probing.
	OpenTimeout time.Duration
	// FallbackReserve is the share of the remaining budget kept back for the fallback.
	FallbackReserve float64
}

// DefaultSettings are the breaker defaults.
func DefaultSettings() Settings {
	return Settings{
		FailureRatio: 0.5, MinRequests: 20, ConsecutiveFailures: 10,
		Interval: 10 * time.Second, OpenTimeout: 3 * time.Second, FallbackReserve: 0.25,
	}
}

// Breakers holds one circuit breaker per Key and the resilience metrics.
type Breakers struct {
	settings  Settings
	fallbacks metric.Int64Counter

	mu sync.Mutex
	m  map[Key]*gobreaker.CircuitBreaker[any]
}

// NewBreakers registers search_stage_fallback_total and provider_breaker_state on meter.
func NewBreakers(meter metric.Meter, s Settings) (*Breakers, error) {
	b := &Breakers{settings: s, m: map[Key]*gobreaker.CircuitBreaker[any]{}}
	var err error
	b.fallbacks, err = meter.Int64Counter("search_stage_fallback_total",
		metric.WithDescription("Stage provider calls that used the fallback or passed through, by reason."))
	if err != nil {
		return nil, fmt.Errorf("fallback counter: %w", err)
	}
	_, err = meter.Int64ObservableGauge("provider_breaker_state",
		metric.WithDescription("Circuit breaker state per tenant, stage and provider: 0 closed, 1 half-open, 2 open."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			b.mu.Lock()
			defer b.mu.Unlock()
			for k, cb := range b.m {
				o.Observe(int64(cb.State()), metric.WithAttributes(
					attribute.String("tenant", k.Tenant), attribute.String("stage", k.Stage), attribute.String("provider", k.Provider)))
			}
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("breaker gauge: %w", err)
	}
	return b, nil
}

func (b *Breakers) breaker(k Key) *gobreaker.CircuitBreaker[any] {
	b.mu.Lock()
	defer b.mu.Unlock()
	if cb, ok := b.m[k]; ok {
		return cb
	}
	s := b.settings
	cb := gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
		Name:        k.String(),
		MaxRequests: 1,
		Interval:    s.Interval,
		Timeout:     s.OpenTimeout,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			if c.ConsecutiveFailures >= s.ConsecutiveFailures {
				return true
			}
			return c.Requests >= s.MinRequests && float64(c.TotalFailures)/float64(c.Requests) >= s.FailureRatio
		},
		// A caller giving up (client disconnect) says nothing about the provider's health.
		IsExcluded: func(err error) bool { return errors.Is(err, context.Canceled) },
	})
	b.m[k] = cb
	return cb
}

// State returns a breaker's state for traces and tests.
func (b *Breakers) State(k Key) gobreaker.State { return b.breaker(k).State() }

// Call runs primary through k's breaker. When the primary fails, times out or its breaker is
// open, fallback runs with what remains of ctx's deadline; a nil fallback means the caller
// passes its input through (stages 4–6) or contributes nothing (one S2 retriever). When a
// fallback exists the primary's deadline keeps FallbackReserve of the budget for it.
func Call[T any](ctx context.Context, b *Breakers, k Key, primary, fallback func(context.Context) (T, error)) (T, Outcome, error) {
	var zero T
	pctx, cancel := ctx, context.CancelFunc(func() {})
	if dl, ok := ctx.Deadline(); ok && fallback != nil {
		remaining := time.Until(dl)
		pctx, cancel = context.WithTimeout(ctx, time.Duration(float64(remaining)*(1-b.settings.FallbackReserve)))
	}
	res, err := b.breaker(k).Execute(func() (any, error) { return primary(pctx) })
	cancel()
	if err == nil {
		v, _ := res.(T)
		return v, Outcome{}, nil
	}
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return zero, Outcome{}, fmt.Errorf("%s: %w", k, ctx.Err())
	}
	reason := ReasonError
	switch {
	case errors.Is(err, gobreaker.ErrOpenState), errors.Is(err, gobreaker.ErrTooManyRequests):
		reason = ReasonBreakerOpen
	case errors.Is(err, context.DeadlineExceeded) || pctx.Err() != nil:
		reason = ReasonTimeout
	}
	b.fallbacks.Add(ctx, 1, metric.WithAttributes(attribute.String("tenant", k.Tenant), attribute.String("stage", k.Stage),
		attribute.String("provider", k.Provider), attribute.String("reason", reason)))
	out := Outcome{Fallback: true, Reason: reason}
	if fallback == nil || ctx.Err() != nil {
		return zero, out, fmt.Errorf("%s: %w", k, err)
	}
	v, ferr := fallback(ctx)
	if ferr != nil {
		return zero, out, fmt.Errorf("%s: primary: %w; fallback: %w", k, err, ferr)
	}
	return v, out, nil
}
