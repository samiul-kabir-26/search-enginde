package registry

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// closingScorer counts Close calls.
type closingScorer struct {
	pipeline.Scorer
	closed *atomic.Int32
}

func (c closingScorer) Close() error { c.closed.Add(1); return nil }

func newTestRegistry(t *testing.T, opts Options) (*Registry, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	r := New(Platform{}, opts, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p, _, _ := stagestest.Providers()
	builds, closed := &atomic.Int32{}, &atomic.Int32{}
	r.Register(KindEmbedder, tenant.ProviderPlatform, func(context.Context, Deps, Spec) (any, error) { return p.TextEmbedder, nil })
	r.Register(KindLexical, tenant.ProviderPlatform, func(context.Context, Deps, Spec) (any, error) { return p.Lexical, nil })
	r.Register(KindVectorStore, tenant.ProviderPlatform, func(context.Context, Deps, Spec) (any, error) { return p.Vectors, nil })
	r.Register(KindCentroids, tenant.ProviderPlatform, func(context.Context, Deps, Spec) (any, error) {
		return &centroidEntry{indexVersion: 4}, nil
	})
	r.Register(KindScorer, tenant.ScorerDefaultLinear, func(context.Context, Deps, Spec) (any, error) {
		builds.Add(1)
		return closingScorer{Scorer: p.Rank, closed: closed}, nil
	})
	return r, builds, closed
}

func resolved(v int) *tenant.Resolved {
	res := stagestest.Resolved()
	res.ConfigVersion = v
	return res
}

func TestRegistryCachesPerVersionAndSupersedes(t *testing.T) {
	r, builds, closed := newTestRegistry(t, Options{Size: 64, DrainGrace: 0})
	ctx := context.Background()
	p, err := r.For(ctx, resolved(1))
	if err != nil {
		t.Fatal(err)
	}
	if p.IndexVersion != 4 || p.Names["rank"] != "default_linear" {
		t.Errorf("providers = %+v", p)
	}
	if _, err := r.For(ctx, resolved(1)); err != nil || builds.Load() != 2 {
		t.Fatalf("second call must hit the cache (prerank + rank built once): builds=%d %v", builds.Load(), err)
	}
	if _, err := r.For(ctx, resolved(2)); err != nil || builds.Load() != 4 {
		t.Fatalf("a new version builds its own providers: %d", builds.Load())
	}

	r.Supersede(stagestest.TenantID, 2)
	r.closeDue(time.Now().Add(time.Second))
	if closed.Load() != 2 {
		t.Errorf("superseded v1 scorers must close after the drain grace: closed=%d", closed.Load())
	}
	if _, err := r.For(ctx, resolved(2)); err != nil || builds.Load() != 4 {
		t.Errorf("the kept version stays cached: builds=%d", builds.Load())
	}
}

func TestRegistryDrainGraceAndEviction(t *testing.T) {
	r, _, closed := newTestRegistry(t, Options{Size: 7, DrainGrace: time.Hour})
	ctx := context.Background()
	if _, err := r.For(ctx, resolved(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.For(ctx, resolved(2)); err != nil {
		t.Fatal(err)
	}
	r.closeDue(time.Now())
	if closed.Load() != 0 {
		t.Error("evicted providers must not close before the drain grace")
	}
	r.closeDue(time.Now().Add(2 * time.Hour))
	if closed.Load() == 0 {
		t.Error("LRU eviction must close providers after the grace")
	}
}

func TestRegistryUnknownTypeAndRefresh(t *testing.T) {
	r, _, _ := newTestRegistry(t, Options{Size: 64, Refresh: time.Nanosecond})
	ctx := context.Background()
	res := resolved(1)
	res.Config.Rank.Scorer.Type = "onnx"
	if _, err := r.For(ctx, res); err == nil || !strings.Contains(err.Error(), "onnx") {
		t.Errorf("unregistered type must fail clearly: %v", err)
	}
	var centroidBuilds atomic.Int32
	r.Register(KindCentroids, tenant.ProviderPlatform, func(context.Context, Deps, Spec) (any, error) {
		centroidBuilds.Add(1)
		return &centroidEntry{indexVersion: int(centroidBuilds.Load())}, nil
	})
	p1, _ := r.For(ctx, resolved(3))
	time.Sleep(time.Millisecond)
	p2, _ := r.For(ctx, resolved(3))
	if p2.IndexVersion <= p1.IndexVersion {
		t.Errorf("time-sensitive entries must refresh: %d then %d", p1.IndexVersion, p2.IndexVersion)
	}
	if info, ok := PlatformEmbedderInfo("clip-vit-b32"); !ok || !info.TextToImage || info.Dim != 512 {
		t.Errorf("clip info = %+v", info)
	}
}
