package searchapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis_rate/v10"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/features"
	"github.com/zavisoft/search-engine/internal/stages/filter"
	"github.com/zavisoft/search-engine/internal/stages/prerank"
	"github.com/zavisoft/search-engine/internal/stages/queryunderstanding"
	"github.com/zavisoft/search-engine/internal/stages/rank"
	"github.com/zavisoft/search-engine/internal/stages/rerank"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/stages/stagestest"
	"github.com/zavisoft/search-engine/internal/tenant"
)

type fixedResolver struct{ res *tenant.Resolved }

func (f fixedResolver) Get(_ context.Context, id tenant.ID) (*tenant.Resolved, error) {
	if id != f.res.Tenant.ID {
		return nil, tenant.ErrNotFound
	}
	return f.res, nil
}

func (f fixedResolver) Pinned(_ context.Context, id tenant.ID, ref string) (*tenant.Resolved, error) {
	if id != f.res.Tenant.ID || (ref != "draft" && ref != "7") {
		return nil, tenant.ErrNotFound
	}
	pinned := *f.res
	pinned.ConfigVersion, pinned.Draft = 7, ref == "draft"
	if pinned.Draft {
		pinned.ConfigVersion = 0
	}
	return &pinned, nil
}

type fixedProviders struct{ p *pipeline.Providers }

func (f fixedProviders) For(context.Context, *tenant.Resolved) (*pipeline.Providers, error) {
	return f.p, nil
}

// countingEngine counts pipeline runs so snapshot reuse can be asserted.
type countingEngine struct {
	*pipeline.Engine
	runs int
}

func (c *countingEngine) Run(ctx context.Context, res *tenant.Resolved, p *pipeline.Providers, q *pipeline.Query) (*pipeline.State, *pipeline.Trace) {
	c.runs++
	return c.Engine.Run(ctx, res, p, q)
}

type harness struct {
	svc      *Service
	srv      *httptest.Server
	signer   *auth.Signer
	engine   *countingEngine
	res      *tenant.Resolved
	redis    *miniredis.Miniredis
	features *features.Store
	events   *fakeSink
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	b := stagestest.Breakers()
	fs := features.New(rdb)
	e, err := pipeline.NewEngine(noop.NewMeterProvider().Meter("t"),
		queryunderstanding.New(b), retrieval.New(b), filter.New(), prerank.New(b, fs), rank.New(b), rerank.New())
	if err != nil {
		t.Fatal(err)
	}
	res := stagestest.Resolved()
	res.Plan.MaxPageDepth = 5
	p, _, _ := stagestest.Providers()
	seed := make([]byte, 32)
	signer, _ := auth.NewSigner(base64.StdEncoding.EncodeToString(seed), "iss")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &harness{signer: signer, engine: &countingEngine{Engine: e}, res: res, redis: mr, features: fs, events: &fakeSink{}}
	svc := &Service{
		Resolver: fixedResolver{res}, Providers: fixedProviders{p}, Engine: h.engine,
		Snapshots: NewSnapshots(rdb), Cursors: NewCursors([]byte("k")), Limiter: redis_rate.NewLimiter(rdb),
		Events: h.events, Vendors: fs, Log: log,
	}
	h.svc = svc
	mux := http.NewServeMux()
	svc.Routes(mux, auth.NewVerifier("http://unused", "iss", log).WithKeys(signer.JWKS()))
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

func (h *harness) token(t *testing.T, scopes ...string) string {
	t.Helper()
	tok, err := h.signer.Issue(h.res.Tenant.ID, "test", scopes, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func (h *harness) search(t *testing.T, tok, body string) (int, SearchResponse, platform.Problem) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, h.srv.URL+"/v1/search", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var sr SearchResponse
	var pr platform.Problem
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(b, &sr); err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
	} else {
		_ = json.Unmarshal(b, &pr)
	}
	return resp.StatusCode, sr, pr
}

func TestSearchPagingModesShareOneSnapshot(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeSearchRead)
	code, first, _ := h.search(t, tok, `{"q":"red leather bag","page":{"size":2}}`)
	if code != http.StatusOK {
		t.Fatalf("code %d", code)
	}
	if first.Total == 0 || first.Total > 5 || len(first.Results) != 2 || first.NextCursor == nil || first.Trace != nil {
		t.Fatalf("first page = %+v", first)
	}
	if first.Results[0].Position != 1 || first.Results[0].Product == nil || first.Facets["brand"] == nil {
		t.Errorf("result shape = %+v facets=%v", first.Results[0], first.Facets)
	}

	_, byCursor, _ := h.search(t, tok, `{"q":"red leather bag","page":{"size":2,"cursor":"`+*first.NextCursor+`"}}`)
	_, byNumber, _ := h.search(t, tok, `{"q":"Red  Leather bag","page":{"size":2,"number":2}}`)
	_, byOffset, _ := h.search(t, tok, `{"q":"red leather bag","page":{"size":2,"offset":2}}`)
	if h.engine.runs != 1 {
		t.Errorf("pipeline ran %d times; paging must reuse the snapshot", h.engine.runs)
	}
	for name, p := range map[string]SearchResponse{"cursor": byCursor, "number": byNumber, "offset": byOffset} {
		if len(p.Results) == 0 || p.Results[0].ProductID != byCursor.Results[0].ProductID || p.Results[0].Position != 3 || p.Offset != 2 {
			t.Errorf("%s page = %+v", name, p.Results)
		}
		if p.Results[0].Product == nil {
			t.Errorf("%s page must be hydrated from the lexical index", name)
		}
	}
	seen := map[string]bool{}
	for _, r := range append(first.Results, byCursor.Results...) {
		if seen[r.ProductID] {
			t.Errorf("duplicate %s across pages", r.ProductID)
		}
		seen[r.ProductID] = true
	}
	_, beyond, _ := h.search(t, tok, `{"q":"red leather bag","page":{"size":2,"offset":50}}`)
	if len(beyond.Results) != 0 || beyond.NextCursor != nil {
		t.Errorf("past the end = %+v", beyond)
	}
}

func TestSearchCursorValidation(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeSearchRead)
	_, first, _ := h.search(t, tok, `{"q":"red bag","page":{"size":1}}`)
	other := NewCursors([]byte("k")).Encode(string(h.res.Tenant.ID), Cursor{Snapshot: "deadbeef", Offset: 1, ConfigVersion: 3})
	for name, cur := range map[string]string{
		"tampered":    strings.Replace(*first.NextCursor, ".", "x.", 1),
		"other query": other,
		"garbage":     "abc",
	} {
		code, _, pr := h.search(t, tok, `{"q":"red bag","page":{"cursor":"`+cur+`"}}`)
		if code != http.StatusBadRequest || len(pr.Errors) == 0 || pr.Errors[0].Code != "invalid_cursor" {
			t.Errorf("%s: code %d %+v", name, code, pr)
		}
	}
	h.redis.FlushAll()
	code, _, pr := h.search(t, tok, `{"q":"red bag","page":{"cursor":"`+*first.NextCursor+`"}}`)
	if code != http.StatusBadRequest || pr.Errors[0].Code != "invalid_cursor" {
		t.Errorf("expired snapshot: code %d %+v", code, pr)
	}
}

func TestSearchValidation(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeSearchRead)
	cases := map[string]struct{ body, path, code string }{
		"empty q":       {`{"q":"  "}`, "/q", "required"},
		"long q":        {`{"q":"` + strings.Repeat("a", 513) + `"}`, "/q", "too_long"},
		"unknown field": {`{"q":"x","sort":"price"}`, "", "invalid_json"},
		"two modes":     {`{"q":"x","page":{"number":1,"offset":0}}`, "/page", "conflict"},
		"page size":     {`{"q":"x","page":{"size":101}}`, "/page/size", "out_of_range"},
		"bad filter":    {`{"q":"x","filters":{"field":"title","op":"eq","value":"y"}}`, "/filters/field", "not_filterable"},
	}
	for name, tc := range cases {
		code, _, pr := h.search(t, tok, tc.body)
		ok := code == http.StatusBadRequest
		found := false
		for _, e := range pr.Errors {
			found = found || (e.Path == tc.path && e.Code == tc.code)
		}
		if !ok || !found {
			t.Errorf("%s: code %d errors %+v", name, code, pr.Errors)
		}
	}
}

func TestSearchFiltersAndExplain(t *testing.T) {
	h := newHarness(t)
	_, plain, _ := h.search(t, h.token(t, auth.ScopeSearchRead), `{"q":"leather","filters":{"field":"brand","op":"eq","value":"Rivet"},"explain":true}`)
	if plain.Trace != nil {
		t.Error("explain needs search:debug")
	}
	for _, r := range plain.Results {
		if r.Product.Brand != "Rivet" {
			t.Errorf("filter not applied: %+v", r.Product)
		}
	}
	_, dbg, _ := h.search(t, h.token(t, auth.ScopeSearchRead, auth.ScopeSearchDebug), `{"q":"leather","explain":true}`)
	if dbg.Trace == nil || len(dbg.Trace.Stages) != 6 || dbg.Trace.Stages[1].Candidates == nil {
		t.Errorf("trace = %+v", dbg.Trace)
	}
	if dbg.Query.Normalized != "leather" || dbg.Query.Attributes["material"] == nil {
		t.Errorf("query info = %+v", dbg.Query)
	}
}

func TestSearchRateLimit(t *testing.T) {
	h := newHarness(t)
	h.res.Plan.SearchRPS, h.res.Plan.SearchBurst = 1, 1
	tok := h.token(t, auth.ScopeSearchRead)
	limited := false
	for range 5 {
		code, _, _ := h.search(t, tok, `{"q":"bag"}`)
		if code == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("the plan's rate limit was not enforced")
	}
}

func TestCursorCodec(t *testing.T) {
	c := NewCursors([]byte("secret"))
	s := c.Encode("T1", Cursor{Snapshot: "abc", Offset: 24, ConfigVersion: 2})
	got, err := c.Decode("T1", s)
	if err != nil || got.Offset != 24 || got.Snapshot != "abc" {
		t.Fatalf("decode = %+v %v", got, err)
	}
	if _, err := c.Decode("T2", s); err == nil {
		t.Error("a cursor must not be valid for another tenant")
	}
	if _, err := NewCursors([]byte("other")).Decode("T1", s); err == nil {
		t.Error("a cursor signed with another key must be rejected")
	}
	var buf bytes.Buffer
	buf.WriteString(s)
	if _, err := c.Decode("T1", buf.String()+"A"); err == nil {
		t.Error("a modified signature must be rejected")
	}
}

func TestSnapshotTTLs(t *testing.T) {
	mr := miniredis.RunT(t)
	s := NewSnapshots(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()
	item := []SnapshotItem{{ID: "p1", Score: 1}}
	for _, tc := range []struct {
		key  string
		snap Snapshot
		ttl  time.Duration
	}{
		{"full", Snapshot{Items: item}, SnapshotTTL},
		{"degraded", Snapshot{Items: item, Degraded: true}, degradedTTL},
		{"degraded-empty", Snapshot{Degraded: true}, 0},
		{"empty", Snapshot{}, SnapshotTTL},
	} {
		if err := s.Put(ctx, "T1", tc.key, &tc.snap); err != nil {
			t.Fatal(err)
		}
		if got := mr.TTL(snapKey("T1", tc.key)); got != tc.ttl {
			t.Errorf("%s: ttl = %v, want %v", tc.key, got, tc.ttl)
		}
	}
	if _, ok, _ := s.Get(ctx, "T1", "degraded-empty"); ok {
		t.Error("a degraded empty result must not be cached")
	}
}
