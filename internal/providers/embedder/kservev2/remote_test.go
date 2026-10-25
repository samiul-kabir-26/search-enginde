package kservev2

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/zavisoft/search-engine/internal/gen/inference"
	"github.com/zavisoft/search-engine/internal/pipeline"
)

// restServer implements the KServe v2 REST infer and ready calls: each item's vector starts
// with its byte length. binaryOut answers with the binary data extension.
func restServer(t *testing.T, dim int, binaryOut bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/ready") {
			if strings.Contains(r.URL.Path, "/missing/") {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/versions/3/infer") {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		header := body
		if n := r.Header.Get("Inference-Header-Content-Length"); n != "" {
			hl, _ := strconv.Atoi(n)
			header = body[:hl]
		}
		var req struct {
			Inputs []struct {
				Name       string         `json:"name"`
				Data       []string       `json:"data"`
				Parameters map[string]any `json:"parameters"`
			} `json:"inputs"`
		}
		if err := json.Unmarshal(header, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var sizes []int
		in := req.Inputs[0]
		if in.Name == InputText {
			for _, d := range in.Data {
				sizes = append(sizes, len(d))
			}
		} else {
			bin := body[len(header):]
			for len(bin) >= 4 {
				n := int(binary.LittleEndian.Uint32(bin))
				if n == 0 {
					http.Error(w, "empty image", http.StatusBadRequest)
					return
				}
				sizes = append(sizes, n)
				bin = bin[4+n:]
			}
		}
		flat := make([]float32, len(sizes)*dim)
		for i, n := range sizes {
			flat[i*dim] = float32(n)
		}
		out := map[string]any{"name": Output, "datatype": "FP32", "shape": []int{len(sizes), dim}}
		if binaryOut {
			buf := &bytes.Buffer{}
			for _, f := range flat {
				_ = binary.Write(buf, binary.LittleEndian, math.Float32bits(f))
			}
			out["parameters"] = map[string]any{"binary_data_size": buf.Len()}
			h, _ := json.Marshal(map[string]any{"outputs": []any{map[string]any{"name": "OTHER", "parameters": map[string]any{"binary_data_size": 0}}, out}})
			w.Header().Set("Inference-Header-Content-Length", strconv.Itoa(len(h)))
			_, _ = w.Write(append(h, buf.Bytes()...))
			return
		}
		out["data"] = flat
		_ = json.NewEncoder(w).Encode(map[string]any{"outputs": []any{out}})
	}))
}

func TestRemoteREST(t *testing.T) {
	for _, binaryOut := range []bool{false, true} {
		srv := restServer(t, 4, binaryOut)
		e, err := NewRemote(Remote{Endpoint: srv.URL, Transport: TransportREST, Token: "tok", Plaintext: true, HTTP: srv.Client(), Name: "byo:kserve_v2:clip"},
			Config{TextModel: "clip", ImageModel: "clip", Version: "3", Dim: 4, MaxBatch: 2})
		if err != nil {
			t.Fatal(err)
		}
		vecs, err := e.EmbedText(context.Background(), []string{"a", "bbb", "cc"})
		if err != nil || len(vecs) != 3 || vecs[1][0] != 3 || vecs[2][0] != 2 {
			t.Fatalf("binary=%v text: %v %v", binaryOut, vecs, err)
		}
		vecs, err = e.EmbedImage(context.Background(), [][]byte{[]byte("12345"), []byte("1")})
		if err != nil || vecs[0][0] != 5 || vecs[1][0] != 1 {
			t.Fatalf("binary=%v image: %v %v", binaryOut, vecs, err)
		}
		if _, err := e.EmbedImage(context.Background(), [][]byte{{}}); !errors.Is(err, pipeline.ErrInvalidInput) {
			t.Errorf("a 400 is invalid input: %v", err)
		}
		if err := e.Health(context.Background()); err != nil {
			t.Errorf("health: %v", err)
		}
		if e.ProviderName() != "byo:kserve_v2:clip" || e.Close() != nil {
			t.Error("name/close")
		}
		srv.Close()
	}
	srv := restServer(t, 4, false)
	defer srv.Close()
	bad, _ := NewRemote(Remote{Endpoint: srv.URL, Transport: TransportREST, Token: "wrong", Plaintext: true, HTTP: srv.Client()}, Config{TextModel: "m", Version: "3", Dim: 4})
	if _, err := bad.EmbedText(context.Background(), []string{"x"}); err == nil {
		t.Error("401 must fail")
	}
	missing, _ := NewRemote(Remote{Endpoint: srv.URL, Transport: TransportREST, Token: "tok", Plaintext: true, HTTP: srv.Client()}, Config{TextModel: "missing", Version: "3", Dim: 4})
	if err := missing.Health(context.Background()); err == nil {
		t.Error("unready model must fail health")
	}
	wrongDim, _ := NewRemote(Remote{Endpoint: srv.URL, Transport: TransportREST, Token: "tok", Plaintext: true, HTTP: srv.Client()}, Config{TextModel: "m", Version: "3", Dim: 8})
	if _, err := wrongDim.EmbedText(context.Background(), []string{"x"}); err == nil {
		t.Error("a shape mismatch must fail")
	}
}

func TestRemoteConfigErrors(t *testing.T) {
	for _, r := range []Remote{
		{Endpoint: "http://example.com", Transport: TransportGRPC},
		{Endpoint: "::bad"},
		{Endpoint: "https://example.com", Transport: "soap"},
		{Endpoint: "https://example.com", Transport: TransportREST},
	} {
		if _, err := NewRemote(r, Config{TextModel: "m", Dim: 4}); err == nil {
			t.Errorf("NewRemote(%+v) must fail", r)
		}
	}
}

type tokenServer struct {
	inference.UnimplementedGRPCInferenceServiceServer
	auth chan string
}

func (s *tokenServer) ModelInfer(ctx context.Context, req *inference.ModelInferRequest) (*inference.ModelInferResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	s.auth <- strings.Join(md.Get("authorization"), ",") + "|" + req.GetModelVersion()
	n := len(req.GetInputs()[0].GetContents().GetBytesContents())
	return &inference.ModelInferResponse{Outputs: []*inference.ModelInferResponse_InferOutputTensor{{
		Name: Output, Datatype: "FP32", Shape: []int64{int64(n), 2}, Contents: &inference.InferTensorContents{Fp32Contents: make([]float32, 2*n)},
	}}}, nil
}

func TestRemoteGRPCSendsTokenAndVersion(t *testing.T) {
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	ts := &tokenServer{auth: make(chan string, 1)}
	inference.RegisterGRPCInferenceServiceServer(srv, ts)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	var d net.Dialer
	e, err := NewRemote(Remote{Endpoint: "http://" + lis.Addr().String(), Token: "tok", Plaintext: true,
		Dial: func(ctx context.Context, addr string) (net.Conn, error) { return d.DialContext(ctx, "tcp", addr) }},
		Config{TextModel: "bge", Version: "7", Dim: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.EmbedText(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if got := <-ts.auth; got != "Bearer tok|7" {
		t.Errorf("metadata|version = %q", got)
	}
}
