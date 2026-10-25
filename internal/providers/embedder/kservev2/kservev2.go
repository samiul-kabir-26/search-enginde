package kservev2

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/zavisoft/search-engine/internal/gen/inference"
	"github.com/zavisoft/search-engine/internal/pipeline"
)

// Tensor names fixed by docs/MODELS.md §1.
const (
	InputText  = "INPUT_TEXT"
	InputImage = "INPUT_IMAGE"
	Output     = "EMBEDDING"
)

// Config names the KServe v2 models behind one embedder. An empty model name means that
// modality is unsupported.
type Config struct {
	TextModel  string
	ImageModel string
	// Version is the model version sent with every request ("" lets the server choose).
	Version string
	Dim     int
	// MaxBatch is the largest batch sent in one request; larger inputs are split.
	MaxBatch int
}

// PlatformModel returns the Triton models for a platform embedder ID (docs/MODELS.md §2).
func PlatformModel(id string) (Config, bool) {
	switch id {
	case "bge-small-en-v1.5":
		return Config{TextModel: "bge_text", Dim: 384, MaxBatch: 32}, true
	case "clip-vit-b32":
		return Config{TextModel: "clip_text", ImageModel: "clip_image", Dim: 512, MaxBatch: 32}, true
	}
	return Config{}, false
}

// transport performs one inference call and returns the EMBEDDING tensor's values and shape.
type transport interface {
	infer(ctx context.Context, model, version, input string, items [][]byte) (flat []float32, shape []int64, err error)
	ready(ctx context.Context, model, version string) (bool, error)
}

// Embedder calls KServe v2 / Open Inference Protocol embedders over gRPC or REST.
type Embedder struct {
	t      transport
	cfg    Config
	closer io.Closer
	name   string
}

var _ pipeline.Embedder = (*Embedder)(nil)

// New returns an Embedder using conn, which the caller owns.
func New(conn grpc.ClientConnInterface, cfg Config) (*Embedder, error) {
	return newEmbedder(&grpcTransport{client: inference.NewGRPCInferenceServiceClient(conn)}, cfg, nil, "")
}

func newEmbedder(t transport, cfg Config, closer io.Closer, name string) (*Embedder, error) {
	if cfg.Dim <= 0 {
		return nil, fmt.Errorf("kservev2 embedder: dim must be > 0")
	}
	if cfg.TextModel == "" && cfg.ImageModel == "" {
		return nil, fmt.Errorf("kservev2 embedder: no model configured")
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 32
	}
	return &Embedder{t: t, cfg: cfg, closer: closer, name: name}, nil
}

// Dim is the embedding dimension.
func (e *Embedder) Dim() int { return e.cfg.Dim }

// Close releases a BYO endpoint's connection; platform embedders share the caller's.
func (e *Embedder) Close() error {
	if e.closer == nil {
		return nil
	}
	if err := e.closer.Close(); err != nil {
		return fmt.Errorf("close kserve connection: %w", err)
	}
	return nil
}

// ProviderName labels a BYO embedder in traces and breaker keys ("" for platform models).
func (e *Embedder) ProviderName() string { return e.name }

// EmbedText embeds UTF-8 texts.
func (e *Embedder) EmbedText(ctx context.Context, texts []string) ([][]float32, error) {
	if e.cfg.TextModel == "" {
		return nil, fmt.Errorf("text embeddings: %w", pipeline.ErrUnsupported)
	}
	items := make([][]byte, len(texts))
	for i, t := range texts {
		items[i] = []byte(t)
	}
	return e.infer(ctx, e.cfg.TextModel, InputText, items)
}

// EmbedImage embeds encoded images (JPEG, PNG or WebP).
func (e *Embedder) EmbedImage(ctx context.Context, images [][]byte) ([][]float32, error) {
	if e.cfg.ImageModel == "" {
		return nil, fmt.Errorf("image embeddings: %w", pipeline.ErrUnsupported)
	}
	return e.infer(ctx, e.cfg.ImageModel, InputImage, images)
}

// Health reports whether every configured model is ready.
func (e *Embedder) Health(ctx context.Context) error {
	for _, m := range []string{e.cfg.TextModel, e.cfg.ImageModel} {
		if m == "" {
			continue
		}
		ok, err := e.t.ready(ctx, m, e.cfg.Version)
		if err != nil {
			return fmt.Errorf("model %s ready: %w", m, err)
		}
		if !ok {
			return fmt.Errorf("model %s is not ready", m)
		}
	}
	return nil
}

func (e *Embedder) infer(ctx context.Context, model, input string, items [][]byte) ([][]float32, error) {
	out := make([][]float32, 0, len(items))
	for start := 0; start < len(items); start += e.cfg.MaxBatch {
		batch := items[start:min(start+e.cfg.MaxBatch, len(items))]
		flat, shape, err := e.t.infer(ctx, model, e.cfg.Version, input, batch)
		if err != nil {
			return nil, fmt.Errorf("infer %s: %w", model, err)
		}
		n, dim := len(batch), e.cfg.Dim
		if len(shape) != 2 || shape[0] != int64(n) || shape[1] != int64(dim) {
			return nil, fmt.Errorf("infer %s: %s shape %v, want [%d %d]", model, Output, shape, n, dim)
		}
		if len(flat) != n*dim {
			return nil, fmt.Errorf("infer %s: %s has %d values, want %d", model, Output, len(flat), n*dim)
		}
		for j := range n {
			out = append(out, flat[j*dim:(j+1)*dim:(j+1)*dim])
		}
	}
	return out, nil
}

// grpcTransport speaks the KServe v2 gRPC protocol. md, when set, is sent with every call
// (a BYO endpoint's bearer token).
type grpcTransport struct {
	client inference.GRPCInferenceServiceClient
	md     metadata.MD
}

func (g *grpcTransport) ctx(ctx context.Context) context.Context {
	if len(g.md) == 0 {
		return ctx
	}
	return metadata.NewOutgoingContext(ctx, g.md)
}

func (g *grpcTransport) infer(ctx context.Context, model, version, input string, items [][]byte) ([]float32, []int64, error) {
	resp, err := g.client.ModelInfer(g.ctx(ctx), &inference.ModelInferRequest{
		ModelName:    model,
		ModelVersion: version,
		Inputs: []*inference.ModelInferRequest_InferInputTensor{{
			Name:     input,
			Datatype: "BYTES",
			Shape:    []int64{int64(len(items))},
			Contents: &inference.InferTensorContents{BytesContents: items},
		}},
		Outputs: []*inference.ModelInferRequest_InferRequestedOutputTensor{{Name: Output}},
	})
	if status.Code(err) == codes.InvalidArgument {
		return nil, nil, fmt.Errorf("%w: %w", pipeline.ErrInvalidInput, err)
	}
	if err != nil {
		return nil, nil, err
	}
	return decodeGRPC(resp)
}

func (g *grpcTransport) ready(ctx context.Context, model, version string) (bool, error) {
	resp, err := g.client.ModelReady(g.ctx(ctx), &inference.ModelReadyRequest{Name: model, Version: version})
	if err != nil {
		return false, err
	}
	return resp.GetReady(), nil
}

func decodeGRPC(resp *inference.ModelInferResponse) ([]float32, []int64, error) {
	for i, t := range resp.GetOutputs() {
		if t.GetName() != Output {
			continue
		}
		if t.GetDatatype() != "FP32" {
			return nil, nil, fmt.Errorf("%s datatype %s, want FP32", Output, t.GetDatatype())
		}
		flat := t.GetContents().GetFp32Contents()
		if raw := resp.GetRawOutputContents(); i < len(raw) {
			if len(raw[i])%4 != 0 {
				return nil, nil, fmt.Errorf("%s raw contents are %d bytes, not a multiple of 4", Output, len(raw[i]))
			}
			flat = littleEndianFloats(raw[i])
		}
		return flat, t.GetShape(), nil
	}
	return nil, nil, fmt.Errorf("response has no %s output", Output)
}

func littleEndianFloats(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for j := range out {
		out[j] = math.Float32frombits(binary.LittleEndian.Uint32(b[j*4:]))
	}
	return out
}
