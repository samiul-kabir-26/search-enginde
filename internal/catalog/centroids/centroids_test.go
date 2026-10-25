package centroids

import (
	"context"
	"math"
	"testing"
)

func unit(xs ...float32) []float32 {
	var n float64
	for _, x := range xs {
		n += float64(x * x)
	}
	for i := range xs {
		xs[i] = float32(float64(xs[i]) / math.Sqrt(n))
	}
	return xs
}

type src [][2]any

func (s src) ScrollVectors(_ context.Context, _ int, _ string, fn func(string, []float32, []string) error) error {
	for i, p := range s {
		if err := fn(string(rune('a'+i)), p[0].([]float32), p[1].([]string)); err != nil {
			return err
		}
	}
	return nil
}

func TestBuildNearestAndCodec(t *testing.T) {
	data := src{
		{unit(1, 0, 0), []string{"bags", "handbags"}},
		{unit(0.9, 0.1, 0), []string{"bags", "handbags"}},
		{unit(1, 0.2, 0), []string{"bags", "handbags"}},
		{unit(0, 1, 0), []string{"shoes"}},
		{unit(0, 0.9, 0.1), []string{"shoes"}},
		{unit(0.1, 1, 0), []string{"shoes"}},
		{unit(0, 0, 1), []string{"rare"}},
	}
	idx, err := BuildFrom(context.Background(), data, 1)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Len() != 3 || idx.IDs[0] != "bags" || idx.Counts[1] != 3 {
		t.Fatalf("index = %v %v (categories under MinMembers must be dropped)", idx.IDs, idx.Counts)
	}
	g := idx.Nearest(unit(1, 0.05, 0), 2, 0.1)
	if len(g) == 0 || (g[0].ID != "handbags" && g[0].ID != "bags") {
		t.Errorf("nearest = %+v", g)
	}
	sum := 0.0
	for _, x := range g {
		sum += x.Confidence
	}
	if math.Abs(sum-1) > 0.01 {
		t.Errorf("confidences of kept guesses should sum to ~1, got %v", sum)
	}
	if idx.Nearest([]float32{1, 0}, 2, 0) != nil {
		t.Error("wrong dimension must give no guess")
	}
	var nilIdx *Index
	if nilIdx.Nearest(unit(1, 0, 0), 1, 0) != nil {
		t.Error("nil index must give no guess")
	}

	b, err := idx.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Dim != idx.Dim || len(back.IDs) != 3 || back.Nearest(unit(0, 1, 0), 1, 0)[0].ID != "shoes" {
		t.Errorf("decode mismatch: %+v", back.IDs)
	}
	if _, err := Decode([]byte("nope")); err == nil {
		t.Error("bad magic accepted")
	}
	if _, err := Decode(b[:len(b)-3]); err == nil {
		t.Error("truncated file accepted")
	}
	if err := NewBuilder().Add(unit(1, 0), nil); err != nil {
		t.Error(err)
	}
	bb := NewBuilder()
	_ = bb.Add(unit(1, 0), []string{"x"})
	if err := bb.Add(unit(1, 0, 0), []string{"x"}); err == nil {
		t.Error("dimension change accepted")
	}
}
