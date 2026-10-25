//go:build integration

// Package testinfra starts the infrastructure containers used by integration tests, pinned to
// the same images as deploy/docker-compose.yml.
package testinfra

import (
	"context"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcopensearch "github.com/testcontainers/testcontainers-go/modules/opensearch"
	tcqdrant "github.com/testcontainers/testcontainers-go/modules/qdrant"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	tcredpanda "github.com/testcontainers/testcontainers-go/modules/redpanda"
)

// Images, kept in step with deploy/docker-compose.yml.
const (
	OpenSearchImage = "opensearchproject/opensearch:3.9.0"
	QdrantImage     = "qdrant/qdrant:v1.20.0"
	RedpandaImage   = "redpandadata/redpanda:v26.2.6"
	MySQLImage      = "mysql:8.4.13"
	RedisImage      = "redis:7.4.11-alpine"
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

// MySQL starts MySQL, applies db/migrations/mysql/*.up.sql in order and returns a DSN.
func MySQL(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(here), "..", "..", "db", "migrations", "mysql")
	ups, err := filepath.Glob(filepath.Join(dir, "*.up.sql"))
	if err != nil || len(ups) == 0 {
		t.Fatalf("migrations in %s: %v", dir, err)
	}
	sort.Strings(ups)
	c, err := tcmysql.Run(ctx, MySQLImage,
		tcmysql.WithDatabase("searchengine"), tcmysql.WithUsername("search"), tcmysql.WithPassword("search"),
		tcmysql.WithScripts(ups...))
	terminate(t, c)
	if err != nil {
		t.Fatalf("start mysql: %v", err)
	}
	dsn, err := c.ConnectionString(ctx, "parseTime=true", "multiStatements=true")
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// Redis starts Redis and returns host:port.
func Redis(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := tcredis.Run(ctx, RedisImage)
	terminate(t, c)
	if err != nil {
		t.Fatalf("start redis: %v", err)
	}
	uri, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(uri, "redis://")
}
