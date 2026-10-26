// Command seed loads the prepared dataset (make data) for the demo tenant: it creates the
// tenant with the default config, uploads product images to S3 and publishes every product as
// a catalog.product.v1 event in the tenant's own storefront JSON shape, which the default
// field mapping translates back to the canonical schema. Re-running is idempotent.
package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
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
	"github.com/zavisoft/search-engine/internal/events"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

const service = "seed"

type config struct {
	LogLevel      string
	MySQLDSN      string
	KafkaBrokers  []string
	EventsTopic   string
	RequestsTopic string
	S3Endpoint    string
	S3Region      string
	S3AccessKey   string
	S3SecretKey   string
	S3Bucket      string
	S3PublicURL   string
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
		EventsTopic:   env.String("KAFKA_TOPIC_EVENTS", events.Topic),
		RequestsTopic: env.String("KAFKA_TOPIC_REQUESTS", events.RequestTopic),
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
	events     bool
	limit      int
}

func main() {
	var o options
	flag.StringVar(&o.dataDir, "data", "data", "dataset directory written by `make data`")
	flag.StringVar(&o.slug, "tenant", "demo", "tenant slug to create or reuse")
	flag.BoolVar(&o.skipImages, "skip-images", false, "do not upload images (they must already be in S3)")
	flag.BoolVar(&o.events, "events", false, "publish the simulated interaction events in data/events instead of the catalog")
	flag.IntVar(&o.limit, "limit", 0, "publish at most this many products or events (0 = all)")
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

	if o.events {
		return publishEvents(ctx, cfg.KafkaBrokers, cfg.EventsTopic, cfg.RequestsTopic, tid, o.dataDir, o.limit, log)
	}

	products, err := readProducts(filepath.Join(o.dataDir, "products.canonical.jsonl"), o.limit)
	if err != nil {
		return fmt.Errorf("%w (run `make data` first)", err)
	}

	imageURL := func(file string) string {
		return fmt.Sprintf("%s/%s/%s/%s", strings.TrimRight(cfg.S3PublicURL, "/"), cfg.S3Bucket, o.slug, file)
	}
	if !o.skipImages {
		if err := uploadImages(ctx, cfg, o, products, log); err != nil {
			return err
		}
	}
	if err := publish(ctx, cfg.KafkaBrokers, tid, products, imageURL, log); err != nil {
		return err
	}
	return publishVendors(ctx, cfg.KafkaBrokers, tid, filepath.Join(o.dataDir, "vendors.jsonl"), log)
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

// vendorRow is one row of data/vendors.jsonl (tools/dataprep/synthesize.py).
type vendorRow struct {
	VendorID           string  `json:"vendor_id"`
	OnTimeDispatchRate float64 `json:"on_time_dispatch_rate"`
	DisputeRate        float64 `json:"dispute_rate"`
	ResponseHours      float64 `json:"response_hours"`
	Verified           bool    `json:"verified"`
}

// vendorScore is the demo tenant's synthetic vendor score in [0, 1] (docs/DATA.md §4):
// 0.40·on_time + 0.25·(1 − min(dispute/0.1, 1)) + 0.15·(1 − min(hours/48, 1)) + 0.20·verified.
// Real tenants compute their own score; the platform only stores it (PLAN.md R9).
func vendorScore(onTime, dispute, responseHours float64, verified bool) float64 {
	score := 0.40*onTime + 0.25*(1-math.Min(dispute/0.1, 1)) + 0.15*(1-math.Min(responseHours/48, 1))
	if verified {
		score += 0.20
	}
	return math.Round(math.Min(math.Max(score, 0), 1)*1e4) / 1e4
}

// publishVendors publishes data/vendors.jsonl to catalog.vendor.v1; the indexer stores them as
// online vendor features.
func publishVendors(ctx context.Context, brokers []string, tid tenant.ID, path string, log *slog.Logger) error {
	f, err := os.Open(path) //nolint:gosec // operator-supplied dataset path
	if err != nil {
		return fmt.Errorf("open vendors: %w", err)
	}
	defer f.Close()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer cl.Close()
	ns := uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://github.com/zavisoft/search-engine/seed/vendors/"+string(tid)))
	var records []*kgo.Record
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var row vendorRow
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			return fmt.Errorf("%s line %d: %w", path, len(records)+1, err)
		}
		score := vendorScore(row.OnTimeDispatchRate, row.DisputeRate, row.ResponseHours, row.Verified)
		v := catalog.Vendor{ID: row.VendorID, Score: &score, OnTimeDispatchRate: &row.OnTimeDispatchRate,
			DisputeRate: &row.DisputeRate, ResponseHours: &row.ResponseHours, Verified: &row.Verified}
		e := catalog.NewVendorEvent(string(tid), v, time.Now())
		e.EventID = uuid.NewSHA1(ns, []byte(row.VendorID)).String()
		value, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("encode vendor event: %w", err)
		}
		r := &kgo.Record{Topic: catalog.TopicVendor, Key: e.Key(), Value: value}
		for k, h := range e.Headers() {
			r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(h)})
		}
		records = append(records, r)
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read vendors: %w", err)
	}
	if err := cl.ProduceSync(ctx, records...).FirstErr(); err != nil {
		return fmt.Errorf("produce vendors: %w", err)
	}
	log.Info("vendor events published", "tenant", tid, "topic", catalog.TopicVendor, "vendors", len(records))
	return nil
}

// publishEvents publishes the simulated interaction history (data/events) to the events topic,
// and its request log (data/events/requests.jsonl.gz) to the request topic. Timestamps are
// shifted so the newest event lands at the current time, keeping the 7- and 30-day feature
// windows populated whenever the stack is seeded.
func publishEvents(ctx context.Context, brokers []string, topic, requestTopic string, tid tenant.ID, dataDir string, limit int, log *slog.Logger) error {
	params, err := os.ReadFile(filepath.Join(dataDir, "simulation_params.json")) //nolint:gosec // operator-supplied dataset path
	if err != nil {
		return fmt.Errorf("%w (run `make data` first)", err)
	}
	var sim struct {
		RefDate time.Time `json:"ref_date"`
	}
	if err := json.Unmarshal(params, &sim); err != nil {
		return fmt.Errorf("simulation_params.json: %w", err)
	}
	if sim.RefDate.IsZero() {
		return errors.New("simulation_params.json: ref_date is missing")
	}
	shift := time.Now().UTC().Truncate(time.Second).Sub(sim.RefDate)
	files, err := filepath.Glob(filepath.Join(dataDir, "events", "part-*.jsonl.gz"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("no data/events/part-*.jsonl.gz (run `make data` first)")
	}

	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ProducerLinger(20*time.Millisecond))
	if err != nil {
		return fmt.Errorf("kafka client: %w", err)
	}
	defer cl.Close()
	var produceErr atomic.Pointer[error]
	start, total := time.Now(), 0
	event := func(line []byte) (*kgo.Record, error) {
		var e events.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		e.TS = e.TS.Add(shift)
		return events.Record(topic, tid, e)
	}
	for _, file := range files {
		n, err := publishFile(ctx, cl, file, event, limit-total, limit > 0, &produceErr)
		total += n
		if err != nil {
			return err
		}
		if limit > 0 && total >= limit {
			break
		}
		log.Info("events progress", "file", filepath.Base(file), "published", total)
	}
	request := func(line []byte) (*kgo.Record, error) {
		var r events.Request
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("decode request: %w", err)
		}
		r.TS = r.TS.Add(shift)
		return events.RequestRecord(requestTopic, tid, r)
	}
	requests, err := publishFile(ctx, cl, filepath.Join(dataDir, "events", "requests.jsonl.gz"), request, 0, false, &produceErr)
	if err != nil {
		return err
	}
	if err := cl.Flush(ctx); err != nil {
		return fmt.Errorf("flush events: %w", err)
	}
	if p := produceErr.Load(); p != nil {
		return fmt.Errorf("produce events: %w", *p)
	}
	log.Info("interaction events published", "tenant", tid, "topic", topic, "events", total, "requests", requests,
		"shift", shift.Round(time.Hour).String(), "took", time.Since(start).Round(time.Millisecond).String())
	return nil
}

// publishFile produces one record per line of a gzipped JSONL file, encoded by encode.
func publishFile(ctx context.Context, cl *kgo.Client, file string, encode func([]byte) (*kgo.Record, error),
	remaining int, limited bool, produceErr *atomic.Pointer[error]) (int, error) {
	f, err := os.Open(file) //nolint:gosec // operator-supplied dataset path
	if err != nil {
		return 0, fmt.Errorf("open events: %w", err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", file, err)
	}
	defer gz.Close()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() && (!limited || n < remaining) {
		r, err := encode(sc.Bytes())
		if err != nil {
			return n, fmt.Errorf("%s line %d: %w", file, n+1, err)
		}
		cl.Produce(ctx, r, func(_ *kgo.Record, err error) {
			if err != nil {
				produceErr.CompareAndSwap(nil, &err)
			}
		})
		n++
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("%s: %w", file, err)
	}
	return n, nil
}
