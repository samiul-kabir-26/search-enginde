// Command control-api is the control plane: tenant sign-up, users and API keys, config drafts
// and versions, onboarding, catalog uploads, index jobs, the playground proxy and the platform
// admin API. It issues JWTs and serves the JWKS consumed by search-api.
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

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/errgroup"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/controlplane"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/scorer/artifacts"
	"github.com/zavisoft/search-engine/internal/providers/scorer/onnx"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

const service = "control-api"

type config struct {
	Addr             string
	LogLevel         string
	AppEnv           string
	ShutdownTimeout  time.Duration
	MySQLDSN         string
	RedisAddr        string
	JWTIssuer        string
	SecretsMasterKey string
	SMTPAddr         string
	MailFrom         string
	AppBaseURL       string
	SearchURL        string
	S3PublicURL      string
	S3Region         string
	S3AccessKey      string
	S3SecretKey      string
	S3UploadBucket   string
	S3Endpoint       string
	S3ModelsBucket   string
	ONNXRuntimeLib   string
	OTLPEndpoint     string
	Secrets          security.SecretBackend
	EgressTrusted    string
	ClickHouseAddr   string
	ClickHouseDB     string
	ClickHouseUser   string
	ClickHousePass   string
}

func loadConfig() (config, error) {
	env := platform.NewEnv()
	c := config{
		Addr:             env.String("CONTROL_API_ADDR", ":8081"),
		LogLevel:         env.String("LOG_LEVEL", "info"),
		AppEnv:           env.String("APP_ENV", "dev"),
		ShutdownTimeout:  env.Duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		MySQLDSN:         env.Required("MYSQL_DSN"),
		RedisAddr:        env.Required("REDIS_ADDR"),
		JWTIssuer:        env.Required("JWT_ISSUER"),
		SecretsMasterKey: env.Required("SECRETS_MASTER_KEY"),
		SMTPAddr:         env.Required("SMTP_ADDR"),
		MailFrom:         env.String("MAIL_FROM", "search-console@localhost"),
		AppBaseURL:       env.String("APP_BASE_URL", "http://localhost:3000"),
		SearchURL:        env.Required("SEARCH_API_URL"),
		S3PublicURL:      env.Required("S3_PUBLIC_URL"),
		S3Region:         env.String("S3_REGION", "us-east-1"),
		S3AccessKey:      env.Required("S3_ACCESS_KEY"),
		S3SecretKey:      env.Required("S3_SECRET_KEY"),
		S3UploadBucket:   env.Required("S3_BUCKET_UPLOADS"),
		S3Endpoint:       env.Required("S3_ENDPOINT"),
		S3ModelsBucket:   env.Required("S3_BUCKET_MODELS"),
		ONNXRuntimeLib:   env.String("ONNXRUNTIME_LIB", ""),
		OTLPEndpoint:     env.String("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		Secrets:          security.SecretBackendFromEnv(env),
		EgressTrusted:    env.String("EGRESS_TRUSTED_HOSTS", ""),
		ClickHouseAddr:   env.String("CLICKHOUSE_ADDR", ""),
		ClickHouseDB:     env.String("CLICKHOUSE_DB", "searchengine"),
		ClickHouseUser:   env.String("CLICKHOUSE_USER", "default"),
		ClickHousePass:   env.String("CLICKHOUSE_PASSWORD", ""),
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

	sealer, err := security.NewSealer(cfg.SecretsMasterKey)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	q := tenantdb.New(db)
	secrets, err := security.NewSecretStore(cfg.Secrets, q)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tenants := tenant.NewStore(db)
	byo := &registry.BYO{Source: tenants, Secrets: secrets, Timeout: 10 * time.Second,
		Egress: security.NewEgress(security.EgressConfig{Trusted: security.Hosts(cfg.EgressTrusted)})}
	settings := controlplane.NewSettings(q, rdb, log)
	if err := settings.Load(ctx); err != nil {
		return err
	}
	keys := auth.NewKeyRing(db, sealer, cfg.JWTIssuer, log)
	if err := keys.Load(ctx); err != nil {
		return err
	}
	presign := s3.NewPresignClient(s3.New(s3.Options{
		Region: cfg.S3Region, BaseEndpoint: aws.String(cfg.S3PublicURL), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	}))
	// Uploaded ONNX scorers are dry-run at registration; without the runtime they are refused.
	onnxReady := true
	if err := onnx.Init(cfg.ONNXRuntimeLib); err != nil {
		onnxReady = false
		log.Warn("ONNX Runtime unavailable; ONNX model uploads are disabled", "err", err)
	}
	models := &artifacts.Store{Bucket: cfg.S3ModelsBucket, Client: s3.New(s3.Options{
		Region: cfg.S3Region, BaseEndpoint: aws.String(cfg.S3Endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	})}
	var chConn driver.Conn
	if cfg.ClickHouseAddr != "" {
		c, err := clickhouse.Open(&clickhouse.Options{
			Addr: []string{cfg.ClickHouseAddr},
			Auth: clickhouse.Auth{
				Database: cfg.ClickHouseDB,
				Username: cfg.ClickHouseUser,
				Password: cfg.ClickHousePass,
			},
		})
		if err == nil {
			chConn = c
			defer chConn.Close()
		} else {
			log.Warn("clickhouse connection failed; ads reports will be unavailable", "err", err)
		}
	}
	srv := &controlplane.Server{
		DB: db, Q: q, Tenants: tenants, Keys: keys, Verifier: auth.NewLocalVerifier(keys, cfg.JWTIssuer),
		Settings: settings, Mailer: controlplane.SMTPMailer{Addr: cfg.SMTPAddr, From: cfg.MailFrom}, Redis: rdb,
		Idem: controlplane.NewIdempotency(rdb), Presign: presign, UploadBucket: cfg.S3UploadBucket,
		SearchURL: cfg.SearchURL, HTTPClient: &http.Client{Timeout: 10 * time.Second},
		AppBaseURL: cfg.AppBaseURL, SecureCookies: cfg.AppEnv != "dev",
		PlatformEmbedder: registry.PlatformEmbedderInfo, BYO: byo, Models: models, ModelBucket: cfg.S3ModelsBucket,
		ONNXReady: onnxReady, ClickHouse: chConn, Log: log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", platform.Healthz)
	mux.Handle("GET /readyz", platform.Readyz(map[string]platform.Check{
		"mysql": db.PingContext,
		"redis": func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
	}, 2*time.Second))
	mux.Handle("GET /metrics", tel.MetricsHandler)
	srv.Routes(mux)

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           platform.Instrument(service, log, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { settings.Watch(gctx); return nil })
	g.Go(func() error {
		keys.Run(gctx, time.Minute, func() (time.Duration, time.Duration) {
			a := settings.Get().Auth
			return time.Duration(a.KeyRotation), time.Duration(a.KeyRetention)
		})
		return nil
	})
	g.Go(func() error { return platform.Serve(gctx, httpSrv, cfg.ShutdownTimeout, log) })
	if err := g.Wait(); err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}
