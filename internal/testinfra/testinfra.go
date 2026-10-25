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
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcopensearch "github.com/testcontainers/testcontainers-go/modules/opensearch"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	tcqdrant "github.com/testcontainers/testcontainers-go/modules/qdrant"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	tcredpanda "github.com/testcontainers/testcontainers-go/modules/redpanda"
	tcvault "github.com/testcontainers/testcontainers-go/modules/vault"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Images, kept in step with deploy/docker-compose.yml.
const (
	OpenSearchImage = "opensearchproject/opensearch:3.9.0"
	QdrantImage     = "qdrant/qdrant:v1.20.0"
	RedpandaImage   = "redpandadata/redpanda:v26.2.6"
	MySQLImage      = "mysql:8.4.13"
	RedisImage      = "redis:7.4.11-alpine"
	// BYO vector stores and secrets (M5); not part of the Compose stack.
	PgvectorImage = "pgvector/pgvector:0.8.7-pg17-trixie"
	MilvusImage   = "milvusdb/milvus:v2.6.25"
	VaultImage    = "hashicorp/vault:2.1.2"
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

// Pgvector starts PostgreSQL with the vector extension enabled and returns a DSN
// (postgres://search:search@host:port/byo?sslmode=disable).
func Pgvector(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := tcpostgres.Run(ctx, PgvectorImage, tcpostgres.WithDatabase("byo"), tcpostgres.WithUsername("search"),
		tcpostgres.WithPassword("search"), tcpostgres.BasicWaitStrategies())
	terminate(t, c)
	if err != nil {
		t.Fatalf("start pgvector: %v", err)
	}
	if _, _, err := c.Exec(ctx, []string{"psql", "-U", "search", "-d", "byo", "-c", "CREATE EXTENSION IF NOT EXISTS vector"}); err != nil {
		t.Fatalf("create extension: %v", err)
	}
	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	return dsn
}

// Milvus starts a standalone Milvus with embedded etcd and local storage (the recipe of
// Milvus's standalone_embed.sh) and returns its gRPC host:port. It needs about 1.5 GB of memory.
func Milvus(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	etcd := "listen-client-urls: http://0.0.0.0:2379\nadvertise-client-urls: http://0.0.0.0:2379\n"
	c, err := testcontainers.Run(ctx, MilvusImage,
		testcontainers.WithExposedPorts("19530/tcp", "9091/tcp"),
		testcontainers.WithEnv(map[string]string{
			"ETCD_USE_EMBED": "true", "ETCD_DATA_DIR": "/var/lib/milvus/etcd", "ETCD_CONFIG_PATH": "/milvus/configs/embedEtcd.yaml",
			"COMMON_STORAGETYPE": "local", "DEPLOY_MODE": "STANDALONE",
		}),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			ContainerFilePath: "/milvus/configs/embedEtcd.yaml", Reader: strings.NewReader(etcd), FileMode: 0o644,
		}),
		testcontainers.WithCmd("milvus", "run", "standalone"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/healthz").WithPort("9091/tcp").WithStartupTimeout(3*time.Minute).WithPollInterval(time.Second),
			wait.ForListeningPort("19530/tcp").WithStartupTimeout(3*time.Minute),
		))
	terminate(t, c)
	if err != nil {
		t.Fatalf("start milvus: %v", err)
	}
	addr, err := c.PortEndpoint(ctx, "19530/tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	return addr
}

// Vault starts Vault in dev mode (KV v2 at secret/) with root token "root" and returns its
// HTTP address.
func Vault(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	c, err := tcvault.Run(ctx, VaultImage, tcvault.WithToken("root"))
	terminate(t, c)
	if err != nil {
		t.Fatalf("start vault: %v", err)
	}
	addr, err := c.HttpHostAddress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
