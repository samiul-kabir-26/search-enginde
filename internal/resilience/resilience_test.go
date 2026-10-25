package resilience

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sony/gobreaker/v2"
	"go.opentelemetry.io/otel/metric/noop"
)

func newBreakers(t *testing.T) *Breakers {
	t.Helper()
	s := DefaultSettings()
	s.ConsecutiveFailures = 3
	s.OpenTimeout = time.Hour
	b, err := NewBreakers(noop.NewMeterProvider().Meter("test"), s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var key = Key{Tenant: "T", Stage: "rank", Provider: "kserve"}

func ok(context.Context) (string, error)   { return "primary", nil }
func fail(context.Context) (string, error) { return "", errors.New("boom") }
func fb(context.Context) (string, error)   { return "fallback", nil }

func TestCallPrimarySucceeds(t *testing.T) {
	v, out, err := Call(context.Background(), newBreakers(t), key, ok, fb)
	if err != nil || v != "primary" || out.Fallback {
		t.Fatalf("got %q %+v %v", v, out, err)
	}
}

func TestCallFallsBackOnError(t *testing.T) {
	v, out, err := Call(context.Background(), newBreakers(t), key, fail, fb)
	if err != nil || v != "fallback" || !out.Fallback || out.Reason != ReasonError {
		t.Fatalf("got %q %+v %v", v, out, err)
	}
}

func TestCallWithoutFallbackReturnsError(t *testing.T) {
	_, out, err := Call[string](context.Background(), newBreakers(t), key, fail, nil)
	if err == nil || !out.Fallback {
		t.Fatalf("want error with fallback outcome, got %+v %v", out, err)
	}
}

func TestCallTimeoutLeavesBudgetForFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	hang := func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	start := time.Now()
	v, out, err := Call(ctx, newBreakers(t), key, hang, fb)
	if err != nil || v != "fallback" || out.Reason != ReasonTimeout {
		t.Fatalf("got %q %+v %v", v, out, err)
	}
	if el := time.Since(start); el > 180*time.Millisecond {
		t.Errorf("primary used %v of a 200ms budget; the fallback reserve was not kept", el)
	}
}

func TestBreakerOpensAndShortCircuits(t *testing.T) {
	b := newBreakers(t)
	for range 3 {
		_, _, _ = Call(context.Background(), b, key, fail, fb)
	}
	if b.State(key) != gobreaker.StateOpen {
		t.Fatalf("state = %v, want open", b.State(key))
	}
	called := false
	primary := func(context.Context) (string, error) { called = true; return "x", nil }
	v, out, err := Call(context.Background(), b, key, primary, fb)
	if called || v != "fallback" || out.Reason != ReasonBreakerOpen || err != nil {
		t.Fatalf("open breaker must skip the primary: called=%v %q %+v %v", called, v, out, err)
	}
	other := Key{Tenant: "U", Stage: "rank", Provider: "kserve"}
	if b.State(other) != gobreaker.StateClosed {
		t.Error("breakers must be isolated per tenant")
	}
}

func TestCanceledCallerDoesNotTrip(t *testing.T) {
	b := newBreakers(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 5 {
		_, _, err := Call(ctx, b, key, func(ctx context.Context) (string, error) { return "", ctx.Err() }, fb)
		if err == nil {
			t.Fatal("canceled caller must get an error")
		}
	}
	if b.State(key) != gobreaker.StateClosed {
		t.Error("client cancellations must not open the breaker")
	}
}

func TestBulkheadRefusesWhenFull(t *testing.T) {
	b := NewBulkheads(2)
	r1, err1 := b.Acquire("a")
	r2, err2 := b.Acquire("a")
	if err1 != nil || err2 != nil {
		t.Fatal(err1, err2)
	}
	if _, err := b.Acquire("a"); !errors.Is(err, ErrBulkheadFull) {
		t.Fatalf("third call: %v", err)
	}
	if r, err := b.Acquire("b"); err != nil {
		t.Fatalf("other tenants are unaffected: %v", err)
	} else {
		r()
	}
	r1()
	if b.InFlight("a") != 1 {
		t.Errorf("in flight = %d", b.InFlight("a"))
	}
	r3, err := b.Acquire("a")
	if err != nil {
		t.Fatal(err)
	}
	r2()
	r3()

	br := newBreakers(t)
	_, out, err := Call(context.Background(), br, Key{"t", "s", "p"}, func(context.Context) (int, error) { return 0, ErrBulkheadFull }, nil)
	if err == nil || out.Reason != ReasonBulkhead {
		t.Errorf("reason = %q, err = %v", out.Reason, err)
	}
}
