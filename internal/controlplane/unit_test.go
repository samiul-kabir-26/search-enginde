package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/tenant"
)

func TestSettingsValidate(t *testing.T) {
	if errs := DefaultSettings().Validate(); len(errs) != 0 {
		t.Fatalf("defaults invalid: %+v", errs)
	}
	s := DefaultSettings()
	s.Auth.AccessTokenTTL = Duration(2 * time.Hour)
	s.Auth.KeyRetention = Duration(time.Minute)
	s.Auth.Password.MemoryKiB = 1024
	s.Auth.PasswordMinLength = 4
	s.Ads.MaxSlots = 0
	s.Ads.MinRelevanceFloor = 2.0
	paths := map[string]bool{}
	for _, e := range s.Validate() {
		paths[e.Path] = true
	}
	for _, p := range []string{"/auth/access_token_ttl", "/auth/key_retention", "/auth/password/memory_kib", "/auth/password_min_length", "/ads/max_slots", "/ads/min_relevance_floor"} {
		if !paths[p] {
			t.Errorf("missing %s in %v", p, paths)
		}
	}
	b, _ := json.Marshal(DefaultSettings())
	var back PlatformSettings
	if err := json.Unmarshal(b, &back); err != nil || !reflect.DeepEqual(back, DefaultSettings()) {
		t.Errorf("settings must round-trip: %s %v", b, err)
	}
	var d Duration
	if err := json.Unmarshal([]byte(`"forever"`), &d); err == nil {
		t.Error("bad duration accepted")
	}
}

func TestIdempotencyMiddleware(t *testing.T) {
	mr := miniredis.RunT(t)
	idem := NewIdempotency(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	var calls atomic.Int32
	h := idem.Wrap(func(*http.Request) string { return "T" }, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if strings.Contains(r.URL.Path, "fail") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"n":%d}`, n)
	}))
	do := func(path, key, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := do("/x", "k", `{"a":1}`)
	again := do("/x", "k", `{"a":1}`)
	if first.Code != 201 || again.Body.String() != first.Body.String() || again.Header().Get("Idempotent-Replayed") != "true" || calls.Load() != 1 {
		t.Fatalf("replay: %d %s / %s calls=%d", first.Code, first.Body, again.Body, calls.Load())
	}
	if rec := do("/x", "k", `{"a":2}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("different body: %d", rec.Code)
	}
	do("/x", "", `{}`)
	do("/x", "", `{}`)
	if calls.Load() != 3 {
		t.Error("requests without a key are never deduplicated")
	}
	do("/fail", "f", `{}`)
	do("/fail", "f", `{}`)
	if calls.Load() != 5 {
		t.Error("5xx responses must not be stored, so retries run again")
	}
	if err := mr.Set("idem:{T}:busy", `{"pending":true,"hash":"`+"x"+`"}`); err != nil {
		t.Fatal(err)
	}
	if rec := do("/x", "busy", `{}`); rec.Code != http.StatusUnprocessableEntity && rec.Code != http.StatusConflict {
		t.Errorf("in-flight key: %d", rec.Code)
	}
	if rec := do("/x", strings.Repeat("k", 300), `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("long key: %d", rec.Code)
	}
}

func TestDiffAndExplainer(t *testing.T) {
	changes, err := tenant.Diff([]byte(`{"a":1,"b":{"c":[1,2],"d":"x"},"e":true}`), []byte(`{"a":1,"b":{"c":[1,3]},"f":null}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range changes {
		got[c.Path] = c.Op
	}
	want := map[string]string{"/b/c": "replace", "/b/d": "remove", "/e": "remove", "/f": "add"}
	for p, op := range want {
		if got[p] != op {
			t.Errorf("%s: %q, want %q (%v)", p, got[p], op, got)
		}
	}
	if _, err := tenant.Diff([]byte(`{`), []byte(`{}`)); err == nil {
		t.Error("bad json must fail")
	}
	cfg, _ := tenant.ParseConfig(tenant.DefaultDocument())
	e := TenantExplainer(cfg)
	if len(e.Stages) != 6 || e.Stages[0].Badge != "Default" || !strings.Contains(e.Stages[0].Choice, "bge-small") {
		t.Errorf("explainer = %+v", e.Stages[0])
	}
	cfg.Policy.Rules = []tenant.PolicyRule{{ID: "r", Kind: "filter", Expr: "true"}}
	cfg.Ads.Enabled = true
	e = TenantExplainer(cfg)
	if e.Stages[2].Badge != "Custom" || !strings.Contains(e.Stages[5].Choice, "slots") {
		t.Errorf("custom choices not reflected: %+v %+v", e.Stages[2], e.Stages[5])
	}
	if len(RetrievalPresets()) != 4 || !mayGrant(principalWithRole("admin"), "developer") || mayGrant(principalWithRole("admin"), "owner") {
		t.Error("presets or grants")
	}
	if _, ok := versionRef("latest"); ok {
		t.Error("bad version ref accepted")
	}
}

func principalWithRole(role string) auth.Principal { return auth.Principal{Role: role} }
