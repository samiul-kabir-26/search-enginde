package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
	"github.com/zavisoft/search-engine/internal/providers/vectorstore"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// ProbeTimeout bounds one live call to a tenant endpoint during tests and validation.
const ProbeTimeout = 5 * time.Second

// Check is one step of a connection test or model validation.
type Check struct {
	Name      string   `json:"name"`
	OK        bool     `json:"ok"`
	Message   string   `json:"message,omitempty"`
	LatencyMS *float64 `json:"latency_ms,omitempty"`
}

func timed(name string, fn func() (string, error)) Check {
	start := time.Now()
	msg, err := fn()
	ms := math.Round(float64(time.Since(start).Microseconds())/10) / 100
	if err != nil {
		return Check{Name: name, Message: err.Error(), LatencyMS: &ms}
	}
	return Check{Name: name, OK: true, Message: msg, LatencyMS: &ms}
}

// ConnectionTestResult is the outcome of POST /connections/{id}/test.
type ConnectionTestResult struct {
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
}

func firstError(checks []Check) string {
	for _, c := range checks {
		if !c.OK {
			return c.Name + ": " + c.Message
		}
	}
	return ""
}

func (r ConnectionTestResult) firstError() string { return firstError(r.Checks) }

// testConnection checks a connection against a collection: the endpoint answers, the
// collection exists with the declared vectors (dimension and metric, supported by the
// adapter), the product ID and filterFields are stored fields, and a sample query works.
func (s *Server) testConnection(ctx context.Context, tid tenant.ID, plan tenant.Plan, conn tenant.Connection, block tenant.StoreConfig, filterFields []string) ConnectionTestResult {
	res := ConnectionTestResult{}
	add := func(c Check) bool {
		res.Checks = append(res.Checks, c)
		return c.OK
	}
	conn.Kind = tenant.ConnectionVectorStore
	st, err := registry.NewBYOVectorStore(ctx, s.BYO, tid, plan, conn, block)
	if !add(Check{Name: "configure", OK: err == nil, Message: errMessage(err)}) {
		return res
	}
	defer func() { _ = st.Close() }()
	probe := func(name string, fn func(context.Context) (string, error)) bool {
		pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
		defer cancel()
		return add(timed(name, func() (string, error) { return fn(pctx) }))
	}
	if !probe("health", func(ctx context.Context) (string, error) { return "", st.Health(ctx) }) {
		return res
	}
	var info vectorstore.Info
	if !probe("collection", func(ctx context.Context) (string, error) {
		if info, err = st.Describe(ctx); err != nil {
			return "", err
		}
		if !info.Exists {
			return "", fmt.Errorf("collection %q does not exist", block.Collection)
		}
		return fmt.Sprintf("%d vector fields", len(info.Vectors)), nil
	}) {
		return res
	}
	spec := registry.StoreSpec(block)
	var vecProblems, fieldProblems []string
	for _, p := range vectorstore.Check(spec, info, st.Metrics(), filterFields) {
		if strings.HasPrefix(p.Path, "/payload_mapping") {
			fieldProblems = append(fieldProblems, p.Message)
		} else {
			vecProblems = append(vecProblems, p.Message)
		}
	}
	if len(spec.Vectors) == 0 {
		vecProblems = append(vecProblems, "no vectors declared (vectors.text and/or vectors.image)")
	}
	add(Check{Name: "vectors", OK: len(vecProblems) == 0, Message: strings.Join(vecProblems, "; ")})
	fields := Check{Name: "fields", OK: len(fieldProblems) == 0, Message: strings.Join(fieldProblems, "; ")}
	if fields.OK && !info.AnyField {
		fields.Message = s.unfilterable(spec, info)
	}
	add(fields)
	if !res.okSoFar() {
		return res
	}
	name, dim := firstVector(spec)
	probe("sample_query", func(ctx context.Context) (string, error) {
		hits, err := st.Search(ctx, pipeline.VectorSearch{Vector: name, Query: unitVector(dim), K: 1})
		return fmt.Sprintf("%d hit(s)", len(hits)), err
	})
	res.OK = res.okSoFar()
	return res
}

func (r ConnectionTestResult) okSoFar() bool {
	for _, c := range r.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

// unfilterable lists the canonical filterable fields a fixed-schema store does not hold:
// request filters on them cannot be pushed down, so the vector retrievers fall back.
func (s *Server) unfilterable(spec vectorstore.Spec, info vectorstore.Info) string {
	var missing []string
	for _, f := range []string{"vendor_id", "brand", "category_ids", "price_minor", "currency", "status", "stock", "ship_regions", "rating"} {
		if !info.Fields[spec.Field(f)] {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "not stored, so request filters on them use the lexical fallback: " + strings.Join(missing, ", ")
}

func firstVector(spec vectorstore.Spec) (string, int) {
	for _, n := range []string{pipeline.VectorText, pipeline.VectorImage} {
		if v, ok := spec.Vector(n); ok {
			return n, v.Dim
		}
	}
	return pipeline.VectorText, 1
}

func unitVector(dim int) []float32 {
	v := make([]float32, max(dim, 1))
	v[0] = 1
	return v
}

func errMessage(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// ModelValidation is the outcome of POST /models/{id}/validate.
type ModelValidation struct {
	OK        bool    `json:"ok"`
	Checks    []Check `json:"checks"`
	LatencyMS float64 `json:"latency_ms"`
}

func (v ModelValidation) firstError() string { return firstError(v.Checks) }

// Probe inputs: two texts (batching) and a small generated JPEG.
var probeTexts = []string{"red leather handbag", "stainless steel water bottle"}

func probeJPEG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := range 64 {
		for x := range 64 {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: uint8(y * 4), B: 128, A: 255})
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, &jpeg.Options{Quality: 85})
	return b.Bytes()
}

// checkVectors verifies a probe's output: n vectors of the declared dimension, L2-normalized.
func checkVectors(vecs [][]float32, n, dim int) (string, error) {
	if len(vecs) != n {
		return "", fmt.Errorf("got %d vectors for %d inputs", len(vecs), n)
	}
	for _, v := range vecs {
		if len(v) != dim {
			return "", fmt.Errorf("vectors have %d dimensions, declared %d", len(v), dim)
		}
		var sum float64
		for _, x := range v {
			sum += float64(x) * float64(x)
		}
		if math.Abs(math.Sqrt(sum)-1) > 1e-2 {
			return "", fmt.Errorf("EMBEDDING must be L2-normalized (norm %.3f)", math.Sqrt(sum))
		}
	}
	return fmt.Sprintf("%d×%d, normalized", n, dim), nil
}

// probeEmbedder runs the probe inferences the model's modality calls for.
func probeEmbedder(ctx context.Context, e pipeline.Embedder, m tenant.Model) []Check {
	var checks []Check
	run := func(name string, fn func(context.Context) (string, error)) bool {
		pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
		defer cancel()
		c := timed(name, func() (string, error) { return fn(pctx) })
		checks = append(checks, c)
		return c.OK
	}
	dim := m.Output.Dim
	if m.Input.Modality == tenant.ModalityImage {
		if !run("image", func(ctx context.Context) (string, error) {
			v, err := e.EmbedImage(ctx, [][]byte{probeJPEG()})
			if err != nil {
				return "", err
			}
			return checkVectors(v, 1, dim)
		}) || !m.Settings.SupportsTextToImage {
			return checks
		}
		run("text_to_image", func(ctx context.Context) (string, error) {
			v, err := e.EmbedText(ctx, probeTexts)
			if err != nil {
				return "", err
			}
			return checkVectors(v, len(probeTexts), dim)
		})
		return checks
	}
	run("text", func(ctx context.Context) (string, error) {
		v, err := e.EmbedText(ctx, probeTexts)
		if err != nil {
			return "", err
		}
		return checkVectors(v, len(probeTexts), dim)
	})
	return checks
}

func allOK(checks []Check) bool {
	return !slices.ContainsFunc(checks, func(c Check) bool { return !c.OK })
}

// validateModel probes a model. A kserve_v2 model is tried over gRPC first and over REST when
// gRPC fails; the transport that works is returned in the settings.
func (s *Server) validateModel(ctx context.Context, tid tenant.ID, plan tenant.Plan, m tenant.Model) (ModelValidation, tenant.ModelSettings) {
	start := time.Now()
	transports := []string{""}
	if m.Type == tenant.ModelKServeV2 {
		transports = []string{kservev2.TransportGRPC, kservev2.TransportREST}
	}
	var res ModelValidation
	settings := m.Settings
	for _, tr := range transports {
		mm := m
		mm.Settings.Transport = tr
		checks := []Check{}
		if tr != "" {
			checks = append(checks, Check{Name: "transport", OK: true, Message: tr})
		}
		e, err := registry.NewBYOEmbedder(ctx, s.BYO, tid, plan, mm)
		if err != nil {
			checks = append(checks, Check{Name: "configure", Message: err.Error()})
		} else {
			checks = append(checks, probeEmbedder(ctx, e, mm)...)
			if c, ok := e.(io.Closer); ok {
				_ = c.Close()
			}
		}
		res.Checks = append(res.Checks, checks...)
		if allOK(checks) {
			res.OK, settings.Transport = true, tr
			// Keep only the checks of the transport that worked.
			res.Checks = checks
			break
		}
	}
	res.LatencyMS = math.Round(float64(time.Since(start).Microseconds())/10) / 100
	return res, settings
}

// probeCache remembers successful validation probes briefly, so editing a draft does not call
// the tenant's endpoints on every keystroke. Failures are always re-probed.
type probeCache struct {
	mu sync.Mutex
	m  map[string]time.Time
}

const probeCacheTTL = 30 * time.Second

func (c *probeCache) ok(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	exp, found := c.m[key]
	return found && time.Now().Before(exp)
}

func (c *probeCache) remember(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]time.Time{}
	}
	c.m[key] = time.Now().Add(probeCacheTTL)
}

// forget drops a tenant's cached probes after its models or connections change.
func (c *probeCache) forget(tid tenant.ID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.m {
		if strings.HasPrefix(k, string(tid)+"/") {
			delete(c.m, k)
		}
	}
}

// byoChecker implements tenant.BYOChecker for config validation with live probes.
type byoChecker struct {
	s    *Server
	tid  tenant.ID
	plan tenant.Plan
}

func (c byoChecker) Embedder(ctx context.Context, modelID string) (tenant.EmbedderInfo, []platform.FieldError) {
	m, err := c.s.Tenants.Model(ctx, c.tid, modelID)
	if errors.Is(err, tenant.ErrNotFound) {
		return tenant.EmbedderInfo{}, []platform.FieldError{fieldErr("/model_id", "not_found", "no such model")}
	}
	if err != nil {
		return tenant.EmbedderInfo{}, []platform.FieldError{fieldErr("/model_id", "unavailable", err.Error())}
	}
	if m.Kind != "embedder" {
		return tenant.EmbedderInfo{}, []platform.FieldError{fieldErr("/model_id", "wrong_kind", fmt.Sprintf("model %s is a %s model", m.ID, m.Kind))}
	}
	if m.Status != tenant.StatusValidated {
		return tenant.EmbedderInfo{}, []platform.FieldError{fieldErr("/model_id", "model_not_validated", fmt.Sprintf("model %s is %s; validate it first", m.ID, m.Status))}
	}
	key := string(c.tid) + "/model/" + m.ID
	if !c.s.probes.ok(key) {
		e, err := registry.NewBYOEmbedder(ctx, c.s.BYO, c.tid, c.plan, m)
		if err != nil {
			return tenant.EmbedderInfo{}, []platform.FieldError{fieldErr("/model_id", "probe_failed", err.Error())}
		}
		checks := probeEmbedder(ctx, e, m)
		if cl, ok := e.(io.Closer); ok {
			_ = cl.Close()
		}
		if !allOK(checks) {
			return tenant.EmbedderInfo{}, []platform.FieldError{fieldErr("/model_id", "probe_failed", firstError(checks))}
		}
		c.s.probes.remember(key)
	}
	return m.EmbedderInfo(), nil
}

func (c byoChecker) VectorStore(ctx context.Context, block tenant.StoreConfig, filterFields []string) []platform.FieldError {
	conn, err := c.s.Tenants.Connection(ctx, c.tid, block.ConnectionID)
	if errors.Is(err, tenant.ErrNotFound) {
		return []platform.FieldError{fieldErr("/connection_id", "not_found", "no such connection")}
	}
	if err != nil {
		return []platform.FieldError{fieldErr("/connection_id", "unavailable", err.Error())}
	}
	if conn.Kind != tenant.ConnectionVectorStore {
		return []platform.FieldError{fieldErr("/connection_id", "wrong_kind", "the connection is not a vector store")}
	}
	if conn.Status != tenant.StatusValidated {
		return []platform.FieldError{fieldErr("/connection_id", "connection_not_validated", fmt.Sprintf("connection %s is %s; test it first", conn.ID, conn.Status))}
	}
	key := fmt.Sprintf("%s/store/%s/%v/%v", c.tid, conn.ID, block, filterFields)
	if c.s.probes.ok(key) {
		return nil
	}
	st, err := registry.NewBYOVectorStore(ctx, c.s.BYO, c.tid, c.plan, conn, block)
	if err != nil {
		return []platform.FieldError{fieldErr("/connection_id", "probe_failed", err.Error())}
	}
	defer func() { _ = st.Close() }()
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	info, err := st.Describe(pctx)
	if err != nil {
		return []platform.FieldError{fieldErr("/connection_id", "probe_failed", "describe "+endpointHost(conn.Endpoint)+": "+err.Error())}
	}
	var errs []platform.FieldError
	for _, p := range vectorstore.Check(registry.StoreSpec(block), info, st.Metrics(), filterFields) {
		errs = append(errs, platform.FieldError{Path: p.Path, Code: p.Code, Message: p.Message})
	}
	if len(errs) == 0 {
		c.s.probes.remember(key)
	}
	return errs
}
