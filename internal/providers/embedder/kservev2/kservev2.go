package kservev2

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
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
	Dim        int
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

// Embedder calls KServe v2 / Open Inference Protocol embedders over gRPC.
type Embedder struct {
	client inference.GRPCInferenceServiceClient
	cfg    Config
}

var _ pipeline.Embedder = (*Embedder)(nil)

// New returns an Embedder using conn, which the caller owns.
func New(conn grpc.ClientConnInterface, cfg Config) (*Embedder, error) {
	if cfg.Dim <= 0 {
		return nil, fmt.Errorf("kservev2 embedder: dim must be > 0")
	}
	if cfg.TextModel == "" && cfg.ImageModel == "" {
		return nil, fmt.Errorf("kservev2 embedder: no model configured")
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 32
	}
	return &Embedder{client: inference.NewGRPCInferenceServiceClient(conn), cfg: cfg}, nil
}

// Dim is the embedding dimension.
func (e *Embedder) Dim() int { return e.cfg.Dim }

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
		resp, err := e.client.ModelReady(ctx, &inference.ModelReadyRequest{Name: m})
		if err != nil {
			return fmt.Errorf("model %s ready: %w", m, err)
		}
		if !resp.GetReady() {
			return fmt.Errorf("model %s is not ready", m)
		}
	}
	return nil
}

func (e *Embedder) infer(ctx context.Context, model, input string, items [][]byte) ([][]float32, error) {
	out := make([][]float32, 0, len(items))
	for start := 0; start < len(items); start += e.cfg.MaxBatch {
		batch := items[start:min(start+e.cfg.MaxBatch, len(items))]
		resp, err := e.client.ModelInfer(ctx, &inference.ModelInferRequest{
			ModelName: model,
			Inputs: []*inference.ModelInferRequest_InferInputTensor{{
				Name:     input,
				Datatype: "BYTES",
				Shape:    []int64{int64(len(batch))},
				Contents: &inference.InferTensorContents{BytesContents: batch},
			}},
			Outputs: []*inference.ModelInferRequest_InferRequestedOutputTensor{{Name: Output}},
		})
		if status.Code(err) == codes.InvalidArgument {
			return nil, fmt.Errorf("infer %s: %w: %w", model, pipeline.ErrInvalidInput, err)
		}
		if err != nil {
			return nil, fmt.Errorf("infer %s: %w", model, err)
		}
		vecs, err := decodeEmbeddings(resp, len(batch), e.cfg.Dim)
		if err != nil {
			return nil, fmt.Errorf("infer %s: %w", model, err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func decodeEmbeddings(resp *inference.ModelInferResponse, n, dim int) ([][]float32, error) {
	for i, t := range resp.GetOutputs() {
		if t.GetName() != Output {
			continue
		}
		if t.GetDatatype() != "FP32" {
			return nil, fmt.Errorf("%s datatype %s, want FP32", Output, t.GetDatatype())
		}
		if s := t.GetShape(); len(s) != 2 || s[0] != int64(n) || s[1] != int64(dim) {
			return nil, fmt.Errorf("%s shape %v, want [%d %d]", Output, s, n, dim)
		}
		flat := t.GetContents().GetFp32Contents()
		if raw := resp.GetRawOutputContents(); i < len(raw) {
			if len(raw[i]) != n*dim*4 {
				return nil, fmt.Errorf("%s raw contents are %d bytes, want %d", Output, len(raw[i]), n*dim*4)
			}
			flat = make([]float32, n*dim)
			for j := range flat {
				flat[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i][j*4:]))
			}
		}
		if len(flat) != n*dim {
			return nil, fmt.Errorf("%s has %d values, want %d", Output, len(flat), n*dim)
		}
		vecs := make([][]float32, n)
		for j := range vecs {
			vecs[j] = flat[j*dim : (j+1)*dim : (j+1)*dim]
		}
		return vecs, nil
	}
	return nil, fmt.Errorf("response has no %s output", Output)
}
