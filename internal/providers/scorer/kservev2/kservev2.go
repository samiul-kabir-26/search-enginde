package kservev2

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/zavisoft/search-engine/internal/gen/inference"
	"github.com/zavisoft/search-engine/internal/pipeline"
	embedder "github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
)

// Tensor names (docs/MODELS.md §1).
const (
	Input  = "FEATURES"
	Output = "SCORES"
)

type transport interface {
	infer(ctx context.Context, model, version string, features []float32, n, f int) ([]float32, error)
}

// Scorer scores candidates with one batched call to a KServe v2 model.
type Scorer struct {
	t       transport
	model   string
	version string
	schema  pipeline.FeatureSchema
	name    string
	closer  io.Closer
}

var _ pipeline.Scorer = (*Scorer)(nil)

// NewPlatform scores with a model served by the platform's Triton.
func NewPlatform(conn grpc.ClientConnInterface, model string, schema pipeline.FeatureSchema, name string) *Scorer {
	return &Scorer{t: &grpcTransport{client: inference.NewGRPCInferenceServiceClient(conn)}, model: model, schema: schema, name: name}
}

// NewRemote scores with a tenant's endpoint (gRPC, or REST when validation detected it).
// Nothing is dialed until the first call.
func NewRemote(r embedder.Remote, model, version string, schema pipeline.FeatureSchema) (*Scorer, error) {
	base, err := r.Base()
	if err != nil {
		return nil, err
	}
	s := &Scorer{model: model, version: version, schema: schema, name: r.Name}
	switch r.Transport {
	case embedder.TransportREST:
		if r.HTTP == nil {
			return nil, errors.New("kserve REST transport needs an HTTP client")
		}
		s.t = &restTransport{base: base, client: r.HTTP, token: r.Token}
	case "", embedder.TransportGRPC:
		conn, err := r.GRPCConn()
		if err != nil {
			return nil, err
		}
		s.t, s.closer = &grpcTransport{client: inference.NewGRPCInferenceServiceClient(conn), md: r.Metadata()}, conn
	default:
		return nil, fmt.Errorf("kserve transport %q is not grpc or rest", r.Transport)
	}
	return s, nil
}

// FeatureSchema implements pipeline.Scorer.
func (s *Scorer) FeatureSchema() pipeline.FeatureSchema { return s.schema }

// ProviderName labels the scorer in traces.
func (s *Scorer) ProviderName() string { return s.name }

// Close closes a remote connection.
func (s *Scorer) Close() error {
	if s.closer == nil {
		return nil
	}
	if err := s.closer.Close(); err != nil {
		return fmt.Errorf("close kserve scorer: %w", err)
	}
	return nil
}

// Score sends every candidate in one call; policy_boost is added on top unless the model takes
// it.
func (s *Scorer) Score(ctx context.Context, _ *pipeline.Query, cs []pipeline.Candidate) ([]float32, error) {
	if len(cs) == 0 {
		return nil, nil
	}
	scores, err := s.infer(ctx, s.schema.Matrix(cs), len(cs))
	if err != nil {
		return nil, err
	}
	return s.schema.WithPolicyBoost(scores, cs), nil
}

func (s *Scorer) infer(ctx context.Context, features []float32, n int) ([]float32, error) {
	scores, err := s.t.infer(ctx, s.model, s.version, features, n, len(s.schema.Names))
	if err != nil {
		return nil, fmt.Errorf("score %s: %w", s.model, err)
	}
	if len(scores) != n {
		return nil, fmt.Errorf("score %s: %s has %d values for %d candidates", s.model, Output, len(scores), n)
	}
	return scores, nil
}

// Probe scores two rows of declared defaults and checks the scores are finite (model
// validation and the transport detection of registration).
func (s *Scorer) Probe(ctx context.Context) error {
	row := make([]float32, len(s.schema.Names))
	for i, n := range s.schema.Names {
		row[i] = float32(s.schema.Defaults[n])
	}
	scores, err := s.infer(ctx, append(append([]float32(nil), row...), row...), 2)
	if err != nil {
		return err
	}
	for _, v := range scores {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return errors.New("the model returned a non-finite score for the declared defaults")
		}
	}
	return nil
}

type grpcTransport struct {
	client inference.GRPCInferenceServiceClient
	md     metadata.MD
}

func (g *grpcTransport) infer(ctx context.Context, model, version string, features []float32, n, f int) ([]float32, error) {
	if len(g.md) > 0 {
		ctx = metadata.NewOutgoingContext(ctx, g.md)
	}
	raw := make([]byte, 4*len(features))
	for i, v := range features {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(v))
	}
	resp, err := g.client.ModelInfer(ctx, &inference.ModelInferRequest{
		ModelName: model, ModelVersion: version,
		Inputs:           []*inference.ModelInferRequest_InferInputTensor{{Name: Input, Datatype: "FP32", Shape: []int64{int64(n), int64(f)}}},
		Outputs:          []*inference.ModelInferRequest_InferRequestedOutputTensor{{Name: Output}},
		RawInputContents: [][]byte{raw},
	})
	if status.Code(err) == codes.InvalidArgument {
		return nil, fmt.Errorf("%w: %w", pipeline.ErrInvalidInput, err)
	}
	if err != nil {
		return nil, err
	}
	for i, t := range resp.GetOutputs() {
		if t.GetName() != Output {
			continue
		}
		if t.GetDatatype() != "FP32" {
			return nil, fmt.Errorf("%s datatype %s, want FP32", Output, t.GetDatatype())
		}
		if rawOut := resp.GetRawOutputContents(); i < len(rawOut) {
			b := rawOut[i]
			out := make([]float32, len(b)/4)
			for j := range out {
				out[j] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*j:]))
			}
			return out, nil
		}
		return t.GetContents().GetFp32Contents(), nil
	}
	return nil, fmt.Errorf("response has no %s output", Output)
}

// restTransport speaks the KServe v2 REST protocol with JSON tensors.
type restTransport struct {
	base   string
	client *http.Client
	token  string
}

type restTensor struct {
	Name     string    `json:"name"`
	Shape    []int64   `json:"shape,omitempty"`
	Datatype string    `json:"datatype,omitempty"`
	Data     []float32 `json:"data,omitempty"`
}

func (r *restTransport) infer(ctx context.Context, model, version string, features []float32, n, f int) ([]float32, error) {
	path := r.base + "/v2/models/" + model
	if version != "" {
		path += "/versions/" + version
	}
	body, err := json.Marshal(map[string]any{
		"inputs":  []restTensor{{Name: Input, Shape: []int64{int64(n), int64(f)}, Datatype: "FP32", Data: features}},
		"outputs": []map[string]string{{"name": Output}},
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path+"/infer", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("kserve rest: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(raw[:min(len(raw), 200)]))
		if resp.StatusCode == http.StatusBadRequest {
			return nil, fmt.Errorf("%w: HTTP 400: %s", pipeline.ErrInvalidInput, msg)
		}
		return nil, fmt.Errorf("kserve rest: HTTP %d: %s", resp.StatusCode, msg)
	}
	var out struct {
		Outputs []restTensor `json:"outputs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	for _, t := range out.Outputs {
		if t.Name == Output {
			return t.Data, nil
		}
	}
	return nil, fmt.Errorf("response has no %s output", Output)
}
