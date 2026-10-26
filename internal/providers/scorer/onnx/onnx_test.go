package onnx

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
)

// initRuntime loads ONNX Runtime from ONNXRUNTIME_LIB or third_party (`make onnxruntime`).
func initRuntime(t testing.TB) {
	t.Helper()
	lib := os.Getenv("ONNXRUNTIME_LIB")
	if lib == "" || !filepath.IsAbs(lib) {
		_, here, _, _ := runtime.Caller(0)
		lib = filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "third_party", "onnxruntime", "lib", "libonnxruntime.so")
	}
	if _, err := os.Stat(lib); err != nil {
		t.Skipf("ONNX Runtime not installed (%s); run `make onnxruntime`", lib)
	}
	if err := Init(lib); err != nil {
		t.Fatal(err)
	}
}

func model(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func schema(names ...string) pipeline.FeatureSchema {
	d := map[string]float64{}
	for _, n := range names {
		d[n] = feature.Defaults()[n]
	}
	return pipeline.FeatureSchema{Names: names, Defaults: d}
}

func TestLinearScoresWithDefaultsAndBoost(t *testing.T) {
	initRuntime(t)
	s, err := New(model(t, "linear3.onnx"), schema(feature.RRFScore, feature.Rating, feature.CTR7d), Options{Name: "onnx:linear3"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	cs := []pipeline.Candidate{
		{Features: map[string]float64{feature.RRFScore: 1, feature.Rating: 0.5, feature.CTR7d: 0.1}},
		{Features: map[string]float64{feature.RRFScore: 0.5}, PolicyBoost: 0.25}, // rating default 0.6, ctr default 0
	}
	got, err := s.Score(context.Background(), nil, cs)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{1 + 1 + 0.3, 0.5 + 1.2 + 0 + 0.25}
	for i := range want {
		if math.Abs(float64(got[i])-want[i]) > 1e-5 {
			t.Errorf("score %d = %v, want %v", i, got[i], want[i])
		}
	}
	if s.ProviderName() != "onnx:linear3" || len(s.FeatureSchema().Names) != 3 {
		t.Error("schema and name")
	}
}

func TestGBDTParityWithLightGBM(t *testing.T) {
	initRuntime(t)
	var exp struct {
		Inputs [][]float32 `json:"inputs"`
		Scores []float64   `json:"scores"`
	}
	if err := json.Unmarshal(model(t, "gbdt4.expected.json"), &exp); err != nil {
		t.Fatal(err)
	}
	s, err := New(model(t, "gbdt4.onnx"), schema("a", "b", "c", "d"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var flat []float32
	for _, row := range exp.Inputs {
		flat = append(flat, row...)
	}
	got, err := s.Run(flat, len(exp.Inputs))
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range exp.Scores {
		if math.Abs(float64(got[i])-w) > 1e-4 {
			t.Errorf("row %d: onnx %v, lightgbm %v", i, got[i], w)
		}
	}
}

func TestContractChecks(t *testing.T) {
	initRuntime(t)
	for name, tc := range map[string]struct {
		model  []byte
		schema pipeline.FeatureSchema
		want   string
	}{
		"width":    {model(t, "linear3.onnx"), schema("a", "b"), "takes 3 features"},
		"input":    {model(t, "bad_input.onnx"), schema("a"), "named FEATURES"},
		"not onnx": {[]byte("not a model"), schema("a"), "not a loadable"},
		"empty":    {nil, schema("a"), "1 byte to 200 MB"},
	} {
		if _, err := New(tc.model, tc.schema, Options{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
		if err := DryRun(tc.model, tc.schema); err == nil {
			t.Errorf("%s: dry run must fail", name)
		}
	}
	if err := DryRun(model(t, "gbdt4.onnx"), schema("a", "b", "c", "d")); err != nil {
		t.Errorf("dry run: %v", err)
	}
}

func BenchmarkScore2000(b *testing.B) {
	initRuntime(b)
	s, err := New(model(b, "gbdt4.onnx"), schema("a", "b", "c", "d"), Options{})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	cs := make([]pipeline.Candidate, 2000)
	for i := range cs {
		cs[i].Features = map[string]float64{"a": float64(i%97) / 97, "b": float64(i%13) / 13, "c": 0.5, "d": 0.1}
	}
	for b.Loop() {
		if _, err := s.Score(context.Background(), nil, cs); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTrainedModel scores 2,000 candidates with a trained model (docs/MODELS.md §5: ≤ 5 ms
// for pre-rank). Set ONNX_BENCH_MODEL to the model and ONNX_BENCH_FEATURES to its manifest.
func BenchmarkTrainedModel(b *testing.B) {
	path, manifest := os.Getenv("ONNX_BENCH_MODEL"), os.Getenv("ONNX_BENCH_FEATURES")
	if path == "" || manifest == "" {
		b.Skip("set ONNX_BENCH_MODEL and ONNX_BENCH_FEATURES (features.json)")
	}
	initRuntime(b)
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	raw, err := os.ReadFile(manifest)
	if err != nil {
		b.Fatal(err)
	}
	var m struct {
		Features []string `json:"features"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		b.Fatal(err)
	}
	s, err := New(data, pipeline.NewFeatureSchema(m.Features), Options{IntraOpThreads: 1})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	cs := make([]pipeline.Candidate, 2000)
	for i := range cs {
		cs[i].Features = map[string]float64{}
		for j, f := range m.Features {
			cs[i].Features[f] = float64((i*7+j*13)%100) / 100
		}
	}
	for b.Loop() {
		if _, err := s.Score(context.Background(), nil, cs); err != nil {
			b.Fatal(err)
		}
	}
}
