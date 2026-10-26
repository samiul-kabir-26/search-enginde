package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/events"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/features"
	"github.com/zavisoft/search-engine/internal/stages/prerank"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// fakeSink records produced and emitted events.
type fakeSink struct {
	mu       sync.Mutex
	produced []events.Event
	emitted  [][]events.Event
	tenants  []tenant.ID
	fail     bool
}

func (f *fakeSink) Produce(_ context.Context, tid tenant.ID, evs []events.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("kafka down")
	}
	f.produced = append(f.produced, evs...)
	f.tenants = append(f.tenants, tid)
	return nil
}

func (f *fakeSink) Emit(tid tenant.ID, evs []events.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.emitted = append(f.emitted, evs)
	f.tenants = append(f.tenants, tid)
}

func (h *harness) do(t *testing.T, method, path, tok, body string) (int, platform.Problem, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, h.srv.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var pr platform.Problem
	if resp.StatusCode >= 400 {
		_ = json.Unmarshal(b, &pr)
	}
	return resp.StatusCode, pr, string(b)
}

func eventJSON(id, typ, extra string) string {
	return fmt.Sprintf(`{"event_id":"%s","type":"%s","ts":"2026-10-25T12:00:00Z","product_id":"p01","position":1%s}`, id, typ, extra)
}

const (
	ev1 = "a0000000-0000-4000-8000-000000000001"
	ev2 = "a0000000-0000-4000-8000-000000000002"
)

func TestEventsIngest(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeEventsWrite)

	body := `{"events":[` + eventJSON(ev1, "click", "") + `,` + eventJSON(ev1, "click", "") + `,` +
		eventJSON(ev2, "order", `,"order_id":"o1","quantity":2,"value_minor":9000`) + `]}`
	code, _, resp := h.do(t, http.MethodPost, "/v1/events", tok, body)
	if code != http.StatusAccepted || !strings.Contains(resp, `"accepted":2`) {
		t.Fatalf("code %d %s", code, resp)
	}
	if len(h.events.produced) != 2 || h.events.tenants[0] != h.res.Tenant.ID {
		t.Errorf("produced %d events for %v; want 2 (duplicate event_id dropped) for the token's tenant", len(h.events.produced), h.events.tenants)
	}

	for name, tc := range map[string]struct {
		body string
		path string
	}{
		"tenant in body":       {`{"events":[` + eventJSON(ev1, "click", `,"tenant_id":"other"`) + `]}`, ""},
		"order without fields": {`{"events":[` + eventJSON(ev1, "order", "") + `]}`, "/events/0/order_id"},
		"bad uuid":             {`{"events":[` + eventJSON("nope", "click", "") + `]}`, "/events/0/event_id"},
		"randomized click":     {`{"events":[` + eventJSON(ev1, "click", `,"randomized":true`) + `]}`, "/events/0/randomized"},
		"impression w/o pos":   {`{"events":[{"event_id":"` + ev1 + `","type":"impression","ts":"2026-10-25T12:00:00Z","product_id":"p1"}]}`, "/events/0/position"},
		"future ts":            {`{"events":[{"event_id":"` + ev1 + `","type":"click","ts":"2099-01-01T00:00:00Z","product_id":"p1"}]}`, "/events/0/ts"},
		"empty":                {`{"events":[]}`, "/events"},
		"too many":             {`{"events":[` + strings.Repeat(eventJSON(ev1, "click", "")+",", events.MaxBatchSize) + eventJSON(ev1, "click", "") + `]}`, "/events"},
	} {
		code, pr, _ := h.do(t, http.MethodPost, "/v1/events", tok, tc.body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: code %d, want 400", name, code)
			continue
		}
		if tc.path != "" && (len(pr.Errors) == 0 || pr.Errors[0].Path != tc.path) {
			t.Errorf("%s: errors %+v, want path %s", name, pr.Errors, tc.path)
		}
	}

	if code, _, _ := h.do(t, http.MethodPost, "/v1/events", h.token(t, auth.ScopeSearchRead), body); code != http.StatusForbidden {
		t.Errorf("search:read token: code %d, want 403", code)
	}
	h.events.fail = true
	if code, _, _ := h.do(t, http.MethodPost, "/v1/events", tok, body); code != http.StatusServiceUnavailable {
		t.Errorf("kafka down: code %d, want 503 so the client retries", code)
	}
}

func TestEventsRateLimitCountsEvents(t *testing.T) {
	h := newHarness(t)
	h.res.Plan.EventsRPS = 100
	tok := h.token(t, auth.ScopeEventsWrite)
	batch := func(offset int) string {
		var b strings.Builder
		b.WriteString(`{"events":[`)
		for i := range events.MaxBatchSize {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(eventJSON(fmt.Sprintf("a0000000-0000-4000-8000-%012d", offset+i), "click", ""))
		}
		b.WriteString(`]}`)
		return b.String()
	}
	if code, _, resp := h.do(t, http.MethodPost, "/v1/events", tok, batch(0)); code != http.StatusAccepted {
		t.Fatalf("a full batch must fit the burst: code %d %s", code, resp)
	}
	code, pr, _ := h.do(t, http.MethodPost, "/v1/events", tok, batch(events.MaxBatchSize))
	if code != http.StatusTooManyRequests || !strings.Contains(pr.Detail, "100 events per second") {
		t.Errorf("a second batch at once exceeds 100 events/s: code %d %+v", code, pr)
	}
}

func TestVendorsPutIsPartial(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeCatalogWrite)
	code, _, resp := h.do(t, http.MethodPut, "/v1/vendors", tok,
		`{"vendors":[{"vendor_id":"v_r","score":0.85,"on_time_dispatch_rate":0.98,"response_hours":12,"verified":true}]}`)
	if code != http.StatusOK || !strings.Contains(resp, `"updated":1`) {
		t.Fatalf("code %d %s", code, resp)
	}
	// A later update of one field keeps the others.
	if code, _, _ := h.do(t, http.MethodPut, "/v1/vendors", tok, `{"vendors":[{"vendor_id":"v_r","score":0.4}]}`); code != http.StatusOK {
		t.Fatalf("partial update: code %d", code)
	}
	got, err := h.features.Read(context.Background(), h.res.Tenant.ID, pipeline.FeatureRequest{VendorIDs: []string{"v_r"}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{feature.VendorScore: 0.4, feature.VendorOnTimeDispatch: 0.98, feature.VendorResponseHours: 0.25, feature.VendorVerified: 1}
	for k, v := range want {
		if got.Vendors["v_r"][k] != v {
			t.Errorf("%s = %v, want %v (all: %v)", k, got.Vendors["v_r"][k], v, got.Vendors["v_r"])
		}
	}
	if _, ok := got.Vendors["v_r"][feature.VendorDisputeRate]; ok {
		t.Error("a field never sent must stay unset so the declared default applies")
	}

	for name, body := range map[string]string{
		"score out of range": `{"vendors":[{"vendor_id":"v_r","score":1.5}]}`,
		"no fields":          `{"vendors":[{"vendor_id":"v_r"}]}`,
		"no id":              `{"vendors":[{"score":0.5}]}`,
		"negative hours":     `{"vendors":[{"vendor_id":"v_r","response_hours":-1}]}`,
		"unknown field":      `{"vendors":[{"vendor_id":"v_r","rank":3}]}`,
	} {
		if code, _, _ := h.do(t, http.MethodPut, "/v1/vendors", tok, body); code != http.StatusBadRequest {
			t.Errorf("%s: code %d, want 400", name, code)
		}
	}
	if code, _, _ := h.do(t, http.MethodPut, "/v1/vendors", h.token(t, auth.ScopeEventsWrite), `{"vendors":[{"vendor_id":"v","score":1}]}`); code != http.StatusForbidden {
		t.Errorf("events:write token: code %d, want 403", code)
	}
}

func traceCandidate(t *testing.T, tr *pipeline.Trace, stage, id string) map[string]float64 {
	t.Helper()
	for _, st := range tr.Stages {
		if st.Stage != stage {
			continue
		}
		for _, c := range st.Candidates {
			if c.ProductID == id {
				return c.Scores
			}
		}
	}
	t.Fatalf("%s not in the %s trace", id, stage)
	return nil
}

func position(rs []Result, id string) int {
	for _, r := range rs {
		if r.ProductID == id {
			return r.Position
		}
	}
	return 0
}

// TestOnlineFeaturesReadableInSearch is M6's acceptance check: item, vendor and user features
// stored in Redis are read by S4, show in the explain trace and change the ranking.
func TestOnlineFeaturesReadableInSearch(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	tid := h.res.Tenant.ID
	tok := h.token(t, auth.ScopeSearchRead, auth.ScopeSearchDebug)
	const q = `{"q":"red leather bag","user":{"id":"u1"},"page":{"size":5},"explain":true}`

	_, before, _ := h.search(t, tok, q)
	if position(before.Results, "p04") < 2 {
		t.Fatalf("p04 must start below the top for the test to show a change: %+v", before.Results)
	}

	if err := h.features.PutItems(ctx, tid, map[string]features.Item{"p04": {CTR7d: 0.9, CTR30d: 0.8, CVR7d: 0.7, CVR30d: 0.6, Orders30dLog: 0.5}}); err != nil {
		t.Fatal(err)
	}
	score, verified := 0.95, true
	if code, _, _ := h.do(t, http.MethodPut, "/v1/vendors", h.token(t, auth.ScopeCatalogWrite),
		fmt.Sprintf(`{"vendors":[{"vendor_id":"v_g","score":%v,"verified":%v}]}`, score, verified)); code != http.StatusOK {
		t.Fatal(code)
	}
	if err := h.features.PutUsers(ctx, tid, map[string]features.User{"u1": {CategoryAffinity: map[string]float64{"backpacks": 0.7, "bags": 0.9}, PriceLo: 2000, PriceHi: 3000}}); err != nil {
		t.Fatal(err)
	}

	code, after, pr := h.search(t, tok, q)
	if code != http.StatusOK {
		t.Fatalf("code %d %+v", code, pr)
	}
	got := traceCandidate(t, after.Trace, pipeline.StagePrerank, "p04")
	for k, v := range map[string]float64{
		feature.CTR7d: 0.9, feature.CVR7d: 0.7, feature.Orders30dLog: 0.5,
		feature.VendorScore: 0.95, feature.VendorVerified: 1,
		feature.UserCategoryAffinity: 0.9, feature.UserPriceBandMatch: 1,
	} {
		if got[k] != v {
			t.Errorf("trace %s = %v, want %v", k, got[k], v)
		}
	}
	if p := position(after.Results, "p04"); p != 1 {
		t.Errorf("p04 at %d after its CTR/CVR and vendor score were stored; want 1 (was %d)", p, position(before.Results, "p04"))
	}
	other := traceCandidate(t, after.Trace, pipeline.StagePrerank, "p02") // Rivet: nothing stored
	if _, ok := other[feature.CTR7d]; ok || other[feature.UserPriceBandMatch] != 0 || other[feature.UserCategoryAffinity] != 0.9 {
		t.Errorf("p02 features = %v; unset items keep the declared default, 7900 is outside the band, bags affinity applies", other)
	}
	for _, st := range after.Trace.Stages {
		if st.Stage == pipeline.StagePrerank && st.Fallback {
			t.Errorf("prerank fell back: %s", st.FallbackReason)
		}
	}
}

func TestFeatureStoreOutageFallsBackToDefaults(t *testing.T) {
	h := newHarness(t)
	h.redis.Close()
	code, sr, pr := h.search(t, h.token(t, auth.ScopeSearchRead, auth.ScopeSearchDebug), `{"q":"red leather bag","explain":true}`)
	if code != http.StatusOK || len(sr.Results) == 0 || sr.Degraded {
		t.Fatalf("code %d degraded=%v %+v", code, sr.Degraded, pr)
	}
	for _, st := range sr.Trace.Stages {
		if st.Stage == pipeline.StagePrerank && (!st.Fallback || !strings.HasPrefix(st.FallbackReason, prerank.ProviderOnlineFeatures+":")) {
			t.Errorf("prerank trace = %+v; want an online_features fallback", st)
		}
	}
}

func TestSearchEmitsImpressions(t *testing.T) {
	h := newHarness(t)
	tok := h.token(t, auth.ScopeSearchRead)
	_, first, _ := h.search(t, tok, `{"q":"red leather bag","user":{"id":"u1","session_id":"s1"},"page":{"size":2}}`)
	_, second, _ := h.search(t, tok, `{"q":"red leather bag","user":{"id":"u1","session_id":"s1"},"page":{"size":2,"number":2}}`)
	if len(h.events.emitted) != 2 {
		t.Fatalf("emitted %d batches, want one per served page", len(h.events.emitted))
	}
	for i, page := range []SearchResponse{first, second} {
		batch := h.events.emitted[i]
		if len(batch) != len(page.Results) {
			t.Fatalf("page %d: %d impressions for %d results", i+1, len(batch), len(page.Results))
		}
		for j, e := range batch {
			r := page.Results[j]
			if e.Type != events.TypeImpression || e.RequestID != page.RequestID || e.ProductID != r.ProductID ||
				e.Position != r.Position || e.UserID != "u1" || e.SessionID != "s1" || e.TS.IsZero() {
				t.Errorf("page %d impression %d = %+v for result %+v", i+1, j, e, r)
			}
		}
		if _, errs := events.ValidateBatch(batch, batch[0].TS); len(errs) > 0 {
			t.Errorf("emitted impressions must pass ingest validation: %+v", errs)
		}
	}

	code, _, _ := h.do(t, http.MethodGet, "/v1/products/p01/similar?size=3", tok, "")
	if code != http.StatusOK || len(h.events.emitted) != 3 || h.events.emitted[2][0].RequestID == "" {
		t.Errorf("similar items: code %d, batches %d", code, len(h.events.emitted))
	}
}
