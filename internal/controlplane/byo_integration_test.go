//go:build integration

package controlplane_test

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	qdrantgo "github.com/qdrant/go-client/qdrant"

	"github.com/zavisoft/search-engine/internal/providers/vectorstore/qdrant"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

const textDim = 8

func unitVec(seed int, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32((seed+i)%5 + 1)
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

// openAIFake requires the API key stored as a secret and returns textDim-dimensional vectors.
func openAIFake(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var req struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i, s := range req.Input {
			data[i] = map[string]any{"index": i, "embedding": unitVec(len(s), textDim)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// kserveRESTFake speaks only the KServe v2 REST protocol (no gRPC), for transport detection.
func kserveRESTFake(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v2/models/bge/versions/1/infer") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Inputs []struct{ Data []string } `json:"inputs"`
		}
		_ = json.Unmarshal(body, &req)
		var flat []float32
		for i := range req.Inputs[0].Data {
			flat = append(flat, unitVec(i, textDim)...)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"outputs": []any{map[string]any{
			"name": "EMBEDDING", "datatype": "FP32", "shape": []int{len(req.Inputs[0].Data), textDim}, "data": flat}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func storedCiphertext(t *testing.T, e *env, id string) string {
	t.Helper()
	var ct []byte
	if err := e.db.QueryRowContext(t.Context(), "SELECT ciphertext FROM tenant_secrets WHERE id = ?", id).Scan(&ct); err != nil {
		t.Fatal(err)
	}
	return string(ct)
}

// closedPort returns a local address nothing listens on.
func closedPort(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestBYOControlPlane(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, tid := e.signup("byo", "owner@byo.test")
	base := "/v1/tenants/" + tid

	qaddr := testinfra.Qdrant(t)
	admin, err := qdrant.NewClient(qaddr)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := admin.CreateCollection(ctx, &qdrantgo.CreateCollection{CollectionName: "products", VectorsConfig: qdrantgo.NewVectorsConfigMap(map[string]*qdrantgo.VectorParams{
		"emb": {Size: textDim, Distance: qdrantgo.Distance_Cosine}, "img": {Size: 512, Distance: qdrantgo.Distance_Cosine},
	})}); err != nil {
		t.Fatal(err)
	}
	qendpoint := "http://" + strings.Replace(qaddr, "localhost", "127.0.0.1", 1)
	storeBlock := map[string]any{"collection": "products",
		"vectors": map[string]any{"text": map[string]any{"name": "emb", "dim": textDim, "metric": "cosine"}, "image": map[string]any{"name": "img", "dim": 512, "metric": "cosine"}}}

	emb, ks := openAIFake(t), kserveRESTFake(t)
	var secretRef, modelID, connID, published string
	t.Run("secrets", func(t *testing.T) {
		codes := errorCodes(owner.expect(http.StatusBadRequest, owner.do("POST", base+"/secrets", map[string]string{"kind": "Bad Kind", "value": ""}), "invalid secret"))
		if codes["/kind"] == "" || codes["/value"] == "" {
			t.Errorf("errors = %v", codes)
		}
		r := owner.expect(http.StatusCreated, owner.do("POST", base+"/secrets", map[string]string{"kind": "api_key", "value": "sk-test"}), "create secret")
		secretRef = r.body["secret_ref"].(string)
		if strings.Contains(r.raw, "sk-test") {
			t.Error("the value must never be returned")
		}
		if ct := storedCiphertext(t, e, secretRef); strings.Contains(ct, "sk-test") {
			t.Error("the secret must be stored encrypted")
		}
	})

	t.Run("models", func(t *testing.T) {
		kind := errorCodes(owner.expect(http.StatusBadRequest, owner.do("POST", base+"/models", map[string]any{
			"kind": "ranker", "type": "onnx", "name": "m",
		}), "unknown kind"))
		if kind["/kind"] == "" {
			t.Errorf("missing error at /kind: %v", kind)
		}
		codes := errorCodes(owner.expect(http.StatusBadRequest, owner.do("POST", base+"/models", map[string]any{
			"kind": "embedder", "type": "onnx", "name": "", "version": "1", "upload_id": "x",
			"input_schema": map[string]any{"modality": "audio"}, "output_schema": map[string]any{"dim": 0},
		}), "invalid model"))
		for _, p := range []string{"/type", "/name", "/upload_id", "/input_schema/modality", "/output_schema/dim"} {
			if codes[p] == "" {
				t.Errorf("missing error at %s: %v", p, codes)
			}
		}
		denied := errorCodes(owner.expect(http.StatusBadRequest, owner.do("POST", base+"/models", map[string]any{
			"kind": "embedder", "type": "openai_embeddings", "name": "emb", "version": "1", "endpoint": "https://169.254.169.254/latest",
			"input_schema": map[string]any{"modality": "text"}, "output_schema": map[string]any{"dim": textDim},
		}), "metadata endpoint"))
		if denied["/endpoint"] != "egress_denied" {
			t.Errorf("SSRF guard: %v", denied)
		}
		r := owner.expect(http.StatusCreated, owner.do("POST", base+"/models", map[string]any{
			"kind": "embedder", "type": "openai_embeddings", "name": "emb", "version": "1", "endpoint": emb.URL, "secret_ref": secretRef,
			"input_schema": map[string]any{"modality": "text"}, "output_schema": map[string]any{"dim": textDim},
		}), "register model")
		modelID = r.body["id"].(string)
		if r.body["status"] != "registered" {
			t.Errorf("status = %v", r.body["status"])
		}
		v := owner.expect(http.StatusOK, owner.do("POST", base+"/models/"+modelID+"/validate", nil), "validate")
		if v.body["ok"] != true {
			t.Fatalf("validation = %s", v.raw)
		}
		got := owner.expect(http.StatusOK, owner.do("GET", base+"/models/"+modelID, nil), "get model")
		if got.body["status"] != "validated" || got.body["validated_at"] == nil {
			t.Errorf("model = %s", got.raw)
		}

		wrong := owner.expect(http.StatusCreated, owner.do("POST", base+"/models", map[string]any{
			"kind": "embedder", "type": "openai_embeddings", "name": "emb", "version": "1", "endpoint": emb.URL, "secret_ref": secretRef,
			"input_schema": map[string]any{"modality": "text"}, "output_schema": map[string]any{"dim": 16},
		}), "register mismatched model")
		wid := wrong.body["id"].(string)
		if v := owner.expect(http.StatusOK, owner.do("POST", base+"/models/"+wid+"/validate", nil), "validate mismatch"); v.body["ok"] != false || !strings.Contains(v.raw, "want 16") {
			t.Errorf("a dimension mismatch fails validation: %s", v.raw)
		}
		if m := owner.do("GET", base+"/models/"+wid, nil); m.body["status"] != "failed" || m.body["last_error"] == nil {
			t.Errorf("failed model = %s", m.raw)
		}

		k := owner.expect(http.StatusCreated, owner.do("POST", base+"/models", map[string]any{
			"kind": "embedder", "type": "kserve_v2", "name": "bge", "version": "1", "endpoint": ks.URL,
			"input_schema": map[string]any{"modality": "text"}, "output_schema": map[string]any{"dim": textDim},
		}), "register kserve")
		kv := owner.expect(http.StatusOK, owner.do("POST", base+"/models/"+k.body["id"].(string)+"/validate", nil), "validate kserve")
		if kv.body["ok"] != true || !strings.Contains(kv.raw, `"message":"rest"`) {
			t.Errorf("an endpoint without gRPC validates over REST: %s", kv.raw)
		}
		if m := owner.do("GET", base+"/models/"+k.body["id"].(string), nil); m.body["transport"] != "rest" {
			t.Errorf("transport recorded: %s", m.raw)
		}
		list := owner.expect(http.StatusOK, owner.do("GET", base+"/models", nil), "list models")
		if len(list.body["items"].([]any)) != 3 {
			t.Errorf("models = %s", list.raw)
		}
	})

	t.Run("connections", func(t *testing.T) {
		codes := errorCodes(owner.expect(http.StatusBadRequest, owner.do("POST", base+"/connections", map[string]any{
			"kind": "lexical", "type": "faiss", "endpoint": "https://10.0.0.7:6334", "secret_ref": "01JNOSUCHSECRET00000000000",
		}), "invalid connection"))
		for p, c := range map[string]string{"/kind": "byo_unavailable", "/type": "unsupported", "/endpoint": "egress_denied", "/secret_ref": "not_found"} {
			if codes[p] != c {
				t.Errorf("%s = %q, want %q (%v)", p, codes[p], c, codes)
			}
		}
		pg := errorCodes(owner.expect(http.StatusBadRequest, owner.do("POST", base+"/connections", map[string]any{
			"kind": "vector_store", "type": "pgvector", "endpoint": "postgres://search@8.8.8.8:5432/catalog?sslmode=disable",
		}), "pgvector without TLS"))
		if pg["/endpoint"] != "tls_required" {
			t.Errorf("pgvector TLS: %v", pg)
		}
		r := owner.expect(http.StatusCreated, owner.do("POST", base+"/connections", map[string]any{
			"kind": "vector_store", "type": "qdrant", "endpoint": qendpoint,
		}), "create connection")
		connID = r.body["id"].(string)
		if r.body["status"] != "untested" {
			t.Errorf("status = %v", r.body["status"])
		}
		owner.expect(http.StatusBadRequest, owner.do("POST", base+"/connections/"+connID+"/test", nil), "no collection to test")

		bad := map[string]any{"collection": "products", "vectors": map[string]any{"text": map[string]any{"name": "emb", "dim": 16, "metric": "dot"}}}
		res := owner.expect(http.StatusOK, owner.do("POST", base+"/connections/"+connID+"/test", bad), "test mismatch")
		if res.body["ok"] != false || !strings.Contains(res.raw, "16") {
			t.Errorf("mismatch test = %s", res.raw)
		}
		if c := owner.do("GET", base+"/connections/"+connID, nil); c.body["status"] != "failed" || c.body["last_error"] == nil {
			t.Errorf("failed connection = %s", c.raw)
		}
		res = owner.expect(http.StatusOK, owner.do("POST", base+"/connections/"+connID+"/test", storeBlock), "test")
		if res.body["ok"] != true {
			t.Fatalf("test = %s", res.raw)
		}
		names := []string{}
		for _, c := range res.body["checks"].([]any) {
			names = append(names, c.(map[string]any)["name"].(string))
		}
		if strings.Join(names, ",") != "configure,health,collection,vectors,fields,sample_query" {
			t.Errorf("checks = %v", names)
		}
		other, _ := e.signup("byo-other", "other@byo.test")
		other.expect(http.StatusNotFound, other.do("GET", base+"/connections/"+connID, nil), "another tenant's connection")
	})

	t.Run("BYO draft validates live and publishes", func(t *testing.T) {
		draft := owner.expect(http.StatusOK, owner.do("GET", base+"/config/draft", nil), "draft")
		doc := draft.body["document"].(map[string]any)
		doc["embedders"].(map[string]any)["text"] = map[string]any{"provider": "byo", "model_id": modelID}
		vector := map[string]any{"provider": "byo", "connection_id": connID}
		for k, v := range storeBlock {
			vector[k] = v
		}
		doc["stores"].(map[string]any)["vector"] = vector
		r := owner.expect(http.StatusOK, owner.do("PUT", base+"/config/draft", doc), "save BYO draft")
		if v := r.body["validation"].(map[string]any); v["valid"] != true {
			t.Fatalf("BYO draft invalid: %s", r.raw)
		}
		pub := owner.expect(http.StatusCreated, owner.do("POST", base+"/config/versions", nil), "publish")
		published = strconv.Itoa(int(pub.body["version"].(float64)))

		vector["vectors"].(map[string]any)["text"].(map[string]any)["dim"] = 16
		r = owner.expect(http.StatusOK, owner.do("PUT", base+"/config/draft", doc), "save mismatched draft")
		codes := errorCodes(response{body: r.body["validation"].(map[string]any)})
		if codes["/stores/vector/vectors/text/dim"] != "dim_mismatch" {
			t.Errorf("rule 3 (live): %v", codes)
		}
		vector["vectors"].(map[string]any)["text"].(map[string]any)["dim"] = textDim
		owner.expect(http.StatusOK, owner.do("PUT", base+"/config/draft", doc), "restore draft")

		c := owner.expect(http.StatusConflict, owner.do("DELETE", base+"/connections/"+connID, nil), "delete in use")
		if !strings.Contains(c.raw, "the draft") {
			t.Errorf("in use by = %s", c.raw)
		}
	})

	t.Run("updating a live connection is tested first", func(t *testing.T) {
		owner.expect(http.StatusNoContent, owner.do("POST", "/v1/auth/verify-email", map[string]string{"token": e.mailer.lastToken(t, "owner@byo.test")}), "verify")
		r := owner.expect(http.StatusOK, owner.do("POST", "/v1/auth/login", map[string]string{"tenant": "byo", "email": "owner@byo.test", "password": password}), "relogin")
		owner.token = r.body["access_token"].(string)
		owner.expect(http.StatusOK, owner.do("POST", base+"/config/versions/"+published+"/activate", nil), "activate BYO version")
		deadAddr := closedPort(t)
		r = owner.expect(http.StatusUnprocessableEntity, owner.do("PUT", base+"/connections/"+connID, map[string]any{
			"kind": "vector_store", "type": "qdrant", "endpoint": "http://" + deadAddr,
		}), "breaking update of a live connection")
		if !strings.Contains(r.raw, "connection_test_failed") {
			t.Errorf("update = %s", r.raw)
		}
		if c := owner.do("GET", base+"/connections/"+connID, nil); c.body["endpoint"] != qendpoint || c.body["status"] != "validated" {
			t.Errorf("a failed update changes nothing: %s", c.raw)
		}
		ok := owner.expect(http.StatusOK, owner.do("PUT", base+"/connections/"+connID, map[string]any{
			"kind": "vector_store", "type": "qdrant", "endpoint": qendpoint,
		}), "passing update of a live connection")
		if ok.body["status"] != "validated" {
			t.Errorf("a tested update stays validated: %s", ok.raw)
		}
	})

	t.Run("plans and platform models", func(t *testing.T) {
		starter := e.client()
		r := starter.expect(http.StatusCreated, starter.do("POST", "/v1/signup", map[string]string{
			"company_name": "Small", "slug": "small", "region": "eu", "plan": "starter", "email": "s@small.test", "password": password,
		}), "starter signup")
		starter.token = r.body["access_token"].(string)
		sb := "/v1/tenants/" + r.body["tenant_id"].(string)
		f := starter.expect(http.StatusForbidden, starter.do("POST", sb+"/secrets", map[string]string{"kind": "api_key", "value": "x"}), "starter secret")
		if !strings.Contains(f.raw, "plan_forbids_byo") {
			t.Errorf("starter = %s", f.raw)
		}
		pm := e.client().expect(http.StatusOK, e.client().do("GET", "/v1/platform/models", nil), "platform models")
		// Two embedders and the two learned scorers (M7).
		if len(pm.body["items"].([]any)) != 4 {
			t.Errorf("platform models = %s", pm.raw)
		}
	})
}
