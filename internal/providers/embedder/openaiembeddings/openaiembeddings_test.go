package openaiembeddings

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/zavisoft/search-engine/internal/pipeline"
)

// server answers /v1/embeddings with unnormalized vectors [len(text), 1, 0...] listed in
// reverse order, so ordering by `index` and normalization are both exercised.
func server(t *testing.T, dim int) (*httptest.Server, *[]int) {
	t.Helper()
	var mu sync.Mutex
	batches := &[]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer sk-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "text-embedding-3-small" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		*batches = append(*batches, len(req.Input))
		mu.Unlock()
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i := len(req.Input) - 1; i >= 0; i-- {
			v := make([]float32, dim)
			v[0], v[1] = float32(len(req.Input[i])), 1
			data = append(data, item{Index: i, Embedding: v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	return srv, batches
}

func TestEmbedTextBatchesOrdersAndNormalizes(t *testing.T) {
	srv, batches := server(t, 3)
	defer srv.Close()
	e, err := New(srv.Client(), Config{Endpoint: srv.URL + "/", Model: "text-embedding-3-small", Dim: 3, MaxBatch: 2, APIKey: "sk-1", Name: "byo:openai"})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := e.EmbedText(context.Background(), []string{"aaa", "b", "cc"})
	if err != nil {
		t.Fatal(err)
	}
	for i, n := range []float64{3, 1, 2} {
		want := n / math.Sqrt(n*n+1)
		if math.Abs(float64(vecs[i][0])-want) > 1e-6 {
			t.Errorf("vector %d = %v, want first value %.4f", i, vecs[i], want)
		}
	}
	if len(*batches) != 2 || (*batches)[0] != 2 || (*batches)[1] != 1 {
		t.Errorf("batches = %v", *batches)
	}
	if _, err := e.EmbedImage(context.Background(), nil); !errors.Is(err, pipeline.ErrUnsupported) {
		t.Errorf("images are unsupported: %v", err)
	}
	if e.Dim() != 3 || e.ProviderName() != "byo:openai" || e.Close() != nil {
		t.Error("Dim/ProviderName/Close")
	}
}

func TestEmbedTextErrors(t *testing.T) {
	srv, _ := server(t, 3)
	defer srv.Close()
	ctx := context.Background()
	wrongKey, _ := New(srv.Client(), Config{Endpoint: srv.URL, Model: "text-embedding-3-small", Dim: 3, APIKey: "nope"})
	if _, err := wrongKey.EmbedText(ctx, []string{"x"}); err == nil {
		t.Error("401 must fail")
	}
	badModel, _ := New(srv.Client(), Config{Endpoint: srv.URL, Model: "other", Dim: 3, APIKey: "sk-1"})
	if _, err := badModel.EmbedText(ctx, []string{"x"}); !errors.Is(err, pipeline.ErrInvalidInput) {
		t.Errorf("400 is invalid input: %v", err)
	}
	wrongDim, _ := New(srv.Client(), Config{Endpoint: srv.URL, Model: "text-embedding-3-small", Dim: 4, APIKey: "sk-1"})
	if _, err := wrongDim.EmbedText(ctx, []string{"x"}); err == nil {
		t.Error("a dimension mismatch must fail")
	}
	if _, err := New(srv.Client(), Config{Endpoint: srv.URL, Model: "m"}); err == nil {
		t.Error("dim is required")
	}
	if _, err := New(srv.Client(), Config{Endpoint: srv.URL, Dim: 3}); err == nil {
		t.Error("model is required")
	}
}

func TestNormalize(t *testing.T) {
	if v := Normalize([]float32{0, 0}); v[0] != 0 {
		t.Error("zero vectors stay zero")
	}
	if v := Normalize([]float32{0.6, 0.8}); v[0] != 0.6 {
		t.Error("unit vectors are unchanged")
	}
}
