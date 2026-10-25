package pipeline

import (
	"context"
	"fmt"
	"maps"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/zavisoft/search-engine/internal/tenant"
)

// Stage names, in pipeline order.
const (
	StageQueryUnderstanding = "query_understanding"
	StageRetrieval          = "retrieval"
	StageFilter             = "filter"
	StagePrerank            = "prerank"
	StageRank               = "rank"
	StageRerank             = "rerank"
)

// TraceCandidates is how many candidates per stage the explain trace records.
const TraceCandidates = 50

// State is what flows between stages.
type State struct {
	Resolved   *tenant.Resolved
	Providers  *Providers
	Query      *Query
	Candidates []Candidate
	Facets     map[string][]FacetValue
	// TotalMatches is the approximate lexical match count.
	TotalMatches int64
	// Degraded is set when retrieval returned nothing because every retriever failed, or a
	// stage could not run; the response is still 200.
	Degraded bool
}

// Report is what a stage says about its run for the trace.
type Report struct {
	Provider       string
	Version        string
	Fallback       bool
	FallbackReason string
}

// Stage is one pipeline step. Run mutates st; an error marks the response degraded but the
// pipeline continues, because later stages tolerate empty or unscored input.
type Stage interface {
	Name() string
	Run(ctx context.Context, st *State) (Report, error)
}

// StageTrace is one stage's entry in the explain trace (api/openapi/search.yaml Trace).
type StageTrace struct {
	Stage          string           `json:"stage"`
	Provider       string           `json:"provider"`
	Version        string           `json:"version,omitempty"`
	LatencyMS      float64          `json:"latency_ms"`
	InCount        int              `json:"in_count"`
	OutCount       int              `json:"out_count"`
	Fallback       bool             `json:"fallback"`
	FallbackReason string           `json:"fallback_reason,omitempty"`
	Error          string           `json:"error,omitempty"`
	Candidates     []TraceCandidate `json:"candidates,omitempty"`
}

// TraceCandidate is one candidate's scores after a stage.
type TraceCandidate struct {
	ProductID string             `json:"product_id"`
	Scores    map[string]float64 `json:"scores"`
}

// Trace is the per-stage explain trace.
type Trace struct {
	Stages []StageTrace `json:"stages"`
}

// Engine runs stages under the tenant's budgets.
type Engine struct {
	stages   []Stage
	budgetOf func(b tenant.Budgets, stage string) int
	tracer   trace.Tracer
	stageDur metric.Float64Histogram
	reqDur   metric.Float64Histogram
	empty    metric.Int64Counter
	now      func() time.Time
}

// NewEngine builds an engine over the stages and registers the search metrics.
func NewEngine(meter metric.Meter, stages ...Stage) (*Engine, error) {
	e := &Engine{stages: stages, budgetOf: stageBudget, tracer: otel.Tracer("search/pipeline"), now: time.Now}
	var err error
	if e.stageDur, err = meter.Float64Histogram("search_stage_duration_seconds",
		metric.WithDescription("Pipeline stage latency."), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1)); err != nil {
		return nil, fmt.Errorf("stage histogram: %w", err)
	}
	if e.reqDur, err = meter.Float64Histogram("search_request_duration_seconds",
		metric.WithDescription("End-to-end pipeline latency."), metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1, 2)); err != nil {
		return nil, fmt.Errorf("request histogram: %w", err)
	}
	if e.empty, err = meter.Int64Counter("search_results_empty_total",
		metric.WithDescription("Searches that returned no results.")); err != nil {
		return nil, fmt.Errorf("empty counter: %w", err)
	}
	return e, nil
}

func stageBudget(b tenant.Budgets, stage string) int {
	switch stage {
	case StageQueryUnderstanding:
		return b.S1MS
	case StageRetrieval:
		return b.S2MS
	case StageFilter:
		return b.S3MS
	case StagePrerank:
		return b.S4MS
	case StageRank:
		return b.S5MS
	case StageRerank:
		return b.S6MS
	}
	return 0
}

// Run executes every stage with deadline min(stage budget, remaining request budget). The trace
// always records counts and latency; candidate scores are recorded only when q.Explain is set.
func (e *Engine) Run(ctx context.Context, res *tenant.Resolved, p *Providers, q *Query) (*State, *Trace) {
	start := e.now()
	budgets := res.Config.Budgets
	if q.Type == QueryImage || q.Type == QuerySimilar {
		budgets = budgets.ForImage()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(budgets.TotalMS)*time.Millisecond)
	defer cancel()

	st := &State{Resolved: res, Providers: p, Query: q}
	tr := &Trace{}
	tenantAttr := attribute.String("tenant", string(res.Tenant.ID))
	for _, s := range e.stages {
		in := len(st.Candidates)
		sctx, scancel := context.WithTimeout(ctx, time.Duration(e.budgetOf(budgets, s.Name()))*time.Millisecond)
		sctx, span := e.tracer.Start(sctx, "stage."+s.Name(), trace.WithAttributes(tenantAttr, attribute.String("stage", s.Name())))
		t0 := e.now()
		rep, err := s.Run(sctx, st)
		elapsed := e.now().Sub(t0)
		scancel()

		entry := StageTrace{
			Stage: s.Name(), Provider: rep.Provider, Version: rep.Version,
			LatencyMS: float64(elapsed.Microseconds()) / 1000, InCount: in, OutCount: len(st.Candidates),
			Fallback: rep.Fallback, FallbackReason: rep.FallbackReason,
		}
		if err != nil {
			entry.Error = err.Error()
			st.Degraded = true
			span.RecordError(err)
		}
		if q.Explain {
			entry.Candidates = traceCandidates(st.Candidates)
		}
		span.SetAttributes(attribute.String("provider", rep.Provider), attribute.Bool("fallback", rep.Fallback),
			attribute.Int("in_count", in), attribute.Int("out_count", len(st.Candidates)))
		span.End()
		e.stageDur.Record(ctx, elapsed.Seconds(), metric.WithAttributes(tenantAttr,
			attribute.String("stage", s.Name()), attribute.String("provider", rep.Provider)))
		tr.Stages = append(tr.Stages, entry)
	}
	e.reqDur.Record(ctx, e.now().Sub(start).Seconds(), metric.WithAttributes(tenantAttr, attribute.String("query_type", string(q.Type))))
	if len(st.Candidates) == 0 {
		e.empty.Add(ctx, 1, metric.WithAttributes(tenantAttr))
	}
	return st, tr
}

func traceCandidates(cs []Candidate) []TraceCandidate {
	n := min(len(cs), TraceCandidates)
	out := make([]TraceCandidate, n)
	for i := range n {
		scores := maps.Clone(cs[i].Features)
		if scores == nil {
			scores = map[string]float64{}
		}
		scores["score"] = cs[i].Score
		scores["rrf_score_raw"] = cs[i].RRF
		out[i] = TraceCandidate{ProductID: cs[i].ProductID, Scores: scores}
	}
	return out
}
