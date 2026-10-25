package registry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
)

type fakeSource struct {
	models map[string]tenant.Model
	conns  map[string]tenant.Connection
}

func (f fakeSource) Model(_ context.Context, _ tenant.ID, id string) (tenant.Model, error) {
	m, ok := f.models[id]
	if !ok {
		return m, tenant.ErrNotFound
	}
	return m, nil
}

func (f fakeSource) Connection(_ context.Context, _ tenant.ID, id string) (tenant.Connection, error) {
	c, ok := f.conns[id]
	if !ok {
		return c, tenant.ErrNotFound
	}
	return c, nil
}

type memSecrets map[string]string

func (m memSecrets) Put(context.Context, string, string, string) (string, error) { return "", nil }
func (m memSecrets) Get(_ context.Context, _, ref string) (security.Secret, error) {
	v, ok := m[ref]
	if !ok {
		return security.Secret{}, security.ErrSecretNotFound
	}
	return security.NewSecret(v), nil
}

// slowEmbeddings serves /v1/embeddings after delay, counting calls in flight.
func slowEmbeddings(t *testing.T, delay time.Duration) (*httptest.Server, *int, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	peak, cur := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		cur++
		peak = max(peak, cur)
		mu.Unlock()
		defer func() { mu.Lock(); cur--; mu.Unlock() }()
		time.Sleep(delay)
		var req struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range data {
			data[i] = map[string]any{"index": i, "embedding": []float32{1, 0}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv, &peak, &mu
}

func byoResolved(textModel string) *tenant.Resolved {
	res := stagestest.Resolved()
	res.Config.Embedders.Text = tenant.EmbedderConfig{Provider: tenant.ProviderBYO, ModelID: textModel}
	res.Config.Stores.Vector = tenant.StoreConfig{Provider: tenant.ProviderBYO, ConnectionID: "conn", Collection: "c",
		Vectors: map[string]tenant.VectorSpec{"text": {Dim: 2, Metric: "cosine"}}}
	return res
}

func TestBYOProvidersAreGuardedNamedAndRefreshed(t *testing.T) {
	srv, peak, mu := slowEmbeddings(t, 50*time.Millisecond)
	host := strings.TrimPrefix(srv.URL, "http://")
	src := fakeSource{
		models: map[string]tenant.Model{
			"good":    {ID: "good", Kind: "embedder", Type: tenant.ModelOpenAIEmbeddings, Name: "emb", Endpoint: srv.URL, SecretRef: "s1", Status: tenant.StatusValidated, Input: tenant.ModelInput{Modality: "text"}, Output: tenant.ModelOutput{Dim: 2}},
			"pending": {ID: "pending", Kind: "embedder", Type: tenant.ModelOpenAIEmbeddings, Name: "emb", Endpoint: srv.URL, Status: tenant.StatusRegistered, Output: tenant.ModelOutput{Dim: 2}},
		},
		conns: map[string]tenant.Connection{"conn": {ID: "conn", Kind: tenant.ConnectionVectorStore, Type: tenant.StoreQdrant, Endpoint: "https://qdrant.example.com", Status: tenant.StatusUntested}},
	}
	byo := &BYO{Source: src, Secrets: memSecrets{"s1": "sk"}, Egress: security.NewEgress(security.EgressConfig{Trusted: []string{host}}),
		Bulkheads: resilience.NewBulkheads(2)}
	r, _, _ := newTestRegistry(t, Options{Size: 64, BYO: byo, BYORefresh: time.Hour})
	r.Register(KindEmbedder, tenant.ProviderBYO, byoEmbedder)
	r.Register(KindVectorStore, tenant.ProviderBYO, byoVectors)
	ctx := context.Background()

	p, err := r.For(ctx, byoResolved("good"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Names["text_embedder"] != "byo:openai_embeddings:emb" || !p.BYO["text_embedder"] || !p.BYO["vector"] || p.BYO["image_embedder"] {
		t.Errorf("names %v byo %v", p.Names, p.BYO)
	}
	// An untested connection builds a provider that fails every call, so the stage falls back
	// instead of the request failing; it is retried after byoRetry.
	if _, err := p.Vectors.Search(ctx, pipeline.VectorSearch{Vector: "text", Query: []float32{1, 0}, K: 1}); !errors.Is(err, errBYOUnavailable) {
		t.Errorf("unvalidated connection: %v", err)
	}
	if p.Names["vector"] != "byo:qdrant" {
		t.Errorf("broken store name = %s", p.Names["vector"])
	}

	var wg sync.WaitGroup
	var full int
	var fmu sync.Mutex
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.TextEmbedder.EmbedText(ctx, []string{"x"}); errors.Is(err, resilience.ErrBulkheadFull) {
				fmu.Lock()
				full++
				fmu.Unlock()
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	gotPeak := *peak
	mu.Unlock()
	if gotPeak > 2 || full != 3 {
		t.Errorf("bulkhead of 2: peak in flight %d, refused %d", gotPeak, full)
	}

	pending := byoResolved("pending")
	pending.ConfigVersion = 2
	pp, err := r.For(ctx, pending)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pp.TextEmbedder.EmbedText(ctx, []string{"x"}); !errors.Is(err, errBYOUnavailable) || pp.TextEmbedder.Dim() != 2 {
		t.Errorf("unvalidated model: %v (dim %d)", err, pp.TextEmbedder.Dim())
	}
	k := cacheKey{tenant: stagestest.TenantID, version: byoResolved("good").ConfigVersion, role: "vector"}
	e, _ := r.cache.Peek(k)
	if e == nil || e.expires.IsZero() || time.Until(e.expires) > byoRetry {
		t.Errorf("broken providers expire after byoRetry: %+v", e)
	}
	if _, err := New(Platform{}, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil))).For(ctx, byoResolved("good")); err == nil {
		t.Error("without BYO deps a BYO config fails clearly")
	}
}

func TestNewBYOEmbedderAndStoreErrors(t *testing.T) {
	byo := &BYO{Secrets: memSecrets{}, Egress: security.NewEgress(security.EgressConfig{})}
	ctx := context.Background()
	for _, m := range []tenant.Model{
		{Kind: "rank", Type: tenant.ModelKServeV2},
		{Kind: "embedder", Type: tenant.ModelOpenAIEmbeddings, SecretRef: "missing"},
		{Kind: "embedder", Type: tenant.ModelOpenAIEmbeddings, Input: tenant.ModelInput{Modality: "image"}},
		{Kind: "embedder", Type: tenant.ModelONNX},
		{Kind: "embedder", Type: tenant.ModelKServeV2, Endpoint: "http://example.com", Output: tenant.ModelOutput{Dim: 2}, Input: tenant.ModelInput{Modality: "text"}, Name: "m"},
	} {
		if _, err := NewBYOEmbedder(ctx, byo, stagestest.TenantID, tenant.Plan{}, m); err == nil {
			t.Errorf("NewBYOEmbedder(%+v) must fail", m)
		}
	}
	img, err := NewBYOEmbedder(ctx, byo, stagestest.TenantID, tenant.Plan{}, tenant.Model{Kind: "embedder", Type: tenant.ModelKServeV2, Name: "clip",
		Endpoint: "https://kserve.example.com", Input: tenant.ModelInput{Modality: "image"}, Output: tenant.ModelOutput{Dim: 512},
		Settings: tenant.ModelSettings{SupportsTextToImage: true, Transport: "rest"}})
	if err != nil || img.Dim() != 512 {
		t.Fatalf("image model: %v", err)
	}
	for _, c := range []tenant.Connection{
		{Kind: tenant.ConnectionLexical, Type: tenant.StoreQdrant},
		{Kind: tenant.ConnectionVectorStore, Type: "faiss"},
		{Kind: tenant.ConnectionVectorStore, Type: tenant.StorePgvector, Endpoint: "postgres://db.example.com/x?sslmode=disable"},
		{Kind: tenant.ConnectionVectorStore, Type: tenant.StoreMilvus, Endpoint: "http://milvus.example.com"},
	} {
		if _, err := NewBYOVectorStore(ctx, byo, stagestest.TenantID, tenant.Plan{}, c, tenant.StoreConfig{Collection: "c"}); err == nil {
			t.Errorf("NewBYOVectorStore(%+v) must fail", c)
		}
	}
	for _, typ := range []string{tenant.StoreQdrant, tenant.StorePgvector, tenant.StoreMilvus} {
		endpoint := "https://store.example.com"
		if typ == tenant.StorePgvector {
			endpoint = "postgres://u@store.example.com/db?sslmode=verify-full"
		}
		st, err := NewBYOVectorStore(ctx, byo, stagestest.TenantID, tenant.Plan{}, tenant.Connection{Kind: tenant.ConnectionVectorStore, Type: typ, Endpoint: endpoint}, tenant.StoreConfig{Collection: "c"})
		if err != nil || st.ProviderName() != "byo:"+typ {
			t.Errorf("%s: %v", typ, err)
			continue
		}
		_ = st.Close()
	}
}
