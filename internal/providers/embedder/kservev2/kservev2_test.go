package kservev2

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/zavisoft/search-engine/internal/gen/inference"
	"github.com/zavisoft/search-engine/internal/pipeline"
)

// fakeServer returns, for item i of a request, a vector whose first value is the item's byte
// length, so tests can check ordering across split batches. raw selects raw_output_contents.
type fakeServer struct {
	inference.UnimplementedGRPCInferenceServiceServer
	dim   int
	raw   bool
	mu    sync.Mutex
	sizes []int
}

func (f *fakeServer) ModelInfer(_ context.Context, req *inference.ModelInferRequest) (*inference.ModelInferResponse, error) {
	items := req.GetInputs()[0].GetContents().GetBytesContents()
	f.mu.Lock()
	f.sizes = append(f.sizes, len(items))
	f.mu.Unlock()
	flat := make([]float32, len(items)*f.dim)
	for i, it := range items {
		flat[i*f.dim] = float32(len(it))
	}
	out := &inference.ModelInferResponse_InferOutputTensor{Name: Output, Datatype: "FP32", Shape: []int64{int64(len(items)), int64(f.dim)}}
	resp := &inference.ModelInferResponse{ModelName: req.GetModelName(), Outputs: []*inference.ModelInferResponse_InferOutputTensor{out}}
	if f.raw {
		buf := make([]byte, 4*len(flat))
		for i, v := range flat {
			binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
		}
		resp.RawOutputContents = [][]byte{buf}
	} else {
		out.Contents = &inference.InferTensorContents{Fp32Contents: flat}
	}
	return resp, nil
}

func (f *fakeServer) ModelReady(_ context.Context, req *inference.ModelReadyRequest) (*inference.ModelReadyResponse, error) {
	return &inference.ModelReadyResponse{Ready: req.GetName() != "missing"}, nil
}

func dial(t *testing.T, srv *fakeServer) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	inference.RegisterGRPCInferenceServiceServer(s, srv)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func TestEmbedSplitsBatchesAndKeepsOrder(t *testing.T) {
	for _, raw := range []bool{false, true} {
		srv := &fakeServer{dim: 4, raw: raw}
		e, err := New(dial(t, srv), Config{TextModel: "bge_text", Dim: 4, MaxBatch: 3})
		if err != nil {
			t.Fatal(err)
		}
		texts := []string{"a", "bb", "ccc", "dddd", "eeeee", "ffffff", "g"}
		vecs, err := e.EmbedText(context.Background(), texts)
		if err != nil {
			t.Fatal(err)
		}
		if len(vecs) != len(texts) {
			t.Fatalf("raw=%v: got %d vectors", raw, len(vecs))
		}
		for i, v := range vecs {
			if len(v) != 4 || v[0] != float32(len(texts[i])) {
				t.Errorf("raw=%v: vector %d = %v", raw, i, v)
			}
		}
		if got := srv.sizes; len(got) != 3 || got[0] != 3 || got[2] != 1 {
			t.Errorf("raw=%v: batch sizes %v, want [3 3 1]", raw, got)
		}
	}
}

func TestEmbedErrors(t *testing.T) {
	srv := &fakeServer{dim: 4}
	conn := dial(t, srv)
	textOnly, _ := New(conn, Config{TextModel: "bge_text", Dim: 4})
	if _, err := textOnly.EmbedImage(context.Background(), [][]byte{{1}}); !errors.Is(err, pipeline.ErrUnsupported) {
		t.Errorf("EmbedImage on text-only model: %v", err)
	}
	wrongDim, _ := New(conn, Config{ImageModel: "clip_image", Dim: 8})
	if _, err := wrongDim.EmbedImage(context.Background(), [][]byte{{1}}); err == nil {
		t.Error("dimension mismatch must fail")
	}
	if _, err := New(conn, Config{Dim: 4}); err == nil {
		t.Error("no models must fail")
	}
	missing, _ := New(conn, Config{TextModel: "missing", Dim: 4})
	if err := missing.Health(context.Background()); err == nil {
		t.Error("unready model must fail health")
	}
	if err := textOnly.Health(context.Background()); err != nil {
		t.Errorf("health: %v", err)
	}
	if c, ok := PlatformModel("clip-vit-b32"); !ok || c.Dim != 512 || c.ImageModel != "clip_image" {
		t.Errorf("PlatformModel(clip) = %+v", c)
	}
}
