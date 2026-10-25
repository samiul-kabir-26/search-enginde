// Command indexer consumes catalog.product.v1 events, maps products to the canonical schema,
// embeds them and upserts them to the lexical and vector stores.
//
// By default it runs as a consumer group writing to each tenant's active index. With
// -backfill -tenant <slug> it rebuilds that tenant's index from the start of the topic into a
// new version, verifies the counts and swaps the aliases.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/go-sql-driver/mysql"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/zavisoft/search-engine/internal/catalog/centroids"
	"github.com/zavisoft/search-engine/internal/catalog/indexer"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/lexical/opensearch"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/tenant"
)

const service = "indexer"

type config struct {
	Addr            string
	LogLevel        string
	ShutdownTimeout time.Duration
	MySQLDSN        string
	KafkaBrokers    []string
	OpenSearchURL   string
	QdrantAddr      string
	TritonAddr      string
	OTLPEndpoint    string
	ImageTimeout    time.Duration
	ImageFetchLimit int
	Parallelism     int
	S3Endpoint      string
	S3Region        string
	S3AccessKey     string
	S3SecretKey     string
	S3ModelsBucket  string
}

func loadConfig() (config, error) {
	env := platform.NewEnv()
	c := config{
		Addr:            env.String("INDEXER_ADDR", ":8082"),
		LogLevel:        env.String("LOG_LEVEL", "info"),
		ShutdownTimeout: env.Duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		MySQLDSN:        env.Required("MYSQL_DSN"),
		KafkaBrokers:    strings.Split(env.Required("KAFKA_BROKERS"), ","),
		OpenSearchURL:   env.Required("OPENSEARCH_URL"),
		QdrantAddr:      env.Required("QDRANT_ADDR"),
		TritonAddr:      env.Required("TRITON_GRPC_ADDR"),
		OTLPEndpoint:    env.String("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ImageTimeout:    env.Duration("INDEXER_IMAGE_TIMEOUT", 10*time.Second),
		ImageFetchLimit: env.Int("INDEXER_IMAGE_FETCH_LIMIT", 16),
		Parallelism:     env.Int("INDEXER_PARALLELISM", 4),
		S3Endpoint:      env.Required("S3_ENDPOINT"),
		S3Region:        env.String("S3_REGION", "us-east-1"),
		S3AccessKey:     env.Required("S3_ACCESS_KEY"),
		S3SecretKey:     env.Required("S3_SECRET_KEY"),
		S3ModelsBucket:  env.Required("S3_BUCKET_MODELS"),
	}
	return c, env.Err()
}

func main() {
	backfill := flag.Bool("backfill", false, "rebuild one tenant's index from the start of the topic")
	slug := flag.String("tenant", "", "tenant slug for -backfill")
	group := flag.String("group", "indexer", "consumer group for streaming mode")
	flag.Parse()
	if *backfill && *slug == "" {
		fmt.Fprintln(os.Stderr, "indexer: -backfill needs -tenant")
		os.Exit(2)
	}
	if err := run(context.Background(), *backfill, *slug, *group); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, backfill bool, slug, group string) error {
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
	triton, err := grpc.NewClient(cfg.TritonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)))
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

	tenants := tenant.NewStore(db)
	s3c := s3.New(s3.Options{
		Region: cfg.S3Region, BaseEndpoint: aws.String(cfg.S3Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	})
	builder := &indexer.Builder{Tenants: tenants, Triton: triton, OpenSearch: osClient, Qdrant: qClient,
		Centroids: &centroids.Store{Client: s3c, Bucket: cfg.S3ModelsBucket}}
	proc := indexer.NewProcessor(indexer.NewHTTPFetcher(cfg.ImageTimeout), log, cfg.ImageFetchLimit, cfg.Parallelism)

	if backfill {
		return runBackfill(ctx, cfg, tenants, slug, builder, proc, log)
	}

	consumer, err := indexer.NewConsumer(cfg.KafkaBrokers, group, builder, proc, log)
	if err != nil {
		return err
	}
	defer consumer.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", platform.Healthz)
	mux.Handle("GET /readyz", platform.Readyz(map[string]platform.Check{"mysql": db.PingContext}, 2*time.Second))
	mux.Handle("GET /metrics", tel.MetricsHandler)
	srv := &http.Server{Addr: cfg.Addr, Handler: platform.Instrument(service, log, mux), ReadHeaderTimeout: 5 * time.Second}

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return platform.Serve(gctx, srv, cfg.ShutdownTimeout, log) })
	g.Go(func() error {
		log.Info("consuming", "group", group, "brokers", cfg.KafkaBrokers)
		return consumer.Run(gctx)
	})
	if err := g.Wait(); err != nil {
		return fmt.Errorf("indexer: %w", err)
	}
	return nil
}

func runBackfill(ctx context.Context, cfg config, tenants *tenant.Store, slug string, b *indexer.Builder, p *indexer.Processor, log *slog.Logger) error {
	t, err := tenants.BySlug(ctx, slug)
	if err != nil {
		return err
	}
	rep, err := indexer.Backfill(ctx, cfg.KafkaBrokers, t.ID, b, p, log)
	log.Info("backfill finished",
		"tenant", slug, "version", rep.Version, "events", rep.Events,
		"upserted", len(rep.Result.UpsertedIDs), "deleted", len(rep.Result.DeletedIDs), "stale", rep.Result.Stale,
		"invalid", rep.Result.Invalid, "missing_image", rep.Result.MissingImage,
		"expected", rep.Expected, "lexical_count", rep.LexicalCount, "vector_count", rep.VectorCount, "centroids", rep.Centroids,
		"promoted", rep.Promoted, "took", rep.Duration.Round(time.Millisecond).String())
	return err
}
