//go:build integration

// Package e2e holds cross-service tests: control-api and search-api in one process against
// real MySQL and Redis.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-redis/redis_rate/v10"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/controlplane"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/searchapi"
	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/stages/filter"
	"github.com/zavisoft/search-engine/internal/stages/prerank"
	"github.com/zavisoft/search-engine/internal/stages/queryunderstanding"
	"github.com/zavisoft/search-engine/internal/stages/rank"
	"github.com/zavisoft/search-engine/internal/stages/rerank"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

type fakeProviders struct{ p *pipeline.Providers }

func (f fakeProviders) For(context.Context, *tenant.Resolved) (*pipeline.Providers, error) {
	return f.p, nil
}

func post(t *testing.T, url, token string, body any) map[string]any {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	method := http.MethodPost
	if s, ok := body.(string); ok && s == "GET" {
		method, rd = http.MethodGet, nil
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		t.Fatalf("%s %s: %d %s", method, url, resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// TestActivationChangesSearchWithinFiveSeconds is M4's acceptance test: publishing and
// activating v2 through control-api changes search-api results within 5 s, without a restart.
func TestActivationChangesSearchWithinFiveSeconds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db, err := sql.Open("mysql", testinfra.MySQL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rdb := redis.NewClient(&redis.Options{Addr: testinfra.Redis(t)})
	defer rdb.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sealer, _ := security.NewSealer(base64.StdEncoding.EncodeToString(key))
	q := tenantdb.New(db)
	keys := auth.NewKeyRing(db, sealer, "http://issuer", log)
	if err := keys.Load(ctx); err != nil {
		t.Fatal(err)
	}
	settings := controlplane.NewSettings(q, rdb, log)
	store := tenant.NewStore(db)
	tid, err := store.Create(ctx, tenant.NewTenant{Slug: "demo", Name: "Demo", Plan: "growth", Region: "local", Document: tenant.DefaultDocument()})
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("correct horse battery staple", auth.DefaultPasswordParams())
	if err := q.CreatePlatformAdmin(ctx, tenantdb.CreatePlatformAdminParams{ID: tenant.NewID().String(), Email: "root@platform.test", PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}

	cp := &controlplane.Server{DB: db, Q: q, Tenants: store, Keys: keys, Verifier: auth.NewLocalVerifier(keys, "http://issuer"),
		Settings: settings, Mailer: controlplane.SMTPMailer{Addr: "127.0.0.1:1"}, Redis: rdb, Idem: controlplane.NewIdempotency(rdb),
		PlatformEmbedder: registry.PlatformEmbedderInfo, HTTPClient: http.DefaultClient, Log: log}
	cpMux := http.NewServeMux()
	cp.Routes(cpMux)
	control := httptest.NewServer(cpMux)
	defer control.Close()

	b := stagestest.Breakers()
	engine, _ := pipeline.NewEngine(noop.NewMeterProvider().Meter("e2e"),
		queryunderstanding.New(b), retrieval.New(b), filter.New(b, nil), prerank.New(b, nil), rank.New(b), rerank.New())
	providers, _, _ := stagestest.Providers()
	resolver := tenant.NewResolver(store, 10*time.Minute)
	svc := &searchapi.Service{Resolver: resolver, Providers: fakeProviders{providers}, Engine: engine,
		Snapshots: searchapi.NewSnapshots(rdb), Cursors: searchapi.NewCursors([]byte("k")), Limiter: redis_rate.NewLimiter(rdb), Log: log}
	sMux := http.NewServeMux()
	svc.Routes(sMux, auth.NewLocalVerifier(keys, "http://issuer"))
	search := httptest.NewServer(sMux)
	defer search.Close()
	go resolver.Watch(ctx, rdb, 30*time.Second, log, nil)
	time.Sleep(200 * time.Millisecond)

	searchToken, err := keys.IssueClaims(auth.Claims{TenantID: string(tid), Scope: auth.ScopeSearchRead}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	brands := func() (int, map[string]bool) {
		out := post(t, search.URL+"/v1/search", searchToken, map[string]any{"q": "leather", "page": map[string]int{"size": 20}})
		seen := map[string]bool{}
		for _, r := range out["results"].([]any) {
			seen[r.(map[string]any)["product"].(map[string]any)["brand"].(string)] = true
		}
		return int(out["config_version"].(float64)), seen
	}
	v, before := brands()
	if v != 1 || len(before) < 2 {
		t.Fatalf("v1 baseline: version %d brands %v", v, before)
	}

	admin := post(t, control.URL+"/v1/admin/login", "", map[string]string{"email": "root@platform.test", "password": "correct horse battery staple"})["access_token"].(string)
	base := control.URL + "/v1/tenants/" + string(tid)
	draft := post(t, base+"/config/draft", admin, "GET")["document"].(map[string]any)
	policy := draft["policy"].(map[string]any)
	policy["always_filters"] = append(policy["always_filters"].([]any), map[string]any{"field": "brand", "op": "eq", "value": "Rivet"})
	req, _ := json.Marshal(draft)
	r, _ := http.NewRequestWithContext(ctx, http.MethodPut, base+"/config/draft", bytes.NewReader(req))
	r.Header.Set("Authorization", "Bearer "+admin)
	resp, err := http.DefaultClient.Do(r)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("put draft: %v %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	ver := post(t, base+"/config/versions", admin, nil)["version"].(float64)

	activated := time.Now()
	post(t, base+"/config/versions/"+itoa(int(ver))+"/activate", admin, nil)
	for {
		v, after := brands()
		if v == int(ver) {
			if len(after) != 1 || !after["Rivet"] {
				t.Fatalf("v2 must only return Rivet: %v", after)
			}
			t.Logf("search served v%d %v after %v", v, after, time.Since(activated).Round(time.Millisecond))
			break
		}
		if time.Since(activated) > 5*time.Second {
			t.Fatalf("still serving v%d %v 5 s after activation", v, after)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }
