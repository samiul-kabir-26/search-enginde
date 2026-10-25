package kservev2

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/zavisoft/search-engine/internal/gen/inference"
	"github.com/zavisoft/search-engine/internal/pipeline"
)

// Transports for remote (BYO) endpoints.
const (
	TransportGRPC = "grpc"
	TransportREST = "rest"
)

// Remote describes a tenant's KServe v2 endpoint.
type Remote struct {
	// Endpoint is https://host[:port][/base]; gRPC uses host:port, REST the full base URL.
	Endpoint string
	// Transport is grpc (default) or rest; model validation detects it (REST is the fallback
	// transport for endpoints without gRPC).
	Transport string
	// Token, when set, is sent as a bearer token.
	Token string
	// Plaintext allows http for platform-trusted hosts only (tests, dev).
	Plaintext bool
	// Dial is the egress dialer (gRPC); HTTP is the egress client (REST).
	Dial func(ctx context.Context, addr string) (net.Conn, error)
	HTTP *http.Client
	// Name labels the provider in traces.
	Name string
}

// NewRemote returns an embedder for a tenant endpoint. Nothing is dialed until the first call.
func NewRemote(r Remote, cfg Config) (*Embedder, error) {
	u, err := url.Parse(r.Endpoint)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("kserve endpoint %q is not a URL", r.Endpoint)
	}
	if u.Scheme != "https" && !r.Plaintext {
		return nil, fmt.Errorf("kserve endpoint %q must be https", r.Endpoint)
	}
	switch r.Transport {
	case TransportREST:
		if r.HTTP == nil {
			return nil, fmt.Errorf("kserve REST transport needs an HTTP client")
		}
		return newEmbedder(&restTransport{base: strings.TrimSuffix(u.String(), "/"), client: r.HTTP, token: r.Token}, cfg, nil, r.Name)
	case "", TransportGRPC:
		port := u.Port()
		if port == "" {
			port = "443"
			if u.Scheme == "http" {
				port = "80"
			}
		}
		creds := insecure.NewCredentials()
		if u.Scheme == "https" {
			creds = credentials.NewTLS(&tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12})
		}
		conn, err := grpc.NewClient("passthrough:///"+net.JoinHostPort(u.Hostname(), port),
			grpc.WithTransportCredentials(creds), grpc.WithContextDialer(r.Dial))
		if err != nil {
			return nil, fmt.Errorf("kserve client: %w", err)
		}
		t := &grpcTransport{client: inference.NewGRPCInferenceServiceClient(conn)}
		if r.Token != "" {
			t.md = metadata.Pairs("authorization", "Bearer "+r.Token)
		}
		return newEmbedder(t, cfg, conn, r.Name)
	}
	return nil, fmt.Errorf("kserve transport %q is not grpc or rest", r.Transport)
}

// restTransport speaks the KServe v2 REST protocol: text as JSON BYTES strings, images with the
// binary tensor data extension (Triton and KServe), and outputs as JSON or binary data.
type restTransport struct {
	base   string
	client *http.Client
	token  string
}

type restTensor struct {
	Name       string         `json:"name"`
	Shape      []int64        `json:"shape,omitempty"`
	Datatype   string         `json:"datatype,omitempty"`
	Data       any            `json:"data,omitempty"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type restRequest struct {
	Inputs  []restTensor `json:"inputs"`
	Outputs []restTensor `json:"outputs"`
}

func (r *restTransport) modelPath(model, version string) string {
	p := r.base + "/v2/models/" + url.PathEscape(model)
	if version != "" {
		p += "/versions/" + url.PathEscape(version)
	}
	return p
}

func (r *restTransport) do(req *http.Request) (*http.Response, error) {
	if r.token != "" {
		req.Header.Set("Authorization", "Bearer "+r.token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	return resp, nil
}

func (r *restTransport) infer(ctx context.Context, model, version, input string, items [][]byte) ([]float32, []int64, error) {
	in := restTensor{Name: input, Shape: []int64{int64(len(items))}, Datatype: "BYTES"}
	body := &bytes.Buffer{}
	var binaryPart []byte
	if input == InputText {
		strs := make([]string, len(items))
		for i, it := range items {
			strs[i] = string(it)
		}
		in.Data = strs
	} else {
		for _, it := range items {
			binaryPart = binary.LittleEndian.AppendUint32(binaryPart, uint32(len(it))) //nolint:gosec // inputs are bounded (10 MB images)
			binaryPart = append(binaryPart, it...)
		}
		in.Parameters = map[string]any{"binary_data_size": len(binaryPart)}
	}
	header, err := json.Marshal(restRequest{Inputs: []restTensor{in}, Outputs: []restTensor{{Name: Output, Parameters: map[string]any{"binary_data": false}}}})
	if err != nil {
		return nil, nil, fmt.Errorf("encode request: %w", err)
	}
	body.Write(header)
	body.Write(binaryPart)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.modelPath(model, version)+"/infer", body)
	if err != nil {
		return nil, nil, fmt.Errorf("infer request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if binaryPart != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
		req.Header.Set("Inference-Header-Content-Length", strconv.Itoa(len(header)))
	}
	resp, err := r.do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("read response: %w", err)
	}
	switch {
	case resp.StatusCode == http.StatusBadRequest:
		return nil, nil, fmt.Errorf("%w: %s: %s", pipeline.ErrInvalidInput, resp.Status, snippet(raw))
	case resp.StatusCode != http.StatusOK:
		return nil, nil, fmt.Errorf("%s: %s", resp.Status, snippet(raw))
	}
	return decodeREST(raw, resp.Header.Get("Inference-Header-Content-Length"))
}

func decodeREST(raw []byte, headerLen string) ([]float32, []int64, error) {
	jsonPart, bin := raw, []byte(nil)
	if headerLen != "" {
		n, err := strconv.Atoi(headerLen)
		if err != nil || n < 0 || n > len(raw) {
			return nil, nil, fmt.Errorf("bad Inference-Header-Content-Length %q", headerLen)
		}
		jsonPart, bin = raw[:n], raw[n:]
	}
	var out struct {
		Outputs []struct {
			Name       string         `json:"name"`
			Datatype   string         `json:"datatype"`
			Shape      []int64        `json:"shape"`
			Data       []float32      `json:"data"`
			Parameters map[string]any `json:"parameters"`
		} `json:"outputs"`
	}
	if err := json.Unmarshal(jsonPart, &out); err != nil {
		return nil, nil, fmt.Errorf("decode response: %w", err)
	}
	offset := 0
	for _, o := range out.Outputs {
		size := 0
		if s, ok := o.Parameters["binary_data_size"].(float64); ok {
			size = int(s)
		}
		if o.Name != Output {
			offset += size
			continue
		}
		if o.Datatype != "FP32" {
			return nil, nil, fmt.Errorf("%s datatype %s, want FP32", Output, o.Datatype)
		}
		if size > 0 {
			if offset+size > len(bin) || size%4 != 0 {
				return nil, nil, fmt.Errorf("%s binary data is truncated", Output)
			}
			return littleEndianFloats(bin[offset : offset+size]), o.Shape, nil
		}
		return o.Data, o.Shape, nil
	}
	return nil, nil, fmt.Errorf("response has no %s output", Output)
}

func (r *restTransport) ready(ctx context.Context, model, version string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.modelPath(model, version)+"/ready", nil)
	if err != nil {
		return false, fmt.Errorf("ready request: %w", err)
	}
	resp, err := r.do(req)
	if err != nil {
		return false, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK, nil
}

func snippet(b []byte) string {
	const max = 256
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
