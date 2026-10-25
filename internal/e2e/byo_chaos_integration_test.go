//go:build integration

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	qdrantgo "github.com/qdrant/go-client/qdrant"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/storetest"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/stages/filter"
	"github.com/zavisoft/search-engine/internal/stages/prerank"
	"github.com/zavisoft/search-engine/internal/stages/queryunderstanding"
	"github.com/zavisoft/search-engine/internal/stages/rank"
	"github.com/zavisoft/search-engine/internal/stages/rerank"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/testinfra"
	"github.com/zavisoft/search-engine/internal/testinfra/faultproxy"
)

// Embedder modes of the fake BYO embeddings endpoint.
const (
	embedOK int32 = iota
	embedFail
	embedHang
	embedSlow
)

// chaosEmbeddings is a tenant's OpenAI-compatible endpoint returning the fake catalog
// embeddings, or failing, hanging or answering slowly.
func chaosEmbeddings(t *testing.T, mode *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case embedFail:
			http.Error(w, "upstream exploded", http.StatusInternalServerError)
			return
		case embedHang:
			// The server notices the client giving up only once the body has been read.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		case embedSlow:
			time.Sleep(250 * time.Millisecond)
		}
		var req struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i, s := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": stagestest.Embed(s, stagestest.Dim)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

type chaosSource struct {
	model tenant.Model
	conn  tenant.Connection
}

func (s chaosSource) Model(_ context.Context, _ tenant.ID, id string) (tenant.Model, error) {
	if id != s.model.ID {
		return tenant.Model{}, tenant.ErrNotFound
	}
	return s.model, nil
}

func (s chaosSource) Connection(_ context.Context, _ tenant.ID, id string) (tenant.Connection, error) {
	if id != s.conn.ID {
		return tenant.Connection{}, tenant.ErrNotFound
	}
	return s.conn, nil
}

// productionBudgets are the default config's budgets (total 300 ms, S1 40 ms, S2 120 ms).
func productionBudgets(t *testing.T) tenant.Budgets {
	t.Helper()
	cfg, err := tenant.ParseConfig(tenant.DefaultDocument())
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Budgets
}

// slack absorbs scheduling on a loaded machine; budgets themselves are enforced by deadlines.
const slack = 25 * time.Millisecond

func p95(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)*95/100]
}

// TestBYOChaos is the M5 acceptance test: a BYO embedder or vector store that fails, hangs or
// is slow falls back to the platform providers within each stage's budget, the request still
// returns results, and another tenant's latency is unaffected.
func TestBYOChaos(t *testing.T) {
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Tenant A's own Qdrant, reached through a fault-injecting proxy.
	qaddr := testinfra.Qdrant(t)
	admin, err := qdrant.NewClient(qaddr)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	vp := map[string]*qdrantgo.VectorParams{"text": {Size: stagestest.Dim, Distance: qdrantgo.Distance_Cosine}, "image": {Size: stagestest.Dim, Distance: qdrantgo.Distance_Cosine}}
	if err := admin.CreateCollection(ctx, &qdrantgo.CreateCollection{CollectionName: "a_products", VectorsConfig: qdrantgo.NewVectorsConfigMap(vp)}); err != nil {
		t.Fatal(err)
	}
	proxy, err := faultproxy.Start(qaddr)
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var embedMode atomic.Int32
	emb := chaosEmbeddings(t, &embedMode)

	tenantA := tenant.ID("01JCHAOSTENANTA0000000000A")
	block := tenant.StoreConfig{Provider: tenant.ProviderBYO, ConnectionID: "conn-a", Collection: "a_products",
		Vectors: map[string]tenant.VectorSpec{"text": {Dim: stagestest.Dim, Metric: "cosine"}, "image": {Dim: stagestest.Dim, Metric: "cosine"}}}
	src := chaosSource{
		model: tenant.Model{ID: "model-a", Kind: "embedder", Type: tenant.ModelOpenAIEmbeddings, Name: "emb", Endpoint: emb.URL, Status: tenant.StatusValidated,
			Input: tenant.ModelInput{Modality: tenant.ModalityText}, Output: tenant.ModelOutput{Dim: stagestest.Dim}},
		conn: tenant.Connection{ID: "conn-a", Kind: tenant.ConnectionVectorStore, Type: tenant.StoreQdrant, Endpoint: "http://" + proxy.Addr(), Status: tenant.StatusValidated},
	}
	bulkheads := resilience.NewBulkheads(8)
	byo := &registry.BYO{Source: src, Secrets: storetest.Secrets{}, Bulkheads: bulkheads, Timeout: 5 * time.Second,
		Egress: security.NewEgress(security.EgressConfig{Trusted: []string{proxy.Addr(), strings.TrimPrefix(emb.URL, "http://"), qaddr}})}

	// Seed tenant A's collection directly (not through the proxy).
	seedConn := src.conn
	seedConn.Endpoint = "http://" + qaddr
	seed, err := registry.NewBYOVectorStore(ctx, byo, tenantA, tenant.Plan{}, seedConn, block)
	if err != nil {
		t.Fatal(err)
	}
	var points []pipeline.VectorPoint
	for _, p := range stagestest.Catalog() {
		v := stagestest.Embed(p.Title, stagestest.Dim)
		points = append(points, pipeline.VectorPoint{ProductID: p.ID, Vectors: map[string][]float32{"text": v, "image": v}, Payload: map[string]any{
			"status": p.Status, "stock": p.Stock, "price_minor": p.PriceMinor, "brand": p.Brand, "category_ids": p.Categories,
			"vendor_id": "v_" + p.Brand, "rating": p.Rating, "rating_count": p.RatingCount,
		}})
	}
	if err := seed.Upsert(ctx, points); err != nil {
		t.Fatal(err)
	}
	_ = seed.Close()

	// The registry builds tenant A's BYO providers through egress, secrets and bulkheads; the
	// platform providers (lexical index, platform embedders and vectors) are the in-memory fakes.
	reg := registry.New(registry.Platform{}, registry.Options{Size: 64, BYO: byo}, log)
	fakes, _, _ := stagestest.Providers()
	reg.Register(registry.KindEmbedder, tenant.ProviderPlatform, func(context.Context, registry.Deps, registry.Spec) (any, error) { return fakes.TextEmbedder, nil })
	reg.Register(registry.KindLexical, tenant.ProviderPlatform, func(context.Context, registry.Deps, registry.Spec) (any, error) { return fakes.Lexical, nil })
	reg.Register(registry.KindVectorStore, tenant.ProviderPlatform, func(context.Context, registry.Deps, registry.Spec) (any, error) { return fakes.Vectors, nil })
	reg.Register(registry.KindCentroids, tenant.ProviderPlatform, func(context.Context, registry.Deps, registry.Spec) (any, error) {
		return nil, errors.New("no centroids in this test")
	})

	budgets := productionBudgets(t)
	resA := stagestest.Resolved()
	resA.Tenant.ID, resA.Config.Budgets = tenantA, budgets
	resA.Config.Embedders.Text = tenant.EmbedderConfig{Provider: tenant.ProviderBYO, ModelID: "model-a"}
	resA.Config.Stores.Vector = block
	resB := stagestest.Resolved()
	resB.Config.Budgets = budgets

	newEngine := func() *pipeline.Engine {
		b, err := resilience.NewBreakers(noop.NewMeterProvider().Meter("chaos"), resilience.DefaultSettings())
		if err != nil {
			t.Fatal(err)
		}
		e, err := pipeline.NewEngine(noop.NewMeterProvider().Meter("chaos"),
			queryunderstanding.New(b), retrieval.New(b), filter.New(), prerank.New(b), rank.New(b), rerank.New())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	type result struct {
		st      *pipeline.State
		tr      *pipeline.Trace
		elapsed time.Duration
	}
	search := func(e *pipeline.Engine, res *tenant.Resolved, text string) result {
		p, err := reg.For(ctx, res)
		if err != nil {
			t.Fatalf("providers: %v", err)
		}
		q := &pipeline.Query{Tenant: res.Tenant.ID, ConfigVersion: res.ConfigVersion, Type: pipeline.QueryText, Raw: text,
			BaseFilter: res.AlwaysFilters, Depth: 100}
		start := time.Now()
		st, tr := e.Run(ctx, res, p, q)
		return result{st: st, tr: tr, elapsed: time.Since(start)}
	}
	stage := func(tr *pipeline.Trace, name string) pipeline.StageTrace {
		for _, s := range tr.Stages {
			if s.Stage == name {
				return s
			}
		}
		t.Fatalf("no %s stage in trace", name)
		return pipeline.StageTrace{}
	}
	within := func(scenario string, r result) {
		t.Helper()
		if len(r.st.Candidates) == 0 || r.st.Degraded {
			t.Errorf("%s: the platform providers must carry the request: %d candidates, degraded=%v (trace %+v)", scenario, len(r.st.Candidates), r.st.Degraded, r.tr)
		}
		for _, s := range []struct {
			name   string
			budget int
		}{{pipeline.StageQueryUnderstanding, budgets.S1MS}, {pipeline.StageRetrieval, budgets.S2MS}} {
			got := stage(r.tr, s.name)
			if limit := time.Duration(s.budget)*time.Millisecond + slack; time.Duration(got.LatencyMS*float64(time.Millisecond)) > limit {
				t.Errorf("%s: %s took %.1f ms, over its %d ms budget", scenario, s.name, got.LatencyMS, s.budget)
			}
		}
		if limit := time.Duration(budgets.TotalMS)*time.Millisecond + slack; r.elapsed > limit {
			t.Errorf("%s: request took %v, over the %d ms total budget", scenario, r.elapsed, budgets.TotalMS)
		}
	}

	t.Run("healthy BYO providers serve vector retrieval", func(t *testing.T) {
		e := newEngine()
		r := search(e, resA, "red leather handbag")
		s1, s2 := stage(r.tr, pipeline.StageQueryUnderstanding), stage(r.tr, pipeline.StageRetrieval)
		if s1.Fallback || s2.Fallback || s1.Provider != "byo:openai_embeddings:emb" {
			t.Fatalf("healthy run fell back: s1=%+v s2=%+v", s1, s2)
		}
		if _, ok := r.st.Candidates[0].Retrievers[tenant.RetrieverVectorText]; !ok || r.st.Candidates[0].ProductID != "p01" {
			t.Errorf("top candidate %+v must come from the BYO store too", r.st.Candidates[0])
		}
		t.Logf("healthy: %v", r.elapsed)
	})

	scenarios := []struct {
		name        string
		set         func()
		reason      string
		stageWanted string
	}{
		{"embedder fails", func() { embedMode.Store(embedFail) }, "text_embedder:error", pipeline.StageQueryUnderstanding},
		{"embedder hangs", func() { embedMode.Store(embedHang) }, "text_embedder:timeout", pipeline.StageQueryUnderstanding},
		{"embedder slow", func() { embedMode.Store(embedSlow) }, "text_embedder:timeout", pipeline.StageQueryUnderstanding},
		{"store refuses", func() { proxy.Set(faultproxy.Refuse, 0) }, "vector_text:", pipeline.StageRetrieval},
		{"store hangs", func() { proxy.Set(faultproxy.Blackhole, 0) }, "vector_text:timeout", pipeline.StageRetrieval},
		{"store slow", func() { proxy.Set(faultproxy.Delay, 250*time.Millisecond) }, "vector_text:timeout", pipeline.StageRetrieval},
	}
	for i, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			embedMode.Store(embedOK)
			proxy.Set(faultproxy.Pass, 0)
			sc.set()
			defer func() { embedMode.Store(embedOK); proxy.Set(faultproxy.Pass, 0) }()
			// A new config version gets new providers (fresh connections), so scenarios do not
			// inherit each other's connection state.
			res := *resA
			res.ConfigVersion = 100 + i
			resA := &res
			e := newEngine()
			var latencies []time.Duration
			breakerOpened := false
			for i := range 14 {
				r := search(e, resA, "red leather handbag")
				within(sc.name, r)
				latencies = append(latencies, r.elapsed)
				st := stage(r.tr, sc.stageWanted)
				open := strings.Contains(st.FallbackReason, resilience.ReasonBreakerOpen)
				if !st.Fallback || (!strings.Contains(st.FallbackReason, sc.reason) && !open) {
					t.Errorf("%s #%d: %s trace = %+v", sc.name, i, sc.stageWanted, st)
				}
				if i == 0 && open {
					t.Errorf("%s: the first failure is the endpoint's, not the breaker's: %s", sc.name, st.FallbackReason)
				}
				breakerOpened = breakerOpened || open
			}
			if !breakerOpened {
				t.Errorf("%s: after 10 consecutive failures the breaker must open", sc.name)
			}
			t.Logf("%s: p95 %v, max %v", sc.name, p95(latencies), slices.Max(latencies))
		})
	}

	t.Run("semantic-only config falls back to the lexical index", func(t *testing.T) {
		proxy.Set(faultproxy.Blackhole, 0)
		defer proxy.Set(faultproxy.Pass, 0)
		res := *resA
		cfg := *resA.Config
		cfg.Retrieval.Text.Retrievers = []tenant.RetrieverConfig{{Name: tenant.RetrieverVectorText, K: 50, Weight: 1}}
		res.Config, res.ConfigVersion = &cfg, 200
		r := search(newEngine(), &res, "red leather handbag")
		within("semantic-only", r)
		if s2 := stage(r.tr, pipeline.StageRetrieval); !strings.Contains(s2.FallbackReason, "vector_text:timeout>lexical_bm25") {
			t.Errorf("S2 = %+v", s2)
		}
	})

	t.Run("tenant isolation", func(t *testing.T) {
		e := newEngine()
		measureB := func(n int) []time.Duration {
			out := make([]time.Duration, n)
			for i := range out {
				r := search(e, resB, "red leather handbag")
				if len(r.st.Candidates) == 0 {
					t.Fatal("tenant B returned nothing")
				}
				out[i] = r.elapsed
			}
			return out
		}
		// underLoad measures tenant B while tenant A sends steady traffic (8 clients, one request
		// every 10 ms each), with A's endpoints healthy or hung. Comparing the two isolates the
		// effect of A's failing providers from the CPU cost of A's traffic itself.
		underLoad := func(hung bool) ([]time.Duration, int64, int64) {
			mode, proxyMode := embedOK, faultproxy.Pass
			if hung {
				mode, proxyMode = embedHang, faultproxy.Blackhole
			}
			embedMode.Store(mode)
			proxy.Set(proxyMode, 0)
			defer func() { embedMode.Store(embedOK); proxy.Set(faultproxy.Pass, 0) }()
			stop := make(chan struct{})
			var wg sync.WaitGroup
			var aRequests, peak atomic.Int64
			for range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					tick := time.NewTicker(10 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-stop:
							return
						case <-tick.C:
						}
						search(e, resA, "red leather handbag")
						aRequests.Add(1)
					}
				}()
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					case <-time.After(time.Millisecond):
						if n := int64(bulkheads.InFlight(string(tenantA))); n > peak.Load() {
							peak.Store(n)
						}
					}
				}
			}()
			time.Sleep(300 * time.Millisecond)
			b := measureB(300)
			close(stop)
			wg.Wait()
			return b, aRequests.Load(), peak.Load()
		}
		measureB(50)
		healthy, healthyA, _ := underLoad(false)
		hung, hungA, peak := underLoad(true)
		hp, dp := p95(healthy), p95(hung)
		t.Logf("tenant B p95 with tenant A's traffic: A healthy %v (%d A requests), A's endpoints hung %v (%d A requests, peak %d BYO calls in flight)",
			hp, healthyA, dp, hungA, peak)
		if margin := max(5*time.Millisecond, hp/2); dp > hp+margin {
			t.Errorf("tenant B's p95 moved from %v to %v when tenant A's endpoints hung", hp, dp)
		}
		if peak > 8 {
			t.Errorf("tenant A's BYO calls in flight peaked at %d, over its bulkhead of 8", peak)
		}
	})
}
