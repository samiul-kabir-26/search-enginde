// Command control-api is the control plane: tenant sign-up, config drafts and
// versions, models, connections, onboarding, index jobs and ads. It issues JWTs
// and serves the JWKS consumed by search-api.
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

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/controlplane"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
)

const service = "control-api"

type config struct {
	Addr            string
	LogLevel        string
	ShutdownTimeout time.Duration
	MySQLDSN        string
	RedisAddr       string
	JWTSigningKey   string
	JWTIssuer       string
	JWTTTL          time.Duration
	OTLPEndpoint    string
}

func loadConfig() (config, error) {
	env := platform.NewEnv()
	c := config{
		Addr:            env.String("CONTROL_API_ADDR", ":8081"),
		LogLevel:        env.String("LOG_LEVEL", "info"),
		ShutdownTimeout: env.Duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		MySQLDSN:        env.Required("MYSQL_DSN"),
		RedisAddr:       env.Required("REDIS_ADDR"),
		JWTSigningKey:   env.Required("JWT_SIGNING_KEY"),
		JWTIssuer:       env.Required("JWT_ISSUER"),
		JWTTTL:          env.Duration("JWT_TTL", 15*time.Minute),
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", platform.Healthz)
	mux.Handle("GET /readyz", platform.Readyz(map[string]platform.Check{
		"mysql": db.PingContext,
		"redis": func(ctx context.Context) error { return rdb.Ping(ctx).Err() },
	}, 2*time.Second))
	mux.Handle("GET /metrics", tel.MetricsHandler)

	signer, err := auth.NewSigner(cfg.JWTSigningKey, cfg.JWTIssuer)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	(&controlplane.Auth{Keys: tenant.NewStore(db), Signer: signer, TokenTTL: cfg.JWTTTL, Log: log}).Routes(mux)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           platform.Instrument(service, log, mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return platform.Serve(ctx, srv, cfg.ShutdownTimeout, log)
}
