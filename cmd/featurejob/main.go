// Command featurejob aggregates ClickHouse events into Redis online features on a schedule
// (M6). It also refreshes per-tenant category centroids, which absorb streaming catalog
// updates between re-indexes (PLAN.md R6):
//
//	featurejob centroids -tenant demo
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/go-sql-driver/mysql"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/catalog/centroids"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/tenant"
)

const service = "featurejob"

func main() {
	if len(os.Args) < 2 || os.Args[1] != "centroids" {
		fmt.Fprintln(os.Stderr, "usage: featurejob centroids -tenant <slug>   (online features arrive in M6, see PLAN.md)")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("centroids", flag.ExitOnError)
	slug := fs.String("tenant", "", "tenant slug")
	_ = fs.Parse(os.Args[2:])
	if *slug == "" {
		fmt.Fprintln(os.Stderr, "featurejob centroids: -tenant is required")
		os.Exit(2)
	}
	if err := runCentroids(context.Background(), *slug); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func runCentroids(ctx context.Context, slug string) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	env := platform.NewEnv()
	logLevel := env.String("LOG_LEVEL", "info")
	dsn := env.Required("MYSQL_DSN")
	qdrantAddr := env.Required("QDRANT_ADDR")
	endpoint, region := env.Required("S3_ENDPOINT"), env.String("S3_REGION", "us-east-1")
	access, secret, bucket := env.Required("S3_ACCESS_KEY"), env.Required("S3_SECRET_KEY"), env.Required("S3_BUCKET_MODELS")
	if err := env.Err(); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log, err := platform.NewLogger(os.Stdout, service, logLevel)
	if err != nil {
		return err
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	defer db.Close()
	t, err := tenant.NewStore(db).BySlug(ctx, slug)
	if err != nil {
		return err
	}
	qc, err := qdrant.NewClient(qdrantAddr)
	if err != nil {
		return err
	}
	defer qc.Close()
	reg, _ := catalog.NewRegistry(nil)
	vs := qdrant.New(qc, t.ID, nil, reg)
	v, err := vs.Active(ctx)
	if err != nil {
		return err
	}
	if v == 0 {
		return fmt.Errorf("tenant %s has no active index", slug)
	}
	idx, err := centroids.BuildFrom(ctx, vs, v)
	if err != nil {
		return err
	}
	store := &centroids.Store{Client: s3.New(s3.Options{
		Region: region, BaseEndpoint: aws.String(endpoint), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(access, secret, ""),
	}), Bucket: bucket}
	m, err := store.Save(ctx, string(t.ID), v, idx)
	if err != nil {
		return err
	}
	log.Info("centroids saved", "tenant", slug, "index_version", v, "categories", m.Categories, "dim", m.Dim, "sha256", m.SHA256)
	return nil
}
