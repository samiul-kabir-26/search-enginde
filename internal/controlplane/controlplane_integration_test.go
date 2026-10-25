//go:build integration

package controlplane_test

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
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/controlplane"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

type mail struct{ to, subject, body string }

type fakeMailer struct {
	mu   sync.Mutex
	sent []mail
}

func (m *fakeMailer) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	m.sent = append(m.sent, mail{to, subject, body})
	m.mu.Unlock()
	return nil
}

func (m *fakeMailer) lastToken(t *testing.T, to string) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	re := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)
	for i := len(m.sent) - 1; i >= 0; i-- {
		if m.sent[i].to == to {
			if g := re.FindStringSubmatch(m.sent[i].body); g != nil {
				return g[1]
			}
		}
	}
	t.Fatalf("no verification mail for %s", to)
	return ""
}

type env struct {
	t        *testing.T
	srv      *httptest.Server
	db       *sql.DB
	rdb      *redis.Client
	mailer   *fakeMailer
	keys     *auth.KeyRing
	verifier *auth.Verifier
	search   *fakeSearch
	byo      *registry.BYO
}

// fakeSearch records what the playground proxy forwards.
type fakeSearch struct {
	mu      sync.Mutex
	version string
	scope   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("mysql", testinfra.MySQL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rdb := redis.NewClient(&redis.Options{Addr: testinfra.Redis(t)})
	t.Cleanup(func() { _ = rdb.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	sealer, err := security.NewSealer(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	q := tenantdb.New(db)
	settings := controlplane.NewSettings(q, rdb, log)
	if err := settings.Load(ctx); err != nil {
		t.Fatal(err)
	}
	keys := auth.NewKeyRing(db, sealer, "http://issuer", log)
	if err := keys.Load(ctx); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: db, rdb: rdb, mailer: &fakeMailer{}, keys: keys, search: &fakeSearch{}}
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := auth.NewLocalVerifier(keys, "http://issuer")
		p, err := v.Verify(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		e.search.mu.Lock()
		e.search.version = r.Header.Get(controlplane.HeaderConfigVersion)
		if err == nil && p.Has(auth.ScopeSearchPlayground) {
			e.search.scope = auth.ScopeSearchPlayground
		}
		e.search.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[],"trace":{"stages":[]}}`))
	}))
	t.Cleanup(search.Close)
	e.verifier = auth.NewLocalVerifier(keys, "http://issuer")
	s3c := s3.New(s3.Options{Region: "us-east-1", BaseEndpoint: aws.String("http://localhost:9100"), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("a", "b", "")})
	// Local test endpoints (127.0.0.1) stand in for tenant endpoints, so they are trusted.
	tenants := tenant.NewStore(db)
	e.byo = &registry.BYO{Source: tenants, Secrets: security.NewDBSecretStore(q, sealer), Timeout: 5 * time.Second,
		Egress: security.NewEgress(security.EgressConfig{Trusted: []string{"127.0.0.1", "localhost"}})}
	srv := &controlplane.Server{
		DB: db, Q: q, Tenants: tenants, Keys: keys, Verifier: e.verifier, Settings: settings, BYO: e.byo,
		Mailer: e.mailer, Redis: rdb, Idem: controlplane.NewIdempotency(rdb), Presign: s3.NewPresignClient(s3c),
		UploadBucket: "catalog-uploads", SearchURL: search.URL, HTTPClient: http.DefaultClient,
		AppBaseURL: "http://console", PlatformEmbedder: registry.PlatformEmbedderInfo, Log: log,
	}
	mux := http.NewServeMux()
	srv.Routes(mux)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

type client struct {
	e     *env
	http  *http.Client
	token string
}

func (e *env) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{Jar: jar}}
}

type response struct {
	code    int
	body    map[string]any
	raw     string
	headers http.Header
}

func (c *client) do(method, path string, body any, headers ...string) response {
	c.e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequestWithContext(c.e.t.Context(), method, c.e.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := response{code: resp.StatusCode, raw: string(raw), headers: resp.Header}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (c *client) expect(code int, r response, what string) response {
	c.e.t.Helper()
	if r.code != code {
		c.e.t.Fatalf("%s: status %d, want %d: %s", what, r.code, code, r.raw)
	}
	return r
}

func errorCodes(r response) map[string]string {
	out := map[string]string{}
	errs, _ := r.body["errors"].([]any)
	for _, e := range errs {
		m := e.(map[string]any)
		out[m["path"].(string)] = m["code"].(string)
	}
	return out
}

const password = "correct horse battery staple"

func (e *env) signup(slug, email string) (*client, string) {
	c := e.client()
	r := c.expect(http.StatusCreated, c.do("POST", "/v1/signup", map[string]string{
		"company_name": "Acme " + slug, "slug": slug, "region": "eu", "plan": "growth", "email": email, "password": password,
	}), "signup")
	c.token = r.body["access_token"].(string)
	return c, r.body["tenant_id"].(string)
}

func TestControlPlane(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	owner, tid := e.signup("acme", "owner@acme.test")
	base := "/v1/tenants/" + tid

	t.Run("signup validation and login", func(t *testing.T) {
		c := e.client()
		r := c.do("POST", "/v1/signup", map[string]string{"company_name": "", "slug": "-x", "region": "", "email": "nope", "password": "short"})
		codes := errorCodes(c.expect(http.StatusBadRequest, r, "bad signup"))
		for _, p := range []string{"/company_name", "/slug", "/region", "/email", "/password"} {
			if codes[p] == "" {
				t.Errorf("missing error for %s: %v", p, codes)
			}
		}
		c.expect(http.StatusConflict, c.do("POST", "/v1/signup", map[string]string{"company_name": "Dup", "slug": "acme", "region": "eu", "email": "x@y.test", "password": password}), "duplicate slug")
		c.expect(http.StatusUnauthorized, c.do("POST", "/v1/auth/login", map[string]string{"tenant": "acme", "email": "owner@acme.test", "password": "wrong password!!"}), "bad password")
		c.expect(http.StatusUnauthorized, c.do("POST", "/v1/auth/login", map[string]string{"tenant": "nope", "email": "owner@acme.test", "password": password}), "bad tenant")
		r = c.expect(http.StatusOK, c.do("POST", "/v1/auth/login", map[string]string{"tenant": "acme", "email": "OWNER@acme.test", "password": password}), "login")
		if r.body["user"].(map[string]any)["role"] != "owner" {
			t.Errorf("login user = %v", r.body["user"])
		}
	})

	t.Run("refresh rotation and reuse detection", func(t *testing.T) {
		c := e.client()
		c.expect(http.StatusOK, c.do("POST", "/v1/auth/login", map[string]string{"tenant": "acme", "email": "owner@acme.test", "password": password}), "login")
		u, _ := url.Parse(e.srv.URL + "/v1/")
		first := c.http.Jar.Cookies(u)[0].Value
		c.expect(http.StatusOK, c.do("POST", "/v1/auth/refresh", nil), "refresh")
		thief := e.client()
		thief.http.Jar.SetCookies(u, []*http.Cookie{{Name: controlplane.RefreshCookie, Value: first, Path: "/v1/"}})
		thief.expect(http.StatusUnauthorized, thief.do("POST", "/v1/auth/refresh", nil), "reused refresh token")
		c.expect(http.StatusUnauthorized, c.do("POST", "/v1/auth/refresh", nil), "the whole family is revoked after reuse")
	})

	t.Run("config lifecycle", func(t *testing.T) {
		draft := owner.expect(http.StatusOK, owner.do("GET", base+"/config/draft", nil), "get draft")
		doc := draft.body["document"].(map[string]any)
		doc["budgets"].(map[string]any)["total_ms"] = 900
		r := owner.expect(http.StatusOK, owner.do("PUT", base+"/config/draft", doc), "save invalid draft")
		if r.body["validation"].(map[string]any)["valid"] != false {
			t.Errorf("draft over the plan maximum must be invalid: %v", r.body["validation"])
		}
		v := owner.expect(http.StatusOK, owner.do("POST", base+"/config/draft/validate", nil), "validate")
		if errorCodes(v)["/budgets/total_ms"] != "exceeds_plan" {
			t.Errorf("validate = %s", v.raw)
		}
		owner.expect(http.StatusUnprocessableEntity, owner.do("POST", base+"/config/versions", nil), "publish invalid")
		owner.expect(http.StatusBadRequest, owner.do("PUT", base+"/config/draft", `{"schema_version":1,"surprise":true}`), "unknown field")

		doc["budgets"].(map[string]any)["total_ms"] = 300
		owner.expect(http.StatusOK, owner.do("PUT", base+"/config/draft", doc), "fix draft")
		p1 := owner.expect(http.StatusCreated, owner.do("POST", base+"/config/versions", nil, "Idempotency-Key", "pub-1"), "publish v1")
		replay := owner.expect(http.StatusCreated, owner.do("POST", base+"/config/versions", nil, "Idempotency-Key", "pub-1"), "replayed publish")
		if p1.body["version"].(float64) != 1 || replay.body["version"].(float64) != 1 || replay.headers.Get("Idempotent-Replayed") != "true" {
			t.Errorf("idempotent publish: %s / %s", p1.raw, replay.raw)
		}
		p2 := owner.expect(http.StatusCreated, owner.do("POST", base+"/config/versions", nil), "publish v2")
		if p2.body["version"].(float64) != 2 {
			t.Errorf("versions must be monotonic: %s", p2.raw)
		}

		owner.expect(http.StatusForbidden, owner.do("POST", base+"/config/versions/1/activate", nil), "activation needs a verified email")
		owner.expect(http.StatusNoContent, owner.do("POST", "/v1/auth/verify-email", map[string]string{"token": e.mailer.lastToken(t, "owner@acme.test")}), "verify")
		owner.expect(http.StatusBadRequest, owner.do("POST", "/v1/auth/verify-email", map[string]string{"token": "used-or-wrong"}), "bad token")
		r = owner.expect(http.StatusOK, owner.do("POST", "/v1/auth/login", map[string]string{"tenant": "acme", "email": "owner@acme.test", "password": password}), "relogin")
		owner.token = r.body["access_token"].(string)

		sub := e.rdb.Subscribe(ctx, tenant.ChannelConfigActivated)
		defer sub.Close()
		if _, err := sub.Receive(ctx); err != nil {
			t.Fatal(err)
		}
		owner.expect(http.StatusOK, owner.do("POST", base+"/config/versions/1/activate", nil), "activate v1")
		select {
		case m := <-sub.Channel():
			if !strings.Contains(m.Payload, `"version":1`) || !strings.Contains(m.Payload, `"state":"active"`) {
				t.Errorf("activation message = %s", m.Payload)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no activation message")
		}
		owner.expect(http.StatusBadRequest, owner.do("POST", base+"/config/versions/2/shadow", map[string]float64{"sample_rate": 0.5}), "shadow rate cap")
		owner.expect(http.StatusConflict, owner.do("POST", base+"/config/versions/1/shadow", map[string]float64{"sample_rate": 0.1}), "active cannot shadow")
		sh := owner.expect(http.StatusOK, owner.do("POST", base+"/config/versions/2/shadow", map[string]float64{"sample_rate": 0.1}), "shadow v2")
		if sh.body["state"] != "shadow" {
			t.Errorf("shadow = %s", sh.raw)
		}
		list := owner.expect(http.StatusOK, owner.do("GET", base+"/config/versions", nil), "list")
		states := map[float64]string{}
		for _, it := range list.body["items"].([]any) {
			m := it.(map[string]any)
			states[m["version"].(float64)] = m["state"].(string)
		}
		if states[1] != "active" || states[2] != "shadow" {
			t.Errorf("states = %v", states)
		}
		owner.expect(http.StatusOK, owner.do("POST", base+"/config/versions/2/activate", nil), "activate v2")
		list = owner.expect(http.StatusOK, owner.do("GET", base+"/config/versions", nil), "list")
		for _, it := range list.body["items"].([]any) {
			m := it.(map[string]any)
			states[m["version"].(float64)] = m["state"].(string)
		}
		if states[1] != "retired" || states[2] != "active" {
			t.Errorf("after activating v2 = %v", states)
		}

		doc["locale"] = "de"
		owner.expect(http.StatusOK, owner.do("PUT", base+"/config/draft", doc), "change draft")
		diff := owner.expect(http.StatusOK, owner.do("GET", base+"/config/diff?from=1&to=draft", nil), "diff")
		changes := diff.body["changes"].([]any)
		if len(changes) != 1 || changes[0].(map[string]any)["path"] != "/locale" {
			t.Errorf("diff = %s", diff.raw)
		}
		owner.expect(http.StatusOK, owner.do("GET", base+"/config/versions/1", nil), "get v1")
		owner.expect(http.StatusNotFound, owner.do("GET", base+"/config/versions/9", nil), "missing version")
	})

	t.Run("api keys, token exchange, idempotency mismatch", func(t *testing.T) {
		r := owner.expect(http.StatusCreated, owner.do("POST", base+"/api-keys", map[string]any{"scopes": []string{"search:read"}}, "Idempotency-Key", "k1"), "create key")
		owner.expect(http.StatusUnprocessableEntity, owner.do("POST", base+"/api-keys", map[string]any{"scopes": []string{"events:write"}}, "Idempotency-Key", "k1"), "same key, different body")
		owner.expect(http.StatusBadRequest, owner.do("POST", base+"/api-keys", map[string]any{"scopes": []string{"platform:admin"}}), "privileged scope")
		apiKey := r.body["api_key"].(string)
		anon := e.client()
		tok := anon.expect(http.StatusOK, anon.do("POST", "/v1/auth/token", map[string]string{"api_key": apiKey}), "exchange")
		p, err := e.verifier.Verify(ctx, tok.body["access_token"].(string))
		if err != nil || string(p.Tenant) != tid || !p.Has(auth.ScopeSearchRead) || p.Has(auth.ScopeEventsWrite) {
			t.Errorf("exchanged token = %+v %v", p, err)
		}
		keyID := r.body["key"].(map[string]any)["id"].(string)
		owner.expect(http.StatusNoContent, owner.do("DELETE", base+"/api-keys/"+keyID, nil), "revoke")
		anon.expect(http.StatusUnauthorized, anon.do("POST", "/v1/auth/token", map[string]string{"api_key": apiKey}), "revoked key")
	})

	t.Run("users and roles", func(t *testing.T) {
		r := owner.expect(http.StatusCreated, owner.do("POST", base+"/users", map[string]string{"email": "dev@acme.test", "role": "developer", "password": password}), "add developer")
		devID := r.body["id"].(string)
		dev := e.client()
		lr := dev.expect(http.StatusOK, dev.do("POST", "/v1/auth/login", map[string]string{"tenant": "acme", "email": "dev@acme.test", "password": password}), "dev login")
		dev.token = lr.body["access_token"].(string)
		dev.expect(http.StatusForbidden, dev.do("POST", base+"/api-keys", map[string]any{}), "developers cannot create keys")
		dev.expect(http.StatusOK, dev.do("GET", base+"/config/draft", nil), "developers can read")
		ownerID := ""
		users := owner.expect(http.StatusOK, owner.do("GET", base+"/users", nil), "list users")
		for _, u := range users.body["items"].([]any) {
			if m := u.(map[string]any); m["role"] == "owner" {
				ownerID = m["id"].(string)
			}
		}
		owner.expect(http.StatusConflict, owner.do("PATCH", base+"/users/"+ownerID, map[string]string{"role": "admin"}), "last owner")
		owner.expect(http.StatusOK, owner.do("PATCH", base+"/users/"+devID, map[string]string{"role": "viewer"}), "demote dev")
		owner.expect(http.StatusNoContent, owner.do("DELETE", base+"/users/"+devID, nil), "remove dev")
	})

	t.Run("tenant isolation", func(t *testing.T) {
		other, otherID := e.signup("globex", "owner@globex.test")
		other.expect(http.StatusNotFound, other.do("GET", base+"/config/draft", nil), "foreign tenant")
		owner.expect(http.StatusNotFound, owner.do("GET", "/v1/tenants/"+otherID+"/config/draft", nil), "foreign tenant")
		anon := e.client()
		anon.expect(http.StatusUnauthorized, anon.do("GET", base+"/config/draft", nil), "anonymous")
	})

	t.Run("onboarding", func(t *testing.T) {
		c, otid := e.signup("initech", "owner@initech.test")
		ob := "/v1/tenants/" + otid + "/onboarding"
		st := c.expect(http.StatusOK, c.do("GET", ob, nil), "state")
		if st.body["current_step"] != "engine_tour" {
			t.Fatalf("state = %s", st.raw)
		}
		c.expect(http.StatusConflict, c.do("PUT", ob+"/steps/catalog", map[string]any{}), "skipping ahead")
		r := c.expect(http.StatusOK, c.do("PUT", ob+"/steps/engine_tour", nil), "tour")
		if r.body["next_step"] != "catalog" {
			t.Errorf("next = %s", r.raw)
		}
		sample := map[string]any{"id": "p1", "name": "Red bag", "category": map[string]any{"path": []string{"Bags"}},
			"price": map[string]any{"amount_minor": 1999, "currency": "USD"}, "vendor": map[string]any{"id": "v1"},
			"images": []map[string]string{{"url": "http://img/p1.jpg"}}}
		mapping := map[string]string{"product_id": "$.id", "title": "$.name", "category_path": "$.category.path", "price_minor": "$.price.amount_minor",
			"currency": "$.price.currency", "vendor_id": "$.vendor.id", "image_urls": "$.images[*].url"}
		bad := c.expect(http.StatusBadRequest, c.do("PUT", ob+"/steps/catalog", map[string]any{"ingestion": "events", "field_mapping": map[string]string{"product_id": "$.id"}, "samples": []any{sample}}), "incomplete mapping")
		if errorCodes(bad)["/field_mapping/vendor_id"] != "required" {
			t.Errorf("catalog errors = %s", bad.raw)
		}
		c.expect(http.StatusOK, c.do("PUT", ob+"/steps/catalog", map[string]any{"ingestion": "upload", "field_mapping": mapping, "samples": []any{sample}}), "catalog")
		c.expect(http.StatusBadRequest, c.do("PUT", ob+"/steps/retrieval", map[string]any{"preset": "magic"}), "bad preset")
		c.expect(http.StatusOK, c.do("PUT", ob+"/steps/retrieval", map[string]any{"preset": "semantic"}), "retrieval")
		r = c.expect(http.StatusOK, c.do("PUT", ob+"/steps/policies?use_defaults=true", nil), "use defaults")
		if r.body["next_step"] != "index_build" {
			t.Errorf("use_defaults next = %s", r.raw)
		}
		c.expect(http.StatusBadRequest, c.do("PUT", ob+"/steps/ranking", map[string]any{"mode": "learned"}), "learned model unavailable")
		r = c.expect(http.StatusOK, c.do("PUT", ob+"/steps/index_build", map[string]any{"upload_id": "01JUPLOAD000000000000000AA"}), "index build")
		job := r.body["data"].(map[string]any)["job"].(map[string]any)
		if job["status"] != "queued" || job["source"] != "upload" {
			t.Errorf("job = %v", job)
		}
		jobs := c.expect(http.StatusOK, c.do("GET", "/v1/tenants/"+otid+"/index/jobs/"+job["id"].(string), nil), "job")
		if jobs.body["config_version"].(float64) != 1 {
			t.Errorf("job = %s", jobs.raw)
		}
		c.expect(http.StatusOK, c.do("PUT", ob+"/steps/playground", nil), "playground")
		c.expect(http.StatusBadRequest, c.do("PUT", ob+"/steps/go_live", map[string]any{}), "go live needs a verified email")
		c.expect(http.StatusNoContent, c.do("POST", "/v1/auth/verify-email", map[string]string{"token": e.mailer.lastToken(t, "owner@initech.test")}), "verify")
		lr := c.expect(http.StatusOK, c.do("POST", "/v1/auth/login", map[string]string{"tenant": "initech", "email": "owner@initech.test", "password": password}), "relogin")
		c.token = lr.body["access_token"].(string)
		r = c.expect(http.StatusOK, c.do("PUT", ob+"/steps/go_live", map[string]any{}), "go live")
		data := r.body["data"].(map[string]any)
		if data["state"] != "active" || !strings.HasPrefix(data["api_key"].(string), "sek_") || r.body["next_step"] != "done" {
			t.Errorf("go live = %s", r.raw)
		}
		draft := c.expect(http.StatusOK, c.do("GET", "/v1/tenants/"+otid+"/config/draft", nil), "draft")
		ret := draft.body["document"].(map[string]any)["retrieval"].(map[string]any)["text"].(map[string]any)["retrievers"].([]any)
		if ret[1].(map[string]any)["weight"].(float64) != 1.5 {
			t.Errorf("semantic preset not saved: %v", ret)
		}
		exp := c.expect(http.StatusOK, c.do("GET", "/v1/tenants/"+otid+"/engine-explainer", nil), "explainer")
		if len(exp.body["stages"].([]any)) != 6 {
			t.Errorf("explainer = %s", exp.raw)
		}
		c.expect(http.StatusOK, e.client().do("GET", "/v1/onboarding/engine-explainer", nil), "public explainer")
	})

	t.Run("catalog preview, uploads and playground", func(t *testing.T) {
		pr := owner.expect(http.StatusOK, owner.do("POST", base+"/catalog/mapping/preview", map[string]any{"samples": []any{map[string]any{"id": "x", "name": "Bag"}}}), "preview with draft mapping")
		res := pr.body["results"].([]any)[0].(map[string]any)
		if res["product"].(map[string]any)["product_id"] != "x" || len(res["errors"].([]any)) == 0 {
			t.Errorf("preview = %s", pr.raw)
		}
		up := owner.expect(http.StatusCreated, owner.do("POST", base+"/catalog/uploads", map[string]string{"format": "jsonl"}), "upload")
		if !strings.Contains(up.body["url"].(string), "/catalog-uploads/uploads/"+tid+"/") || up.body["method"] != "PUT" {
			t.Errorf("upload = %s", up.raw)
		}
		owner.expect(http.StatusOK, owner.do("POST", base+"/playground/search", map[string]any{"q": "red", "config_version": 2}), "playground")
		e.search.mu.Lock()
		gotV, gotScope := e.search.version, e.search.scope
		e.search.mu.Unlock()
		if gotV != "2" || gotScope != auth.ScopeSearchPlayground {
			t.Errorf("proxy forwarded version %q scope %q", gotV, gotScope)
		}
		owner.expect(http.StatusBadRequest, owner.do("POST", base+"/playground/search", map[string]any{"q": "red", "config_version": "latest"}), "bad version ref")
	})

	t.Run("platform admin", func(t *testing.T) {
		admin := e.client()
		admin.expect(http.StatusUnauthorized, admin.do("POST", "/v1/admin/login", map[string]string{"email": "root@platform.test", "password": password}), "no admin yet")
		hash, _ := auth.HashPassword(password, auth.DefaultPasswordParams())
		if err := tenantdb.New(e.db).CreatePlatformAdmin(ctx, tenantdb.CreatePlatformAdminParams{ID: tenant.NewID().String(), Email: "root@platform.test", PasswordHash: hash}); err != nil {
			t.Fatal(err)
		}
		lr := admin.expect(http.StatusOK, admin.do("POST", "/v1/admin/login", map[string]string{"email": "root@platform.test", "password": password}), "admin login")
		admin.token = lr.body["access_token"].(string)
		owner.expect(http.StatusForbidden, owner.do("GET", "/v1/admin/settings", nil), "tenant owners are not admins")
		admin.expect(http.StatusOK, admin.do("GET", base+"/config/draft", nil), "admins can act on tenants")

		set := admin.expect(http.StatusOK, admin.do("GET", "/v1/admin/settings", nil), "settings")
		authSet := set.body["auth"].(map[string]any)
		if authSet["access_token_ttl"] != "15m0s" {
			t.Errorf("default settings = %s", set.raw)
		}
		authSet["access_token_ttl"] = "2h"
		bad := admin.expect(http.StatusBadRequest, admin.do("PUT", "/v1/admin/settings", set.body), "out of bounds")
		if errorCodes(bad)["/auth/access_token_ttl"] != "out_of_range" {
			t.Errorf("settings errors = %s", bad.raw)
		}
		authSet["access_token_ttl"] = "10m"
		admin.expect(http.StatusOK, admin.do("PUT", "/v1/admin/settings", set.body), "update settings")
		lr = owner.expect(http.StatusOK, owner.do("POST", "/v1/auth/login", map[string]string{"tenant": "acme", "email": "owner@acme.test", "password": password}), "login after settings change")
		if lr.body["expires_in"].(float64) != 600 {
			t.Errorf("new access TTL not applied: %v", lr.body["expires_in"])
		}

		plans := admin.expect(http.StatusOK, admin.do("GET", "/v1/admin/plans", nil), "plans")
		if len(plans.body["items"].([]any)) != 3 {
			t.Errorf("plans = %s", plans.raw)
		}
		admin.expect(http.StatusOK, admin.do("PUT", "/v1/admin/plans/pro", map[string]any{"name": "Pro", "max_total_ms": 800, "search_rps": 200, "search_burst": 400,
			"events_rps": 2000, "byo_allowed": true, "egress_allowlist": []string{"api.example.com"}, "max_custom_fields": 80, "max_page_depth": 1000}), "upsert plan")
		admin.expect(http.StatusBadRequest, admin.do("PUT", "/v1/admin/plans/bad", map[string]any{"name": "Bad", "max_total_ms": 1, "search_rps": 0}), "invalid plan")

		oldToken := owner.token
		rot := admin.expect(http.StatusOK, admin.do("POST", "/v1/admin/keys/rotate", nil), "rotate")
		jwks := admin.expect(http.StatusOK, admin.do("GET", "/.well-known/jwks.json", nil), "jwks")
		if len(jwks.body["keys"].([]any)) != 2 {
			t.Errorf("rotation must keep the previous key published: %s", jwks.raw)
		}
		if _, err := e.verifier.Verify(ctx, oldToken); err != nil {
			t.Errorf("tokens from the retired key must still verify: %v", err)
		}
		keys := admin.expect(http.StatusOK, admin.do("GET", "/v1/admin/keys", nil), "keys")
		var retired string
		for _, k := range keys.body["items"].([]any) {
			if m := k.(map[string]any); m["status"] == "retired" {
				retired = m["kid"].(string)
			}
		}
		admin.expect(http.StatusNoContent, admin.do("POST", "/v1/admin/keys/"+retired+"/revoke", nil), "revoke retired")
		if _, err := e.verifier.Verify(ctx, oldToken); err == nil {
			t.Error("tokens from a revoked key must stop verifying")
		}
		admin.expect(http.StatusUnauthorized, admin.do("GET", "/v1/admin/keys", nil), "the admin's own token was signed by the revoked key")
		lr = admin.expect(http.StatusOK, admin.do("POST", "/v1/admin/login", map[string]string{"email": "root@platform.test", "password": password}), "admin login again")
		admin.token = lr.body["access_token"].(string)
		admin.expect(http.StatusNotFound, admin.do("POST", "/v1/admin/keys/nope/revoke", nil), "unknown kid")
		if rot.body["active_kid"] == "" {
			t.Error("rotate must return the new kid")
		}
		admin.expect(http.StatusOK, admin.do("GET", "/v1/admin/tenants?limit=10", nil), "tenants")
	})
}
