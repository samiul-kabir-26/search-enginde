package qdrant

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc"

	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
)

// DialBYO returns a client for a tenant's Qdrant gRPC endpoint (https://host[:6334]). No
// connection is made until the first call. dial is the egress dialer; the target uses the
// passthrough resolver so the dialer sees the host name (for the allowlist) and resolves it
// itself on every dial. plaintext is allowed only for platform-trusted hosts (tests, dev).
func DialBYO(endpoint, apiKey string, plaintext bool, dial func(context.Context, string) (net.Conn, error)) (*qdrant.Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return nil, fmt.Errorf("qdrant endpoint %q is not a URL", endpoint)
	}
	if u.Scheme != "https" && !plaintext {
		return nil, fmt.Errorf("qdrant endpoint %q must be https", endpoint)
	}
	port := 6334
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return nil, fmt.Errorf("qdrant endpoint port %q: %w", p, err)
		}
	}
	c, err := qdrant.NewClient(&qdrant.Config{
		Host:                   "passthrough:///" + u.Hostname(),
		Port:                   port,
		APIKey:                 apiKey,
		UseTLS:                 u.Scheme == "https",
		TLSConfig:              &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12},
		GrpcOptions:            []grpc.DialOption{grpc.WithContextDialer(dial)},
		SkipCompatibilityCheck: true,
		PoolSize:               1,
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant client: %w", err)
	}
	return c, nil
}

var metricNames = map[qdrant.Distance]string{
	qdrant.Distance_Cosine: vectorstore.MetricCosine,
	qdrant.Distance_Dot:    vectorstore.MetricDot,
	qdrant.Distance_Euclid: vectorstore.MetricL2,
}

// Metrics are the metrics a BYO Qdrant collection may use.
func (s *Store) Metrics() []string {
	return []string{vectorstore.MetricCosine, vectorstore.MetricDot, vectorstore.MetricL2}
}

// Describe reports the collection's named vectors; Qdrant payloads are schemaless, so any
// field can be stored and filtered on.
func (s *Store) Describe(ctx context.Context) (vectorstore.Info, error) {
	exists, err := s.client.CollectionExists(ctx, s.target)
	if err != nil {
		return vectorstore.Info{}, fmt.Errorf("check %s: %w", s.target, err)
	}
	if !exists {
		return vectorstore.Info{}, nil
	}
	ci, err := s.client.GetCollectionInfo(ctx, s.target)
	if err != nil {
		return vectorstore.Info{}, fmt.Errorf("describe %s: %w", s.target, err)
	}
	info := vectorstore.Info{Exists: true, AnyField: true, Vectors: map[string]vectorstore.VectorInfo{}}
	vc := ci.GetConfig().GetParams().GetVectorsConfig()
	for name, p := range vc.GetParamsMap().GetMap() {
		info.Vectors[name] = vectorInfo(p)
	}
	if p := vc.GetParams(); p != nil {
		info.Vectors[""] = vectorInfo(p)
	}
	return info, nil
}

func vectorInfo(p *qdrant.VectorParams) vectorstore.VectorInfo {
	m, ok := metricNames[p.GetDistance()]
	if !ok {
		m = p.GetDistance().String()
	}
	return vectorstore.VectorInfo{Dim: int(p.GetSize()), Metric: m} //nolint:gosec // vector sizes are small
}
