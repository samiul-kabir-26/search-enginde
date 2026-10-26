// Command featurejob aggregates ClickHouse events into Redis online features on a schedule
// (PLAN.md M6, R9): every FEATUREJOB_INTERVAL (default 10 min, so features are at most ~15 min
// old) for every tenant with an active config. It also refreshes per-tenant category centroids
// every FEATUREJOB_CENTROIDS_INTERVAL (default 1 h), absorbing streaming catalog updates
// between re-indexes (PLAN.md R6):
//
//	featurejob                         # scheduled, every live tenant
//	featurejob -once -tenant demo      # one feature run for one tenant
//	featurejob centroids -tenant demo  # one centroid refresh
//	featurejob dataset -tenant demo -out data/training/demo.csv.gz  # training dataset (M7)
//	featurejob publish -dir tools/training/out                     # publish trained scorers
package main

import (
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/go-sql-driver/mysql"
	"github.com/qdrant/go-client/qdrant"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/metric/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/catalog/centroids"
	"github.com/zavisoft/search-engine/internal/featurejob"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/features"
	"github.com/zavisoft/search-engine/internal/providers/lexical/opensearch"
	"github.com/zavisoft/search-engine/internal/providers/scorer/artifacts"
	qdrantstore "github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/stages/queryunderstanding"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/tenant"
)

const service = "featurejob"

type config struct {
	LogLevel          string
	MySQLDSN          string
	RedisAddr         string
	ClickHouse        clickhouse.Auth
	ClickHouseAddr    string
	OpenSearchURL     string
	QdrantAddr        string
	TritonAddr        string
	S3Endpoint        string
	S3Region          string
	S3AccessKey       string
	S3SecretKey       string
	S3ModelsBucket    string
	Interval          time.Duration
	CentroidsInterval time.Duration
}

func loadConfig() (config, error) {
	env := platform.NewEnv()
	c := config{
		LogLevel:       env.String("LOG_LEVEL", "info"),
		MySQLDSN:       env.Required("MYSQL_DSN"),
		RedisAddr:      env.Required("REDIS_ADDR"),
		ClickHouseAddr: env.Required("CLICKHOUSE_ADDR"),
		ClickHouse: clickhouse.Auth{
			Database: env.String("CLICKHOUSE_DB", "searchengine"),
			Username: env.String("CLICKHOUSE_USER", "default"), Password: env.String("CLICKHOUSE_PASSWORD", ""),
		},
		OpenSearchURL:     env.Required("OPENSEARCH_URL"),
		QdrantAddr:        env.Required("QDRANT_ADDR"),
		TritonAddr:        env.String("TRITON_GRPC_ADDR", "localhost:8001"),
		S3Endpoint:        env.Required("S3_ENDPOINT"),
		S3Region:          env.String("S3_REGION", "us-east-1"),
		S3AccessKey:       env.Required("S3_ACCESS_KEY"),
		S3SecretKey:       env.Required("S3_SECRET_KEY"),
		S3ModelsBucket:    env.Required("S3_BUCKET_MODELS"),
		Interval:          env.Duration("FEATUREJOB_INTERVAL", 10*time.Minute),
		CentroidsInterval: env.Duration("FEATUREJOB_CENTROIDS_INTERVAL", time.Hour),
	}
	return c, env.Err()
}

func main() {
	var err error
	sub := ""
	if len(os.Args) > 1 {
		sub = os.Args[1]
	}
	switch sub {
	case "centroids":
		fs := flag.NewFlagSet("centroids", flag.ExitOnError)
		slug := fs.String("tenant", "", "tenant slug")
		_ = fs.Parse(os.Args[2:])
		if *slug == "" {
			fmt.Fprintln(os.Stderr, "featurejob centroids: -tenant is required")
			os.Exit(2)
		}
		err = run(context.Background(), *slug, modeCentroids)
	case "dataset":
		fs := flag.NewFlagSet("dataset", flag.ExitOnError)
		slug := fs.String("tenant", "demo", "tenant slug")
		out := fs.String("out", "", "output CSV (gzipped when it ends in .gz)")
		maxReq := fs.Int("max-requests", 20000, "sample at most this many requests (0 = all)")
		conc := fs.Int("concurrency", 8, "requests replayed concurrently")
		_ = fs.Parse(os.Args[2:])
		if *out == "" {
			fmt.Fprintln(os.Stderr, "featurejob dataset: -out is required")
			os.Exit(2)
		}
		err = runDataset(context.Background(), *slug, *out, *maxReq, *conc)
	case "publish":
		fs := flag.NewFlagSet("publish", flag.ExitOnError)
		dir := fs.String("dir", "tools/training/out", "directory of trained models (<id>/model.onnx, <id>/features.json)")
		repo := fs.String("triton-repo", "deploy/triton/model_repository", "Triton model repository for kserve_v2 scorers")
		_ = fs.Parse(os.Args[2:])
		err = runPublish(context.Background(), *dir, *repo)
	default:
		slug := flag.String("tenant", "", "tenant slug (default: every tenant with an active config)")
		once := flag.Bool("once", false, "run the feature aggregation once and exit")
		flag.Parse()
		mode := modeScheduled
		if *once {
			mode = modeOnce
		}
		err = run(context.Background(), *slug, mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

type mode int

const (
	modeScheduled mode = iota
	modeOnce
	modeCentroids
)

// app holds the clients shared by feature and centroid runs.
type app struct {
	log       *slog.Logger
	tenants   *tenant.Store
	job       *featurejob.Job
	qdrant    *qdrant.Client
	centroids *centroids.Store
}

func run(ctx context.Context, slug string, m mode) error {
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
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	defer db.Close()
	qc, err := qdrantstore.NewClient(cfg.QdrantAddr)
	if err != nil {
		return err
	}
	defer qc.Close()
	a := &app{log: log, tenants: tenant.NewStore(db), qdrant: qc, centroids: &centroids.Store{Client: s3.New(s3.Options{
		Region: cfg.S3Region, BaseEndpoint: aws.String(cfg.S3Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	}), Bucket: cfg.S3ModelsBucket}}
	if m == modeCentroids {
		t, err := a.tenants.BySlug(ctx, slug)
		if err != nil {
			return err
		}
		return a.refreshCentroids(ctx, t.ID)
	}

	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer rdb.Close()
	ch, err := clickhouse.Open(&clickhouse.Options{Addr: []string{cfg.ClickHouseAddr}, Auth: cfg.ClickHouse})
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer ch.Close()
	osClient, err := opensearch.NewClient(cfg.OpenSearchURL)
	if err != nil {
		return err
	}
	noFields, _ := catalog.NewRegistry(nil)
	a.job = &featurejob.Job{
		CH: ch, Store: features.New(rdb), Log: log, Now: time.Now,
		Docs: func(tid tenant.ID) featurejob.DocSource { return opensearch.New(osClient, tid, noFields) },
	}

	if m == modeOnce {
		return a.features(ctx, slug)
	}
	log.Info("featurejob scheduled", "interval", cfg.Interval, "centroids_interval", cfg.CentroidsInterval)
	featTick, centTick := time.NewTicker(cfg.Interval), time.NewTicker(cfg.CentroidsInterval)
	defer featTick.Stop()
	defer centTick.Stop()
	a.logErr("feature run", a.features(ctx, slug))
	a.logErr("centroid refresh", a.allCentroids(ctx, slug))
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-featTick.C:
			a.logErr("feature run", a.features(ctx, slug))
		case <-centTick.C:
			a.logErr("centroid refresh", a.allCentroids(ctx, slug))
		}
	}
}

func (a *app) logErr(what string, err error) {
	if err != nil {
		a.log.Error(what+" failed", "err", err)
	}
}

// targets returns the slug's tenant, or every tenant with an active config.
func (a *app) targets(ctx context.Context, slug string) ([]tenant.ID, error) {
	if slug != "" {
		t, err := a.tenants.BySlug(ctx, slug)
		if err != nil {
			return nil, err
		}
		return []tenant.ID{t.ID}, nil
	}
	live, err := a.tenants.LiveVersions(ctx)
	if err != nil {
		return nil, err
	}
	var out []tenant.ID
	seen := map[tenant.ID]bool{}
	for _, lv := range live {
		if !lv.Shadow && !seen[lv.Tenant] {
			seen[lv.Tenant] = true
			out = append(out, lv.Tenant)
		}
	}
	return out, nil
}

// features runs the aggregation for each tenant; one tenant's failure does not stop the others.
func (a *app) features(ctx context.Context, slug string) error {
	ids, err := a.targets(ctx, slug)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		rep, err := a.job.Run(ctx, id)
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", id, err))
			continue
		}
		a.log.Info("online features refreshed", "tenant", id, "items", rep.Items, "users", rep.Users,
			"eta", rep.Eta, "eta_source", rep.EtaSource, "randomized_clicks", rep.RandomizedClicks,
			"took", rep.Took.Round(time.Millisecond).String())
	}
	return errors.Join(errs...)
}

func (a *app) allCentroids(ctx context.Context, slug string) error {
	ids, err := a.targets(ctx, slug)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := a.refreshCentroids(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// refreshCentroids rebuilds the centroids of the tenant's active platform vector index.
// Tenants with a BYO vector store have no platform index and are skipped.
func (a *app) refreshCentroids(ctx context.Context, id tenant.ID) error {
	reg, _ := catalog.NewRegistry(nil)
	vs := qdrantstore.New(a.qdrant, id, nil, reg)
	v, err := vs.Active(ctx)
	if err != nil {
		return err
	}
	if v == 0 {
		a.log.Info("no active platform vector index; centroids skipped", "tenant", id)
		return nil
	}
	idx, err := centroids.BuildFrom(ctx, vs, v)
	if err != nil {
		return err
	}
	m, err := a.centroids.Save(ctx, string(id), v, idx)
	if err != nil {
		return err
	}
	a.log.Info("centroids saved", "tenant", id, "index_version", v, "categories", m.Categories, "dim", m.Dim, "sha256", m.SHA256)
	return nil
}

// runDataset writes a tenant's learning-to-rank dataset (docs/MODELS.md §5), replaying logged
// requests through S1 and S2 of its active config.
func runDataset(ctx context.Context, slug, out string, maxRequests, concurrency int) error {
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
	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	defer db.Close()
	store := tenant.NewStore(db)
	t, err := store.BySlug(ctx, slug)
	if err != nil {
		return err
	}
	res, err := tenant.NewResolver(store, time.Minute).Get(ctx, t.ID)
	if err != nil {
		return err
	}
	rdb := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	defer rdb.Close()
	ch, err := clickhouse.Open(&clickhouse.Options{Addr: []string{cfg.ClickHouseAddr}, Auth: cfg.ClickHouse})
	if err != nil {
		return fmt.Errorf("clickhouse: %w", err)
	}
	defer ch.Close()
	triton, err := grpc.NewClient(cfg.TritonAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("triton: %w", err)
	}
	defer triton.Close()
	osClient, err := opensearch.NewClient(cfg.OpenSearchURL)
	if err != nil {
		return err
	}
	qc, err := qdrantstore.NewClient(cfg.QdrantAddr)
	if err != nil {
		return err
	}
	defer qc.Close()
	s3c := s3.New(s3.Options{
		Region: cfg.S3Region, BaseEndpoint: aws.String(cfg.S3Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	})
	reg := registry.New(registry.Platform{Triton: triton, OpenSearch: osClient, Qdrant: qc,
		Centroids: &centroids.Store{Client: s3c, Bucket: cfg.S3ModelsBucket}}, registry.Options{}, log)
	providers, err := reg.For(ctx, res)
	if err != nil {
		return err
	}
	// Offline replay is not latency-bound: breakers only stop a store that is really down.
	settings := resilience.DefaultSettings()
	settings.ConsecutiveFailures, settings.MinRequests = 50, 200
	breakers, err := resilience.NewBreakers(noop.NewMeterProvider().Meter(service), settings)
	if err != nil {
		return err
	}
	noFields, _ := catalog.NewRegistry(nil)
	fs := features.New(rdb)
	d := &featurejob.Dataset{
		CH: ch, Understand: queryunderstanding.New(breakers), Retrieve: retrieval.New(breakers), Vendors: fs,
		MaxRequests: maxRequests, Concurrency: concurrency, Log: log,
		Job: &featurejob.Job{CH: ch, Log: log, Now: time.Now,
			Docs: func(tid tenant.ID) featurejob.DocSource { return opensearch.New(osClient, tid, noFields) }},
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
		return fmt.Errorf("output dir: %w", err)
	}
	f, err := os.Create(out) //nolint:gosec // operator-supplied output path
	if err != nil {
		return fmt.Errorf("create %s: %w", out, err)
	}
	defer f.Close()
	var w io.Writer = f
	var gz *gzip.Writer
	if strings.HasSuffix(out, ".gz") {
		gz = gzip.NewWriter(f)
		w = gz
	}
	rep, err := d.Build(ctx, res, providers, w)
	if err != nil {
		return err
	}
	if gz != nil {
		if err := gz.Close(); err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
	}
	log.Info("dataset written", "tenant", slug, "out", out, "rows", rep.Rows, "train", rep.Requests[featurejob.SplitTrain],
		"valid", rep.Requests[featurejob.SplitValid], "test", rep.Requests[featurejob.SplitTest], "skipped", rep.Skipped,
		"missing", rep.Missing, "cutoff", rep.Cutoff.Format(time.RFC3339), "took", rep.Took.Round(time.Second).String())
	return nil
}

// runPublish uploads trained platform scorers (tools/training) to the models bucket, after
// checking each model against its manifest, and writes the Triton model directory of kserve_v2
// scorers (restart Triton to load a new version).
func runPublish(ctx context.Context, dir, repo string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log, err := platform.NewLogger(os.Stdout, service, cfg.LogLevel)
	if err != nil {
		return err
	}
	store := &artifacts.Store{Bucket: cfg.S3ModelsBucket, Client: s3.New(s3.Options{
		Region: cfg.S3Region, BaseEndpoint: aws.String(cfg.S3Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	})}
	for id, ps := range artifacts.Platform {
		model, err := os.ReadFile(filepath.Join(dir, id, "model.onnx")) //nolint:gosec // operator-supplied directory
		if errors.Is(err, os.ErrNotExist) {
			log.Info("no trained model; skipped", "model", id)
			continue
		}
		if err != nil {
			return fmt.Errorf("read model %s: %w", id, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, id, "features.json")) //nolint:gosec // operator-supplied directory
		if err != nil {
			return fmt.Errorf("read manifest %s: %w", id, err)
		}
		var m artifacts.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("%s manifest: %w", id, err)
		}
		if m.ID != id || m.Type != ps.Type || m.SHA256 != artifacts.SHA256(model) || len(m.Features) == 0 {
			return fmt.Errorf("%s: the manifest does not match the model (id, type %s, sha256, features)", id, ps.Type)
		}
		if err := store.Put(ctx, artifacts.PlatformModelKey(id), model); err != nil {
			return err
		}
		if err := store.Put(ctx, artifacts.PlatformManifestKey(id), raw); err != nil {
			return err
		}
		if ps.Triton != "" {
			if err := writeTritonModel(filepath.Join(repo, ps.Triton), ps.Triton, model, len(m.Features)); err != nil {
				return err
			}
		}
		log.Info("platform scorer published", "model", id, "type", m.Type, "features", len(m.Features), "sha256", m.SHA256[:16],
			"metrics", m.Metrics)
	}
	return nil
}

// writeTritonModel writes a Triton ONNX-backend model: FEATURES FP32 [N, F] → SCORES FP32 [N]
// (docs/MODELS.md §1), one CPU instance.
func writeTritonModel(dir, name string, model []byte, features int) error {
	// Triton reads the repository from a read-only mount, possibly as another user.
	if err := os.MkdirAll(filepath.Join(dir, "1"), 0o755); err != nil { //nolint:gosec // Triton must read it
		return fmt.Errorf("triton model dir: %w", err)
	}
	config := fmt.Sprintf(`name: %q
backend: "onnxruntime"
max_batch_size: 0
input [ { name: "FEATURES", data_type: TYPE_FP32, dims: [ -1, %d ] } ]
output [ { name: "SCORES", data_type: TYPE_FP32, dims: [ -1 ] } ]
instance_group [ { kind: KIND_CPU, count: 1 } ]
parameters { key: "intra_op_thread_count" value: { string_value: "2" } }
`, name, features)
	if err := os.WriteFile(filepath.Join(dir, "config.pbtxt"), []byte(config), 0o644); err != nil { //nolint:gosec // Triton must read it
		return fmt.Errorf("triton config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "1", "model.onnx"), model, 0o644); err != nil { //nolint:gosec // Triton must read it
		return fmt.Errorf("triton model: %w", err)
	}
	return nil
}
