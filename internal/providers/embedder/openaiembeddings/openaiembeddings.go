package openaiembeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/zavisoft/search-engine/internal/pipeline"
)

// Config is a tenant's OpenAI-compatible embeddings endpoint (docs/MODELS.md §1).
type Config struct {
	// Endpoint is the base URL; requests go to {Endpoint}/v1/embeddings.
	Endpoint string
	// Model is the `model` request field.
	Model string
	Dim   int
	// MaxBatch is the largest `input` list sent in one request (default 64).
	MaxBatch int
	// APIKey, when set, is sent as a bearer token.
	APIKey string
	// Name labels the provider in traces.
	Name string
}

// Embedder embeds text through POST {endpoint}/v1/embeddings. It is text only; vectors are
// L2-normalized client-side when the endpoint does not normalize them.
type Embedder struct {
	client *http.Client
	cfg    Config
	url    string
}

var _ pipeline.Embedder = (*Embedder)(nil)

// New returns an embedder using client (the egress client).
func New(client *http.Client, cfg Config) (*Embedder, error) {
	if cfg.Dim <= 0 {
		return nil, fmt.Errorf("openai embeddings: dim must be > 0")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("openai embeddings: model is required")
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 64
	}
	return &Embedder{client: client, cfg: cfg, url: strings.TrimSuffix(cfg.Endpoint, "/") + "/v1/embeddings"}, nil
}

// Dim is the embedding dimension.
func (e *Embedder) Dim() int { return e.cfg.Dim }

// ProviderName labels the embedder in traces and breaker keys.
func (e *Embedder) ProviderName() string { return e.cfg.Name }

// Close releases idle connections.
func (e *Embedder) Close() error {
	e.client.CloseIdleConnections()
	return nil
}

// EmbedImage is unsupported: openai_embeddings models are text only.
func (e *Embedder) EmbedImage(context.Context, [][]byte) ([][]float32, error) {
	return nil, fmt.Errorf("image embeddings: %w", pipeline.ErrUnsupported)
}

// EmbedText embeds texts, splitting them into batches of at most MaxBatch.
func (e *Embedder) EmbedText(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += e.cfg.MaxBatch {
		vecs, err := e.embed(ctx, texts[start:min(start+e.cfg.MaxBatch, len(texts))])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

type response struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (e *Embedder) embed(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": e.cfg.Model, "input": texts})
	if err != nil {
		return nil, fmt.Errorf("embeddings: encode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embeddings: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.APIKey)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("embeddings: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return nil, fmt.Errorf("embeddings: %w: %s", pipeline.ErrInvalidInput, resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("embeddings: %s", resp.Status)
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("embeddings: decode: %w", err)
	}
	if len(r.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d inputs", len(r.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(texts) || out[d.Index] != nil {
			return nil, fmt.Errorf("embeddings: bad or repeated index %d", d.Index)
		}
		if len(d.Embedding) != e.cfg.Dim {
			return nil, fmt.Errorf("embeddings: vector has %d dimensions, want %d", len(d.Embedding), e.cfg.Dim)
		}
		out[d.Index] = Normalize(d.Embedding)
	}
	return out, nil
}

// Normalize scales v to unit L2 norm in place, unless it already is (or is all zeros).
func Normalize(v []float32) []float32 {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := math.Sqrt(sum)
	if n == 0 || math.Abs(n-1) < 1e-4 {
		return v
	}
	for i := range v {
		v[i] = float32(float64(v[i]) / n)
	}
	return v
}
