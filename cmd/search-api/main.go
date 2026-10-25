// Command search-api is the data plane: text, image and similar-item search,
// plus event ingestion.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/go-redis/redis_rate/v10"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/catalog/centroids"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/lexical/opensearch"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/searchapi"
	"github.com/zavisoft/search-engine/internal/stages/filter"
	"github.com/zavisoft/search-engine/internal/stages/prerank"
	"github.com/zavisoft/search-engine/internal/stages/queryunderstanding"
	"github.com/zavisoft/search-engine/internal/stages/rank"
	"github.com/zavisoft/search-engine/internal/stages/rerank"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/tenant"
)

const service = "search-api"

type config struct {
	Addr            string
	LogLevel        string
	ShutdownTimeout time.Duration
	MySQLDSN        string
	RedisAddr       string
	OpenSearchURL   string
	QdrantAddr      string
	TritonAddr      string
	S3Endpoint      string
	S3Region        string
	S3AccessKey     string
	S3SecretKey     string
	S3ModelsBucket  string
	JWKSURL         string
	JWTIssuer       string
	CursorSecret    string
	ConfigTTL       time.Duration
	OTLPEndpoint    string
}

func loadConfig() (config, error) {
	env := platform.NewEnv()
	c := config{
		Addr:            env.String("SEARCH_API_ADDR", ":8080"),
		LogLevel:        env.String("LOG_LEVEL", "info"),
		ShutdownTimeout: env.Duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		MySQLDSN:        env.Required("MYSQL_DSN"),
		RedisAddr:       env.Required("REDIS_ADDR"),
		OpenSearchURL:   env.Required("OPENSEARCH_URL"),
		QdrantAddr:      env.Required("QDRANT_ADDR"),
		TritonAddr:      env.Required("TRITON_GRPC_ADDR"),
		S3Endpoint:      env.Required("S3_ENDPOINT"),
		S3Region:        env.String("S3_REGION", "us-east-1"),
		S3AccessKey:     env.Required("S3_ACCESS_KEY"),
		S3SecretKey:     env.Required("S3_SECRET_KEY"),
		S3ModelsBucket:  env.Required("S3_BUCKET_MODELS"),
		JWKSURL:         env.Required("JWKS_URL"),
		JWTIssuer:       env.Required("JWT_ISSUER"),
		CursorSecret:    env.Required("SEARCH_CURSOR_SECRET"),
		ConfigTTL:       env.Duration("TENANT_CONFIG_TTL", 30*time.Second),
		OTLPEndpoint:    env.String("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
	}
	return c, env.Err()
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log, err := platform.NewLogger(os.Stdout, service, cfg.LogLevel)
	if err != nil {
		return err
	}
	tel, err := platform.SetupTelemetry(ctx, service, cfg.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer func() {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := tel.Shutdown(sctx); err != nil {
			log.Error("telemetry shutdown", "err", err)
		}
	}()

	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	defer db.Close()
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer rdb.Close()
	triton, err := grpc.NewClient(cfg.TritonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("triton: %w", err)
	}
	defer triton.Close()
	osClient, err := opensearch.NewClient(cfg.OpenSearchURL)
	if err != nil {
		return err
	}
	qClient, err := qdrant.NewClient(cfg.QdrantAddr)
	if err != nil {
		return err
	}
	defer qClient.Close()
	s3c := s3.New(s3.Options{
		Region: cfg.S3Region, BaseEndpoint: aws.String(cfg.S3Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	})

	meter := otel.Meter("search-api")
	breakers, err := resilience.NewBreakers(meter, resilience.DefaultSettings())
	if err != nil {
		return err
	}
	engine, err := pipeline.NewEngine(meter,
		queryunderstanding.New(breakers), retrieval.New(breakers), filter.New(),
		prerank.New(breakers), rank.New(breakers), rerank.New())
	if err != nil {
		return err
	}
	store := tenant.NewStore(db)
	verifier := auth.NewVerifier(cfg.JWKSURL, cfg.JWTIssuer, log)
	if err := verifier.Refresh(ctx); err != nil {
		log.Warn("initial JWKS fetch failed; retrying on first request", "err", err)
	}
	svc := &searchapi.Service{
		Resolver: tenant.NewResolver(store, cfg.ConfigTTL),
		Providers: registry.New(registry.Platform{
			Triton: triton, OpenSearch: osClient, Qdrant: qClient,
			Centroids: &centroids.Store{Client: s3c, Bucket: cfg.S3ModelsBucket},
		}, 2*time.Minute, log),
		Engine:    engine,
		Snapshots: searchapi.NewSnapshots(rdb),
		Cursors:   searchapi.NewCursors([]byte(cfg.CursorSecret)),
		Limiter:   redis_rate.NewLimiter(rdb),
		Log:       log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", platform.Healthz)
	mux.Handle("GET /readyz", platform.Readyz(map[string]platform.Check{
		"mysql": db.PingContext,
		"redis": func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
	}, 2*time.Second))
	mux.Handle("GET /metrics", tel.MetricsHandler)
	svc.Routes(mux, verifier)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           platform.Instrument(service, log, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { verifier.Run(gctx, 10*time.Minute); return nil })
	g.Go(func() error { return platform.Serve(gctx, srv, cfg.ShutdownTimeout, log) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
