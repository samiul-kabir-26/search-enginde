// Command seed loads the prepared dataset (make data) for the demo tenant: it creates the
// tenant with the default config, uploads product images to S3 and publishes every product as
// a catalog.product.v1 event in the tenant's own storefront JSON shape, which the default
// field mapping translates back to the canonical schema. Re-running is idempotent.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"golang.org/x/sync/errgroup"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

const service = "seed"

type config struct {
	LogLevel     string
	MySQLDSN     string
	KafkaBrokers []string
	S3Endpoint   string
	S3Region     string
	S3AccessKey  string
	S3SecretKey  string
	S3Bucket     string
	S3PublicURL  string
	// AdminEmail and AdminPassword bootstrap the first platform admin (PLAN.md R7).
	AdminEmail    string
	AdminPassword string
}

func loadConfig() (config, error) {
	env := platform.NewEnv()
	c := config{
		LogLevel:      env.String("LOG_LEVEL", "info"),
		MySQLDSN:      env.Required("MYSQL_DSN"),
		KafkaBrokers:  strings.Split(env.Required("KAFKA_BROKERS"), ","),
		S3Endpoint:    env.Required("S3_ENDPOINT"),
		S3Region:      env.String("S3_REGION", "us-east-1"),
		S3AccessKey:   env.Required("S3_ACCESS_KEY"),
		S3SecretKey:   env.Required("S3_SECRET_KEY"),
		S3Bucket:      env.Required("S3_BUCKET_IMAGES"),
		S3PublicURL:   env.Required("S3_PUBLIC_URL"),
		AdminEmail:    env.String("PLATFORM_ADMIN_EMAIL", ""),
		AdminPassword: env.String("PLATFORM_ADMIN_PASSWORD", ""),
	}
	return c, env.Err()
}

type options struct {
	dataDir    string
	slug       string
	skipImages bool
	limit      int
}

func main() {
	var o options
	flag.StringVar(&o.dataDir, "data", "data", "dataset directory written by `make data`")
	flag.StringVar(&o.slug, "tenant", "demo", "tenant slug to create or reuse")
	flag.BoolVar(&o.skipImages, "skip-images", false, "do not upload images (they must already be in S3)")
	flag.IntVar(&o.limit, "limit", 0, "publish at most this many products (0 = all)")
	flag.Parse()
	if err := run(context.Background(), o); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o options) error {
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

	products, err := readProducts(filepath.Join(o.dataDir, "products.canonical.jsonl"), o.limit)
	if err != nil {
		return fmt.Errorf("%w (run `make data` first)", err)
	}

	db, err := sql.Open("mysql", cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("mysql: %w", err)
	}
	defer db.Close()
	if err := ensurePlatformAdmin(ctx, db, cfg.AdminEmail, cfg.AdminPassword, log); err != nil {
		return err
	}
	store := tenant.NewStore(db)
	tid, err := ensureTenant(ctx, store, o.slug, log)
	if err != nil {
		return err
	}
	if err := ensureAPIKey(ctx, store, tid, filepath.Join(o.dataDir, "demo_api_key"), log); err != nil {
		return err
	}

	imageURL := func(file string) string {
		return fmt.Sprintf("%s/%s/%s/%s", strings.TrimRight(cfg.S3PublicURL, "/"), cfg.S3Bucket, o.slug, file)
	}
	if !o.skipImages {
		if err := uploadImages(ctx, cfg, o, products, log); err != nil {
			return err
		}
	}
	return publish(ctx, cfg.KafkaBrokers, tid, products, imageURL, log)
}

// canonicalProduct is one row of data/products.canonical.jsonl (tools/dataprep/synthesize.py).
type canonicalProduct struct {
	ProductID    string              `json:"product_id"`
	VendorID     string              `json:"vendor_id"`
	Title        string              `json:"title"`
	Description  string              `json:"description"`
	Brand        string              `json:"brand"`
	CategoryPath []string            `json:"category_path"`
	ProductType  string              `json:"product_type"`
	Attributes   map[string][]string `json:"attributes"`
	PriceMinor   int64               `json:"price_minor"`
	Currency     string              `json:"currency"`
	MOQ          int32               `json:"moq"`
	Stock        int32               `json:"stock"`
	ShipRegions  []string            `json:"ship_regions"`
	Status       string              `json:"status"`
	Rating       *float64            `json:"rating"`
	RatingCount  int32               `json:"rating_count"`
	ImageFiles   []string            `json:"image_files"`
	CreatedAt    string              `json:"created_at"`
}

func readProducts(path string, limit int) ([]canonicalProduct, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied dataset path
	if err != nil {
		return nil, fmt.Errorf("open dataset: %w", err)
	}
	defer f.Close()
	var out []canonicalProduct
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var p canonicalProduct
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, len(out)+1, err)
		}
		out = append(out, p)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return out, nil
}

func ensureTenant(ctx context.Context, store *tenant.Store, slug string, log *slog.Logger) (tenant.ID, error) {
	t, err := store.BySlug(ctx, slug)
	if err == nil {
		log.Info("tenant exists", "tenant", t.ID, "slug", slug)
		return t.ID, nil
	}
	if !errors.Is(err, tenant.ErrNotFound) {
		return "", err
	}
	id, err := store.Create(ctx, tenant.NewTenant{
		Slug: slug, Name: "Demo Marketplace", Plan: "growth", Region: "local", Document: tenant.DefaultDocument(),
	})
	if err != nil {
		return "", err
	}
	log.Info("tenant created", "tenant", id, "slug", slug, "config_version", 1)
	return id, nil
}

// ensurePlatformAdmin creates the first platform admin when none exists.
func ensurePlatformAdmin(ctx context.Context, db *sql.DB, email, password string, log *slog.Logger) error {
	q := tenantdb.New(db)
	n, err := q.CountPlatformAdmins(ctx)
	if err != nil {
		return fmt.Errorf("count platform admins: %w", err)
	}
	if n > 0 {
		return nil
	}
	if email == "" || password == "" {
		log.Warn("no platform admin exists; set PLATFORM_ADMIN_EMAIL and PLATFORM_ADMIN_PASSWORD and rerun make seed")
		return nil
	}
	if err := auth.CheckPasswordPolicy(password, 12); err != nil {
		return fmt.Errorf("PLATFORM_ADMIN_PASSWORD: %w", err)
	}
	hash, err := auth.HashPassword(password, auth.DefaultPasswordParams())
	if err != nil {
		return err
	}
	if err := q.CreatePlatformAdmin(ctx, tenantdb.CreatePlatformAdminParams{ID: tenant.NewID().String(), Email: strings.ToLower(email), PasswordHash: hash}); err != nil {
		return fmt.Errorf("create platform admin: %w", err)
	}
	log.Info("platform admin created", "email", email)
	return nil
}

// ensureAPIKey creates a demo API key with every data-plane scope when the tenant has none
// (or the saved key file is gone) and writes it to path, readable only by the owner.
func ensureAPIKey(ctx context.Context, store *tenant.Store, tid tenant.ID, path string, log *slog.Logger) error {
	n, err := store.CountActiveAPIKeys(ctx, tid)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); n > 0 && statErr == nil {
		log.Info("demo API key exists", "file", path)
		return nil
	}
	key, prefix, hash, err := auth.NewAPIKey()
	if err != nil {
		return err
	}
	if err := store.CreateAPIKey(ctx, tenant.APIKey{
		ID: tenant.NewID().String(), TenantID: tid, Prefix: prefix, Hash: hash,
		Scopes: []string{auth.ScopeSearchRead, auth.ScopeSearchDebug, auth.ScopeEventsWrite},
	}); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return fmt.Errorf("write api key: %w", err)
	}
	log.Info("demo API key created", "file", path, "prefix", prefix)
	return nil
}

func uploadImages(ctx context.Context, cfg config, o options, products []canonicalProduct, log *slog.Logger) error {
	client := s3.New(s3.Options{
		Region:       cfg.S3Region,
		BaseEndpoint: aws.String(cfg.S3Endpoint),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, ""),
	})
	var uploaded, skipped atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(16)
	for _, p := range products {
		for _, file := range p.ImageFiles {
			g.Go(func() error {
				key := o.slug + "/" + file
				_, err := client.HeadObject(gctx, &s3.HeadObjectInput{Bucket: &cfg.S3Bucket, Key: &key})
				if err == nil {
					skipped.Add(1)
					return nil
				}
				var nf *s3types.NotFound
				if !errors.As(err, &nf) {
					return fmt.Errorf("head %s: %w", key, err)
				}
				f, err := os.Open(filepath.Join(o.dataDir, "images", file)) //nolint:gosec // dataset path
				if err != nil {
					return fmt.Errorf("open image: %w", err)
				}
				defer f.Close()
				_, err = client.PutObject(gctx, &s3.PutObjectInput{
					Bucket: &cfg.S3Bucket, Key: &key, Body: f, ContentType: aws.String("image/jpeg"),
				})
				if err != nil {
					return fmt.Errorf("put %s: %w", key, err)
				}
				uploaded.Add(1)
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("upload images: %w", err)
	}
	log.Info("images in S3", "bucket", cfg.S3Bucket, "uploaded", uploaded.Load(), "already_present", skipped.Load())
	return nil
}

// storefrontJSON renders p in the demo tenant's own catalog shape, the one the default
// `catalog.field_mapping` reads (internal/tenant/default_config.json).
func storefrontJSON(p canonicalProduct, imageURL func(string) string) ([]byte, error) {
	images := make([]map[string]string, len(p.ImageFiles))
	for i, f := range p.ImageFiles {
		images[i] = map[string]string{"url": imageURL(f)}
	}
	doc := map[string]any{
		"id":           p.ProductID,
		"name":         p.Title,
		"description":  p.Description,
		"category":     map[string]any{"path": p.CategoryPath},
		"brand":        p.Brand,
		"product_type": p.ProductType,
		"price":        map[string]any{"amount_minor": p.PriceMinor, "currency": p.Currency},
		"images":       images,
		"vendor":       map[string]any{"id": p.VendorID},
		"attributes":   p.Attributes,
		"moq":          p.MOQ,
		"inventory":    map[string]any{"stock": p.Stock},
		"shipping":     map[string]any{"regions": p.ShipRegions},
		"status":       p.Status,
		"reviews":      map[string]any{"rating": p.Rating, "count": p.RatingCount},
		"created_at":   p.CreatedAt,
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("storefront json %s: %w", p.ProductID, err)
	}
	return b, nil
}

// seedSourceVersion orders seed events. Re-publishing the same dataset re-applies the same
// version, which the indexer treats as an idempotent replay.
const seedSourceVersion = 1

func publish(ctx context.Context, brokers []string, tid tenant.ID, products []canonicalProduct, imageURL func(string) string, log *slog.Logger) error {
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ProducerLinger(20*time.Millisecond))
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer cl.Close()
	ns := uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/zavisoft/search-engine/seed/"+string(tid)))
	start := time.Now()
	const batch = 500
	for i := 0; i < len(products); i += batch {
		var records []*kgo.Record
		for _, p := range products[i:min(i+batch, len(products))] {
			body, err := storefrontJSON(p, imageURL)
			if err != nil {
				return err
			}
			e := catalog.ProductEvent{
				EventID:       uuid.NewSHA1(ns, fmt.Appendf(nil, "%s@%d", p.ProductID, seedSourceVersion)).String(),
				TenantID:      string(tid),
				ProductID:     p.ProductID,
				Op:            catalog.OpUpsert,
				TS:            time.Now().UTC(),
				SourceVersion: seedSourceVersion,
				Product:       body,
			}
			value, err := json.Marshal(e)
			if err != nil {
				return fmt.Errorf("encode event: %w", err)
			}
			r := &kgo.Record{Topic: catalog.Topic, Key: e.Key(), Value: value}
			for k, v := range e.Headers() {
				r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
			}
			records = append(records, r)
		}
		if err := cl.ProduceSync(ctx, records...).FirstErr(); err != nil {
			return fmt.Errorf("produce: %w", err)
		}
	}
	log.Info("catalog events published", "tenant", tid, "topic", catalog.Topic, "events", len(products), "took", time.Since(start).Round(time.Millisecond).String())
	return nil
}
