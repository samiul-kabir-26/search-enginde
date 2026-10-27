//go:build integration

package controlplane_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/zavisoft/search-engine/internal/providers/scorer/artifacts"
	"github.com/zavisoft/search-engine/internal/providers/scorer/onnx"
)

// memModels is an in-memory model store: tests write what a presigned PUT or `featurejob
// publish` would.
type memModels struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memModels) put(key string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
}

func (m *memModels) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("%s: %w", key, artifacts.ErrNotFound)
	}
	return b, nil
}

func (m *memModels) Size(ctx context.Context, key string) (int64, error) {
	b, err := m.Get(ctx, key)
	return int64(len(b)), err
}

func (m *memModels) Manifest(ctx context.Context, id string) (artifacts.Manifest, error) {
	b, err := m.Get(ctx, artifacts.PlatformManifestKey(id))
	if err != nil {
		return artifacts.Manifest{}, err
	}
	var man artifacts.Manifest
	err = json.Unmarshal(b, &man)
	return man, err
}

// initONNX loads ONNX Runtime from third_party (`make onnxruntime`), as control-api does.
func initONNX(t *testing.T) bool {
	_, here, _, _ := runtime.Caller(0)
	lib := filepath.Join(filepath.Dir(here), "..", "..", "third_party", "onnxruntime", "lib", "libonnxruntime.so")
	if _, err := os.Stat(lib); err != nil {
		t.Logf("ONNX Runtime not installed (%s); ONNX uploads will be refused", lib)
		return false
	}
	return onnx.Init(lib) == nil
}

func testModel(t *testing.T, name string) []byte {
	_, here, _, _ := runtime.Caller(0)
	b, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "providers", "scorer", "onnx", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sumScorer is a KServe v2 REST scorer: each row's score is the sum of its features.
func sumScorer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Inputs []struct {
				Shape []int     `json:"shape"`
				Data  []float32 `json:"data"`
			} `json:"inputs"`
		}
		if !strings.HasSuffix(r.URL.Path, "/v2/models/ranker/infer") || json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		n, f := req.Inputs[0].Shape[0], req.Inputs[0].Shape[1]
		out := make([]float32, n)
		for i := range n {
			for j := range f {
				out[i] += req.Inputs[0].Data[i*f+j]
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"outputs": []any{map[string]any{"name": "SCORES", "datatype": "FP32", "shape": []int{n}, "data": out}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLearnedScorersAndPolicies(t *testing.T) {
	e := newEnv(t)
	owner, tid := e.signup("ranker", "owner@ranker.test")
	base := "/v1/tenants/" + tid
	if !initONNX(t) {
		t.Skip("needs ONNX Runtime")
	}
	features := []string{"rrf_score", "rating", "ctr_7d"}
	srv := sumScorer(t)

	var prerankID, rankID string
	t.Run("onnx upload", func(t *testing.T) {
		up := owner.expect(http.StatusCreated, owner.do("POST", base+"/models/uploads", map[string]string{"format": "onnx"}), "presign")
		uploadID := up.body["upload_id"].(string)
		if !strings.Contains(up.body["url"].(string), "tenants/"+tid+"/models/"+uploadID+".onnx") || up.body["method"] != "PUT" {
			t.Errorf("upload = %s", up.raw)
		}
		register := func(fs []string) response {
			return owner.do("POST", base+"/models", map[string]any{"kind": "prerank", "type": "onnx", "name": "my-prerank", "version": "1",
				"upload_id": uploadID, "input_schema": map[string]any{"features": fs}})
		}
		if codes := errorCodes(owner.expect(http.StatusBadRequest, register(features), "nothing uploaded")); codes["/upload_id"] != "not_found" {
			t.Errorf("codes %v", codes)
		}
		e.models.put(artifacts.UploadKey(tid, uploadID), testModel(t, "linear3.onnx"))
		if codes := errorCodes(owner.expect(http.StatusBadRequest, register(features[:2]), "wrong width")); codes["/upload_id"] != "dry_run_failed" {
			t.Errorf("codes %v", codes)
		}
		codes := errorCodes(owner.expect(http.StatusBadRequest, register([]string{"rrf_score", "user_price_band_match", "bogus"}), "bad features"))
		if codes["/input_schema/features/1"] != "unavailable" || codes["/input_schema/features/2"] != "unknown_feature" {
			t.Errorf("codes %v", codes)
		}
		m := owner.expect(http.StatusCreated, register(features), "register onnx")
		if m.body["status"] != "validated" || len(m.body["artifact_sha256"].(string)) != 64 {
			t.Errorf("model = %s", m.raw)
		}
		prerankID = m.body["id"].(string)
		v := owner.expect(http.StatusOK, owner.do("POST", base+"/models/"+prerankID+"/validate", nil), "revalidate")
		if v.body["ok"] != true {
			t.Errorf("revalidate = %s", v.raw)
		}
		rr := owner.do("POST", base+"/models", map[string]any{"kind": "rerank", "type": "onnx", "name": "r", "version": "1", "upload_id": uploadID,
			"input_schema": map[string]any{"features": features}})
		// Re-rankers are remote only (M8): an ONNX re-ranker is refused.
		if errorCodes(owner.expect(http.StatusBadRequest, rr, "rerank"))["/type"] != "invalid" {
			t.Errorf("rerank = %s", rr.raw)
		}
	})

	t.Run("kserve rank", func(t *testing.T) {
		m := owner.expect(http.StatusCreated, owner.do("POST", base+"/models", map[string]any{"kind": "rank", "type": "kserve_v2", "name": "ranker",
			"version": "", "endpoint": srv.URL, "input_schema": map[string]any{"features": features}}), "register kserve")
		rankID = m.body["id"].(string)
		if m.body["status"] != "registered" {
			t.Errorf("model = %s", m.raw)
		}
		v := owner.expect(http.StatusOK, owner.do("POST", base+"/models/"+rankID+"/validate", nil), "validate")
		got := owner.expect(http.StatusOK, owner.do("GET", base+"/models/"+rankID, nil), "get")
		if v.body["ok"] != true || got.body["status"] != "validated" || got.body["transport"] != "rest" {
			t.Errorf("validate = %s; model = %s", v.raw, got.raw)
		}
	})

	t.Run("onboarding ranking", func(t *testing.T) {
		ob := base + "/onboarding"
		owner.expect(http.StatusOK, owner.do("PUT", ob+"/steps/engine_tour", nil), "tour")
		sample := map[string]any{"id": "p1", "name": "Red bag", "category": map[string]any{"path": []string{"Bags"}},
			"price": map[string]any{"amount_minor": 1999, "currency": "USD"}, "vendor": map[string]any{"id": "v1"},
			"images": []map[string]string{{"url": "http://img/p1.jpg"}}}
		owner.expect(http.StatusOK, owner.do("PUT", ob+"/steps/catalog", map[string]any{"ingestion": "upload", "samples": []any{sample},
			"field_mapping": map[string]string{"product_id": "$.id", "title": "$.name", "category_path": "$.category.path",
				"price_minor": "$.price.amount_minor", "currency": "$.price.currency", "vendor_id": "$.vendor.id", "image_urls": "$.images[*].url"}}), "catalog")
		owner.expect(http.StatusOK, owner.do("PUT", ob+"/steps/retrieval?use_defaults=true", nil), "defaults for the rest")
		r := owner.do("PUT", ob+"/steps/ranking", map[string]any{"mode": "learned"})
		if errorCodes(owner.expect(http.StatusBadRequest, r, "untrained"))["/mode"] != "not_available" {
			t.Errorf("learned before training = %s", r.raw)
		}
		for id, typ := range map[string]string{"prerank-gbdt-v1": "onnx", "rank-gbdt-v1": "kserve_v2"} {
			man, _ := json.Marshal(artifacts.Manifest{ID: id, Type: typ, Features: features, SHA256: strings.Repeat("0", 64)})
			e.models.put(artifacts.PlatformManifestKey(id), man)
		}
		owner.expect(http.StatusOK, owner.do("PUT", ob+"/steps/ranking", map[string]any{"mode": "learned"}), "learned")
		draft := owner.expect(http.StatusOK, owner.do("GET", base+"/config/draft", nil), "draft").body["document"].(map[string]any)
		if s := draft["prerank"].(map[string]any)["scorer"].(map[string]any); s["type"] != "onnx" || s["model"] != "prerank-gbdt-v1" {
			t.Errorf("prerank = %v", s)
		}
		owner.expect(http.StatusOK, owner.do("PUT", ob+"/steps/ranking", map[string]any{"mode": "byo", "prerank_model_id": prerankID, "rank_model_id": rankID}), "byo")
		draft = owner.expect(http.StatusOK, owner.do("GET", base+"/config/draft", nil), "draft").body["document"].(map[string]any)
		if s := draft["rank"].(map[string]any)["scorer"].(map[string]any); s["type"] != "kserve_v2" || s["model_id"] != rankID {
			t.Errorf("rank = %v", s)
		}
		swapped := owner.do("PUT", ob+"/steps/ranking", map[string]any{"mode": "byo", "prerank_model_id": rankID})
		if errorCodes(owner.expect(http.StatusBadRequest, swapped, "rank model as prerank"))["/prerank/scorer/model_id"] != "wrong_kind" {
			t.Errorf("swapped = %s", swapped.raw)
		}
		models := owner.expect(http.StatusOK, owner.do("GET", "/v1/platform/models", nil), "platform models")
		found := 0
		for _, it := range models.body["items"].([]any) {
			m := it.(map[string]any)
			if m["kind"] == "scorer" && m["available"] == true && len(m["features"].([]any)) == 3 {
				found++
			}
		}
		if found != 2 {
			t.Errorf("platform scorers = %s", models.raw)
		}
	})

	t.Run("policies compile", func(t *testing.T) {
		ob := base + "/onboarding/steps/policies"
		bad := owner.do("PUT", ob, map[string]any{"rules": []map[string]string{{"id": "typo", "kind": "filter", "expr": "item.vendor.verifed"}}, "verified_vendors": true})
		if errorCodes(owner.expect(http.StatusBadRequest, bad, "typo"))["/policy/rules/0/expr"] != "unknown_field" {
			t.Errorf("typo = %s", bad.raw)
		}
		ok := owner.expect(http.StatusOK, owner.do("PUT", ob, map[string]any{"verified_vendors": true,
			"rules": []map[string]string{{"id": "fast", "kind": "boost", "expr": "item.vendor.on_time_dispatch_rate > 0.95 ? 0.2 : 0.0"}}}), "rules")
		if strings.Contains(ok.raw, "deferred") {
			t.Errorf("rules are enforced now: %s", ok.raw)
		}
	})
}
