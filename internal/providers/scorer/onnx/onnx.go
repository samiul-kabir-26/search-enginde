package onnx

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/zavisoft/search-engine/internal/pipeline"
)

// Tensor names (docs/MODELS.md §1).
const (
	Input  = "FEATURES"
	Output = "SCORES"
)

// MaxModelBytes bounds an uploaded model (docs/ARCHITECTURE.md §9).
const MaxModelBytes = 200 << 20

var (
	initOnce sync.Once
	errInit  error
)

// Init loads the ONNX Runtime shared library (ONNXRUNTIME_LIB) for the process. It runs once;
// later calls return the first result.
func Init(libPath string) error {
	initOnce.Do(func() {
		if libPath == "" {
			errInit = errors.New("onnx runtime: no shared library path (set ONNXRUNTIME_LIB)")
			return
		}
		ort.SetSharedLibraryPath(libPath)
		if err := ort.InitializeEnvironment(); err != nil {
			errInit = fmt.Errorf("onnx runtime: %w", err)
		}
	})
	return errInit
}

// Options tune a session.
type Options struct {
	// IntraOpThreads bounds the threads one inference uses (default 1): many requests score
	// concurrently, so per-call parallelism would only oversubscribe the CPU.
	IntraOpThreads int
	// Name labels the scorer in traces.
	Name string
}

// Scorer scores candidates in-process with one ONNX Runtime session. Sessions are safe for
// concurrent use; the registry keeps one per (tenant, config version) and closes it on retire.
type Scorer struct {
	sess   *ort.DynamicAdvancedSession
	schema pipeline.FeatureSchema
	name   string
}

var _ pipeline.Scorer = (*Scorer)(nil)

// New loads model (FEATURES FP32 [N, F] → SCORES FP32 [N] or [N, 1]) for schema, whose names
// must have F entries. Init must have succeeded.
func New(model []byte, schema pipeline.FeatureSchema, opts Options) (*Scorer, error) {
	if err := checkIO(model, len(schema.Names)); err != nil {
		return nil, err
	}
	so, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("session options: %w", err)
	}
	defer func() { _ = so.Destroy() }()
	if err := so.SetIntraOpNumThreads(max(opts.IntraOpThreads, 1)); err != nil {
		return nil, fmt.Errorf("intra-op threads: %w", err)
	}
	if err := so.SetInterOpNumThreads(1); err != nil {
		return nil, fmt.Errorf("inter-op threads: %w", err)
	}
	sess, err := ort.NewDynamicAdvancedSessionWithONNXData(model, []string{Input}, []string{Output}, so)
	if err != nil {
		return nil, fmt.Errorf("onnx session: %w", err)
	}
	return &Scorer{sess: sess, schema: schema, name: opts.Name}, nil
}

// checkIO verifies the model's single input and output against the tensor contract.
func checkIO(model []byte, features int) error {
	if len(model) == 0 || len(model) > MaxModelBytes {
		return fmt.Errorf("model is %d bytes; it must be 1 byte to %d MB", len(model), MaxModelBytes>>20)
	}
	ins, outs, err := ort.GetInputOutputInfoWithONNXData(model)
	if err != nil {
		return fmt.Errorf("not a loadable ONNX model: %w", err)
	}
	if len(ins) != 1 || ins[0].Name != Input {
		return fmt.Errorf("the model must have one input named %s", Input)
	}
	in := ins[0]
	if in.DataType != ort.TensorElementDataTypeFloat || len(in.Dimensions) != 2 {
		return fmt.Errorf("%s must be FP32 [N, F]", Input)
	}
	if f := in.Dimensions[1]; f != int64(features) {
		return fmt.Errorf("%s takes %d features, the declared schema has %d", Input, f, features)
	}
	var out *ort.InputOutputInfo
	for i := range outs {
		if outs[i].Name == Output {
			out = &outs[i]
		}
	}
	if out == nil || out.DataType != ort.TensorElementDataTypeFloat {
		return fmt.Errorf("the model must have an FP32 output named %s", Output)
	}
	if d := out.Dimensions; len(d) == 0 || len(d) > 2 || (len(d) == 2 && d[1] != 1) {
		return fmt.Errorf("%s must be [N] or [N, 1]", Output)
	}
	return nil
}

// FeatureSchema implements pipeline.Scorer.
func (s *Scorer) FeatureSchema() pipeline.FeatureSchema { return s.schema }

// ProviderName labels the scorer in traces.
func (s *Scorer) ProviderName() string { return s.name }

// Score runs one batched inference; policy_boost is added on top unless the model takes it.
func (s *Scorer) Score(ctx context.Context, _ *pipeline.Query, cs []pipeline.Candidate) ([]float32, error) {
	if len(cs) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("onnx score: %w", err)
	}
	scores, err := s.Run(s.schema.Matrix(cs), len(cs))
	if err != nil {
		return nil, err
	}
	return s.schema.WithPolicyBoost(scores, cs), nil
}

// Run scores a row-major [n, F] feature matrix.
func (s *Scorer) Run(features []float32, n int) ([]float32, error) {
	in, err := ort.NewTensor(ort.NewShape(int64(n), int64(len(s.schema.Names))), features)
	if err != nil {
		return nil, fmt.Errorf("input tensor: %w", err)
	}
	defer func() { _ = in.Destroy() }()
	outs := []ort.Value{nil}
	if err := s.sess.Run([]ort.Value{in}, outs); err != nil {
		return nil, fmt.Errorf("onnx inference: %w", err)
	}
	defer func() { _ = outs[0].Destroy() }()
	t, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("%s is not an FP32 tensor", Output)
	}
	data := t.GetData()
	if len(data) != n {
		return nil, fmt.Errorf("%s has %d values for %d candidates", Output, len(data), n)
	}
	return append([]float32(nil), data...), nil
}

// DryRun loads model for schema and scores two rows of declared defaults, as registration does
// (docs/ARCHITECTURE.md §9): the model must load, accept the schema's width and return finite
// scores.
func DryRun(model []byte, schema pipeline.FeatureSchema) error {
	s, err := New(model, schema, Options{})
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	row := make([]float32, len(schema.Names))
	for i, n := range schema.Names {
		row[i] = float32(schema.Defaults[n])
	}
	scores, err := s.Run(append(append([]float32(nil), row...), row...), 2)
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

// Close releases the session.
func (s *Scorer) Close() error {
	if err := s.sess.Destroy(); err != nil {
		return fmt.Errorf("destroy onnx session: %w", err)
	}
	return nil
}
