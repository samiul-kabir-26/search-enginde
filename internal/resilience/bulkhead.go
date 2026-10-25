package resilience

import (
	"errors"
	"sync"
)

// ErrBulkheadFull means the tenant already has its maximum number of BYO calls in flight.
var ErrBulkheadFull = errors.New("tenant bulkhead full")

// ReasonBulkhead is the fallback reason when a call was refused by the tenant's bulkhead.
const ReasonBulkhead = "bulkhead_full"

// Bulkheads bound each tenant's concurrent calls to its own (BYO) endpoints, so one slow
// tenant endpoint holds at most Size slots and cannot exhaust shared workers or connection
// pools (docs/ARCHITECTURE.md §7). A full bulkhead refuses at once rather than queueing, so
// the caller's fallback runs with the budget it has left.
type Bulkheads struct {
	size int

	mu  sync.Mutex
	sem map[string]chan struct{}
}

// NewBulkheads returns bulkheads of size slots per tenant.
func NewBulkheads(size int) *Bulkheads {
	if size <= 0 {
		size = 32
	}
	return &Bulkheads{size: size, sem: map[string]chan struct{}{}}
}

func (b *Bulkheads) of(tenant string) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.sem[tenant]
	if !ok {
		s = make(chan struct{}, b.size)
		b.sem[tenant] = s
	}
	return s
}

// Acquire takes one of the tenant's slots, returning the release func, or ErrBulkheadFull.
func (b *Bulkheads) Acquire(tenant string) (func(), error) {
	s := b.of(tenant)
	select {
	case s <- struct{}{}:
		return func() { <-s }, nil
	default:
		return nil, ErrBulkheadFull
	}
}

// InFlight reports a tenant's calls in flight, for tests and metrics.
func (b *Bulkheads) InFlight(tenant string) int { return len(b.of(tenant)) }
