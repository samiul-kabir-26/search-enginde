package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	embedder "github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
	"github.com/zavisoft/search-engine/internal/providers/scorer/artifacts"
	"github.com/zavisoft/search-engine/internal/providers/scorer/kservev2"
	"github.com/zavisoft/search-engine/internal/providers/scorer/onnx"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// onnxScorer loads an in-process ONNX scorer: a platform model (prerank-gbdt-v1) or a tenant's
// upload. A model that cannot be loaded becomes a failing scorer, so the stage passes
// candidates through instead of the request failing; it is rebuilt after byoRetry.
func onnxScorer(ctx context.Context, d Deps, s Spec) (any, error) {
	c := s.Block.(tenant.StageScorerConfig).Scorer
	if c.Model != "" {
		name := "platform:" + c.Model
		if d.Platform.Models == nil {
			return brokenScorerOf(name, errors.New("no model store configured")), nil
		}
		data, m, err := d.Platform.Models.PlatformModel(ctx, c.Model)
		if err != nil {
			return brokenScorerOf(name, err), nil
		}
		sc, err := onnx.New(data, pipeline.NewFeatureSchema(m.Features), onnx.Options{IntraOpThreads: d.Platform.ONNXThreads, Name: name})
		if err != nil {
			return brokenScorerOf(name, err), nil
		}
		return sc, nil
	}
	m, err := scorerModel(ctx, d, s, c, tenant.ModelONNX)
	name := "byo:onnx:" + c.ModelID
	if err != nil {
		return brokenScorerOf(name, err), nil
	}
	if d.Platform.Models == nil {
		return brokenScorerOf(name, errors.New("no model store configured")), nil
	}
	data, err := d.Platform.Models.Get(ctx, m.ArtifactURI)
	if err == nil && artifacts.SHA256(data) != m.ArtifactSHA256 {
		err = fmt.Errorf("model %s artifact does not match its recorded sha256", m.ID)
	}
	if err != nil {
		return brokenScorerOf(name, err), nil
	}
	sc, err := onnx.New(data, pipeline.NewFeatureSchema(m.Input.Features), onnx.Options{IntraOpThreads: d.Platform.ONNXThreads, Name: "byo:onnx:" + m.Name})
	if err != nil {
		return brokenScorerOf(name, err), nil
	}
	return sc, nil
}

// kserveScorer builds a remote scorer: a platform model in Triton (rank-gbdt-v1) or a tenant's
// registered kserve_v2 endpoint, which takes a slot of the tenant's bulkhead per call.
func kserveScorer(ctx context.Context, d Deps, s Spec) (any, error) {
	c := s.Block.(tenant.StageScorerConfig).Scorer
	if c.Model != "" {
		name := "platform:" + c.Model
		ps, ok := artifacts.Platform[c.Model]
		if !ok || ps.Type != artifacts.TypeKServeV2 {
			return brokenScorerOf(name, fmt.Errorf("%q is not a platform kserve_v2 scorer", c.Model)), nil
		}
		if d.Platform.Models == nil || d.Platform.Triton == nil {
			return brokenScorerOf(name, errors.New("no model store or Triton configured")), nil
		}
		m, err := d.Platform.Models.Manifest(ctx, c.Model)
		if err != nil {
			return brokenScorerOf(name, err), nil
		}
		return kservev2.NewPlatform(d.Platform.Triton, ps.Triton, pipeline.NewFeatureSchema(m.Features), name), nil
	}
	name := "byo:kserve_v2:" + c.ModelID
	m, err := scorerModel(ctx, d, s, c, tenant.ModelKServeV2)
	if err != nil {
		return brokenScorerOf(name, err), nil
	}
	sc, err := NewBYOScorer(ctx, d.BYO, s.Resolved.Tenant.ID, s.Resolved.Plan, m)
	if err != nil {
		return brokenScorerOf(name, err), nil
	}
	if d.BYO.Bulkheads == nil {
		return sc, nil
	}
	return &guardedScorer{BYOScorer: sc, tenant: string(s.Resolved.Tenant.ID), b: d.BYO.Bulkheads}, nil
}

// scorerModel loads and checks a tenant's registered scorer model for the stage.
func scorerModel(ctx context.Context, d Deps, s Spec, c tenant.ScorerConfig, typ string) (tenant.Model, error) {
	if d.BYO == nil {
		return tenant.Model{}, errors.New("tenant models are not configured")
	}
	m, err := d.BYO.Source.Model(ctx, s.Resolved.Tenant.ID, c.ModelID)
	switch {
	case err != nil:
		return m, err
	case m.Kind != s.Role || m.Type != typ:
		return m, fmt.Errorf("model %s is a %s %s model, not a %s %s model", m.ID, m.Type, m.Kind, typ, s.Role)
	case m.Status != tenant.StatusValidated:
		return m, fmt.Errorf("model %s is %s, not validated", m.ID, m.Status)
	}
	return m, nil
}

// BYOScorer is a tenant's remote scorer: the pipeline interface plus the probe validation runs.
type BYOScorer interface {
	pipeline.Scorer
	Probe(ctx context.Context) error
	ProviderName() string
	io.Closer
}

// NewBYOScorer builds a registered kserve_v2 scorer model. It reads the model's secret but
// dials nothing.
func NewBYOScorer(ctx context.Context, b *BYO, t tenant.ID, plan tenant.Plan, m tenant.Model) (BYOScorer, error) {
	if m.Type != tenant.ModelKServeV2 {
		return nil, fmt.Errorf("model %s is %s; only kserve_v2 scorers are remote", m.ID, m.Type)
	}
	token, err := b.secret(ctx, t, m.SecretRef)
	if err != nil {
		return nil, err
	}
	allow := plan.EgressAllowlist
	sc, err := kservev2.NewRemote(embedder.Remote{
		Endpoint: m.Endpoint, Transport: m.Settings.Transport, Token: token, Plaintext: b.plaintext(m.Endpoint),
		Dial: b.Egress.GRPCDialer(allow), HTTP: b.Egress.HTTPClient(allow, b.httpTimeout()), Name: "byo:kserve_v2:" + m.Name,
	}, m.Name, m.Version, pipeline.NewFeatureSchema(m.Input.Features))
	if err != nil {
		return nil, fmt.Errorf("model %s: %w", m.ID, err)
	}
	return sc, nil
}

// brokenScorer fails every call (errBYOUnavailable), so scoring falls back to the previous
// order; it is rebuilt after byoRetry.
type brokenScorer struct {
	err  error
	name string
}

func brokenScorerOf(name string, err error) *brokenScorer { return &brokenScorer{err: err, name: name} }

func (b *brokenScorer) Score(context.Context, *pipeline.Query, []pipeline.Candidate) ([]float32, error) {
	return nil, fmt.Errorf("%w: %w", errBYOUnavailable, b.err)
}
func (b *brokenScorer) FeatureSchema() pipeline.FeatureSchema { return pipeline.FeatureSchema{} }
func (b *brokenScorer) ProviderName() string                  { return b.name }
func (b *brokenScorer) retryAfter() time.Duration             { return byoRetry }

// guardedScorer takes a slot of the tenant's bulkhead for every call.
type guardedScorer struct {
	BYOScorer
	tenant string
	b      *resilience.Bulkheads
}

func (g *guardedScorer) Score(ctx context.Context, q *pipeline.Query, cs []pipeline.Candidate) ([]float32, error) {
	release, err := g.b.Acquire(g.tenant)
	if err != nil {
		return nil, err
	}
	defer release()
	return g.BYOScorer.Score(ctx, q, cs)
}
