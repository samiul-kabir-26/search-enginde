package tenant

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestDiff(t *testing.T) {
	a := json.RawMessage(`{"budgets":{"total_ms":300,"s1_ms":40},"facets":[1,2],"locale":"en"}`)
	b := json.RawMessage(`{"budgets":{"total_ms":600},"facets":[1,3],"query":{"attributes":{}}}`)
	got, err := Diff(a, b)
	if err != nil {
		t.Fatal(err)
	}
	want := []Change{
		{Path: "/budgets/s1_ms", Op: "remove", From: 40.0},
		{Path: "/budgets/total_ms", Op: "replace", From: 300.0, To: 600.0},
		{Path: "/facets", Op: "replace", From: []any{1.0, 2.0}, To: []any{1.0, 3.0}},
		{Path: "/locale", Op: "remove", From: "en"},
		{Path: "/query", Op: "add", To: map[string]any{"attributes": map[string]any{}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Diff =\n%+v\nwant\n%+v", got, want)
	}
	if same, _ := Diff(a, a); len(same) != 0 {
		t.Errorf("identical documents differ: %+v", same)
	}
	if _, err := Diff(json.RawMessage(`{`), b); err == nil {
		t.Error("bad from document")
	}
	if _, err := Diff(a, json.RawMessage(`[`)); err == nil {
		t.Error("bad to document")
	}
}

// lockedSource makes fakeSource safe for the watcher goroutine.
type lockedSource struct {
	mu sync.Mutex
	f  *fakeSource
}

func (l *lockedSource) Tenant(ctx context.Context, id ID) (Tenant, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Tenant(ctx, id)
}
func (l *lockedSource) Plan(ctx context.Context, id string) (Plan, error) { return l.f.Plan(ctx, id) }
func (l *lockedSource) ActiveConfig(ctx context.Context, id ID) (int, *Config, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.ActiveConfig(ctx, id)
}
func (l *lockedSource) ShadowConfig(ctx context.Context, id ID) (int, float64, *Config, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.ShadowConfig(ctx, id)
}
func (l *lockedSource) ConfigVersion(ctx context.Context, id ID, v int) (*Config, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.ConfigVersion(ctx, id, v)
}
func (l *lockedSource) Draft(ctx context.Context, id ID) (*Config, error) { return l.f.Draft(ctx, id) }
func (l *lockedSource) LiveVersions(ctx context.Context) ([]LiveVersion, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.LiveVersions(ctx)
}

func (l *lockedSource) set(fn func(f *fakeSource) int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fn(l.f)
}

func TestWatchAppliesActivationsAndReconciles(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rdb.Close()
	src := &lockedSource{f: &fakeSource{tenant: Tenant{ID: "T", Plan: "growth", Status: "active"}, version: 1}}
	loads := func() int { return src.set(func(f *fakeSource) int { return f.loads }) }
	r := NewResolver(src, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := r.Get(ctx, "T"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var changed []ID
	done := make(chan struct{})
	go func() {
		r.Watch(ctx, rdb, 50*time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)), func(id ID) {
			mu.Lock()
			changed = append(changed, id)
			mu.Unlock()
		})
		close(done)
	}()
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for range 200 {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	// Publish until the subscription is live (pub/sub drops messages without subscribers).
	waitFor("the activation", func() bool {
		_ = rdb.Publish(ctx, ChannelConfigActivated, "not json").Err()
		if err := PublishActivation(ctx, rdb, Activation{TenantID: "T", Version: 2, State: "active"}); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		return len(changed) > 0
	})
	if _, err := r.Get(ctx, "T"); err != nil || loads() < 2 {
		t.Errorf("an activation must invalidate the cached tenant (loads %d, %v)", loads(), err)
	}

	// A version that changes without a message is found by the reconcile loop.
	mu.Lock()
	changed = nil
	mu.Unlock()
	if _, err := r.Get(ctx, "T"); err != nil {
		t.Fatal(err)
	}
	src.set(func(f *fakeSource) int { f.version = 3; return 0 })
	waitFor("the reconcile", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(changed) > 0
	})
	if res, _ := r.Get(ctx, "T"); res.ConfigVersion != 3 {
		t.Errorf("reconciled version = %d", res.ConfigVersion)
	}
	cancel()
	<-done
}
