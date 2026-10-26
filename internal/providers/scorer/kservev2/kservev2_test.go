package kservev2

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/zavisoft/search-engine/internal/gen/inference"
	"github.com/zavisoft/search-engine/internal/pipeline"
	embedder "github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
)

// sumServer scores each row as the sum of its features, as raw or typed FP32 output.
type sumServer struct {
	inference.UnimplementedGRPCInferenceServiceServer
	raw   bool
	short bool
	auth  string
}

func (s *sumServer) ModelInfer(ctx context.Context, req *inference.ModelInferRequest) (*inference.ModelInferResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.auth = strings.Join(md.Get("authorization"), ",")
	in := req.GetInputs()[0]
	if in.GetName() != Input || in.GetDatatype() != "FP32" || len(in.GetShape()) != 2 {
		return nil, status.Error(codes.InvalidArgument, "bad input")
	}
	n, f := int(in.GetShape()[0]), int(in.GetShape()[1])
	b := req.GetRawInputContents()[0]
	scores := make([]float32, n)
	for i := range n {
		for j := range f {
			scores[i] += math.Float32frombits(binary.LittleEndian.Uint32(b[4*(i*f+j):]))
		}
	}
	if s.short {
		scores = scores[:n-1]
	}
	out := &inference.ModelInferResponse_InferOutputTensor{Name: Output, Datatype: "FP32", Shape: []int64{int64(len(scores))}}
	resp := &inference.ModelInferResponse{Outputs: []*inference.ModelInferResponse_InferOutputTensor{out}}
	if s.raw {
		raw := make([]byte, 4*len(scores))
		for i, v := range scores {
			binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(v))
		}
		resp.RawOutputContents = [][]byte{raw}
	} else {
		out.Contents = &inference.InferTensorContents{Fp32Contents: scores}
	}
	return resp, nil
}

func serve(t *testing.T, srv *sumServer) string {
	t.Helper()
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g := grpc.NewServer()
	inference.RegisterGRPCInferenceServiceServer(g, srv)
	go func() { _ = g.Serve(lis) }()
	t.Cleanup(g.Stop)
	return lis.Addr().String()
}

func dialer() func(context.Context, string) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }
}

var schema = pipeline.FeatureSchema{Names: []string{"a", "b"}, Defaults: map[string]float64{"a": 0.5, "b": 1}}

func candidates() []pipeline.Candidate {
	return []pipeline.Candidate{
		{Features: map[string]float64{"a": 1, "b": 2}},
		{Features: map[string]float64{"a": 3}, PolicyBoost: -0.5}, // b defaults to 1
	}
}

func TestGRPCScoresRawAndTyped(t *testing.T) {
	for _, raw := range []bool{false, true} {
		srv := &sumServer{raw: raw}
		s, err := NewRemote(embedder.Remote{Endpoint: "http://" + serve(t, srv), Token: "tok", Plaintext: true, Dial: dialer(), Name: "byo:rank"}, "rank", "", schema)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Score(context.Background(), nil, candidates())
		if err != nil {
			t.Fatal(err)
		}
		if got[0] != 3 || got[1] != 3.5 {
			t.Errorf("raw=%v scores %v, want [3 3.5] (defaults, then the boost on top)", raw, got)
		}
		if srv.auth != "Bearer tok" || s.ProviderName() != "byo:rank" {
			t.Errorf("auth %q name %q", srv.auth, s.ProviderName())
		}
		if err := s.Probe(context.Background()); err != nil {
			t.Error(err)
		}
		_ = s.Close()
	}
}

func TestShortResponseFails(t *testing.T) {
	s, _ := NewRemote(embedder.Remote{Endpoint: "http://" + serve(t, &sumServer{short: true}), Plaintext: true, Dial: dialer()}, "rank", "", schema)
	defer func() { _ = s.Close() }()
	if _, err := s.Score(context.Background(), nil, candidates()); err == nil || !strings.Contains(err.Error(), "1 values for 2 candidates") {
		t.Errorf("err = %v", err)
	}
}

func TestPlatformScorer(t *testing.T) {
	conn, err := grpc.NewClient("passthrough:///"+serve(t, &sumServer{raw: true}), grpc.WithContextDialer(dialer()),
		grpc.WithTransportCredentials(insecureCreds()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	s := NewPlatform(conn, "rank_gbdt_v1", schema, "platform:rank-gbdt-v1")
	if got, err := s.Score(context.Background(), nil, candidates()); err != nil || got[0] != 3 {
		t.Errorf("got %v err %v", got, err)
	}
	if err := s.Close(); err != nil {
		t.Error(err)
	}
}

func TestREST(t *testing.T) {
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		var req struct {
			Inputs []restTensor `json:"inputs"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		in := req.Inputs[0]
		if in.Name != Input || in.Datatype != "FP32" {
			http.Error(w, "bad input", http.StatusBadRequest)
			return
		}
		n, f := int(in.Shape[0]), int(in.Shape[1])
		out := make([]float32, n)
		for i := range n {
			for j := range f {
				out[i] += in.Data[i*f+j]
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"outputs": []restTensor{{Name: Output, Datatype: "FP32", Shape: []int64{int64(n)}, Data: out}}})
	}))
	defer srv.Close()
	s, err := NewRemote(embedder.Remote{Endpoint: srv.URL + "/base/", Transport: embedder.TransportREST, Token: "t", Plaintext: true, HTTP: srv.Client()}, "rank", "2", schema)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Score(context.Background(), nil, candidates())
	if err != nil || got[0] != 3 || got[1] != 3.5 {
		t.Fatalf("got %v err %v", got, err)
	}
	if path != "/base/v2/models/rank/versions/2/infer" || auth != "Bearer t" {
		t.Errorf("path %s auth %s", path, auth)
	}
}

func TestRemoteNeedsHTTPS(t *testing.T) {
	if _, err := NewRemote(embedder.Remote{Endpoint: "http://example.com"}, "m", "", schema); err == nil {
		t.Error("plain http must be refused for tenant endpoints")
	}
	if _, err := NewRemote(embedder.Remote{Endpoint: "https://example.com", Transport: "smoke"}, "m", "", schema); err == nil {
		t.Error("unknown transport")
	}
}

func insecureCreds() credentials.TransportCredentials { return insecure.NewCredentials() }
