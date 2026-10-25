//go:build integration

// Package testinfra starts the infrastructure containers used by integration tests, pinned to
// the same images as deploy/docker-compose.yml.
package testinfra

import (
	"context"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcopensearch "github.com/testcontainers/testcontainers-go/modules/opensearch"
	tcqdrant "github.com/testcontainers/testcontainers-go/modules/qdrant"
	tcredpanda "github.com/testcontainers/testcontainers-go/modules/redpanda"
)

// Images, kept in step with deploy/docker-compose.yml.
const (
	OpenSearchImage = "opensearchproject/opensearch:3.9.0"
	QdrantImage     = "qdrant/qdrant:v1.20.0"
	RedpandaImage   = "redpandadata/redpanda:v26.2.6"
)

func terminate(t *testing.T, c testcontainers.Container) {
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(c); err != nil {
			t.Logf("terminate container: %v", err)
		}
	})
}

// OpenSearch starts a single-node cluster with security disabled and returns its URL.
func OpenSearch(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := tcopensearch.Run(ctx, OpenSearchImage,
		testcontainers.WithEnv(map[string]string{
			"DISABLE_SECURITY_PLUGIN":     "true",
			"DISABLE_INSTALL_DEMO_CONFIG": "true",
			"OPENSEARCH_JAVA_OPTS":        "-Xms512m -Xmx512m",
		}))
	terminate(t, c)
	if err != nil {
		t.Fatalf("start opensearch: %v", err)
	}
	addr, err := c.Address(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// Qdrant starts Qdrant and returns its gRPC host:port.
func Qdrant(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := tcqdrant.Run(ctx, QdrantImage)
	terminate(t, c)
	if err != nil {
		t.Fatalf("start qdrant: %v", err)
	}
	addr, err := c.GRPCEndpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// Redpanda starts a single Redpanda broker with topic auto-creation and returns its seed broker.
func Redpanda(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := tcredpanda.Run(ctx, RedpandaImage, tcredpanda.WithAutoCreateTopics())
	terminate(t, c)
	if err != nil {
		t.Fatalf("start redpanda: %v", err)
	}
	addr, err := c.KafkaSeedBroker(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
