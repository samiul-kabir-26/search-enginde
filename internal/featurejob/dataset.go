package featurejob

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"golang.org/x/sync/errgroup"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/providers/features"
	"github.com/zavisoft/search-engine/internal/providers/scorer/defaultlinear"
	"github.com/zavisoft/search-engine/internal/stages/prerank"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Dataset splits (docs/MODELS.md §5: by time, 80/10/10).
const (
	SplitTrain = "train"
	SplitValid = "valid"
	SplitTest  = "test"
)

// datasetChunk is how many requests are replayed concurrently before their rows are written,
// in request order, so the output is deterministic.
const datasetChunk = 256

// Stage runs one pipeline stage (S1, S2).
type Stage interface {
	Run(ctx context.Context, st *pipeline.State) (pipeline.Report, error)
}

// Dataset builds the learning-to-rank dataset of a tenant (docs/MODELS.md §5): one row per
// impression of a logged text request that got at least one click, cart or order, labelled
// order=3, add_to_cart=2, click=1, else 0, with the features the serving pipeline computes for
// that product and query. Features come from the serving code itself: S1 and S2 replay the
// query restricted to the request's impressed products, S4's builder computes request-local
// features, and online item features are aggregated as of the end of the training period, so
// no feature sees the period it is evaluated on.
type Dataset struct {
	CH Querier
	// Job aggregates item features as of a cutoff (its Store and Until are set per build).
	Job *Job
	// Understand and Retrieve are S1 and S2.
	Understand, Retrieve Stage
	// Vendors reads the tenant's vendor features (they are not time-dependent).
	Vendors pipeline.FeatureReader
	// MaxRequests caps the requests used (evenly sampled over time; 0 keeps all).
	MaxRequests int
	Concurrency int
	Log         *slog.Logger
}

// DatasetReport summarizes a build.
type DatasetReport struct {
	Requests map[string]int
	Rows     int
	// Skipped counts requests whose replay failed; Missing impressions of products no longer
	// in the index.
	Skipped, Missing int
	Cutoff           time.Time
	Took             time.Duration
}

type loggedRequest struct {
	id    string
	ts    time.Time
	query string
	split string
}

type impression struct {
	product  string
	position int
	label    int
	// randomized marks an impression shown in a randomized order: such requests carry no
	// position bias, so they give an unbiased offline evaluation.
	randomized bool
}

// Columns are the dataset's columns: identifiers, the label, the default_linear baselines of the
// tenant config, then every non-user catalog feature in docs/MODELS.md §4 order.
func Columns() []string {
	return append([]string{"request_id", "ts", "split", "product_id", "position", "label", "randomized", "baseline_prerank", "baseline_rank"},
		datasetFeatures()...)
}

func datasetFeatures() []string {
	cat := feature.Catalog()
	var out []string
	for _, n := range feature.Names() {
		if cat[n] != feature.SourceUser {
			out = append(out, n)
		}
	}
	return out
}

// Build writes tid's dataset as CSV to w, replaying requests with res's config and providers.
func (d *Dataset) Build(ctx context.Context, res *tenant.Resolved, providers *pipeline.Providers, w io.Writer) (DatasetReport, error) {
	start := time.Now()
	tid := res.Tenant.ID
	rep := DatasetReport{Requests: map[string]int{}}
	reqs, err := d.requests(ctx, tid, start.Add(-Window))
	if err != nil {
		return rep, err
	}
	if len(reqs) < 10 {
		return rep, fmt.Errorf("only %d logged text requests with interactions; run seed -events or wait for traffic", len(reqs))
	}
	nTrain, nValid := len(reqs)*8/10, len(reqs)/10
	for i := range reqs {
		switch {
		case i < nTrain:
			reqs[i].split = SplitTrain
		case i < nTrain+nValid:
			reqs[i].split = SplitValid
		default:
			reqs[i].split = SplitTest
		}
		rep.Requests[reqs[i].split]++
	}
	rep.Cutoff = reqs[nTrain].ts

	items := &memStore{items: map[string]features.Item{}}
	job := *d.Job
	job.Store, job.Until, job.SkipUsers = items, rep.Cutoff, true
	if _, err := job.Run(ctx, tid); err != nil {
		return rep, fmt.Errorf("item features as of %s: %w", rep.Cutoff.Format(time.RFC3339), err)
	}
	imps, err := d.impressions(ctx, tid, reqs, start.Add(-Window))
	if err != nil {
		return rep, err
	}

	cw := csv.NewWriter(bufio.NewWriterSize(w, 1<<20))
	if err := cw.Write(Columns()); err != nil {
		return rep, fmt.Errorf("write header: %w", err)
	}
	baselinePre, baselineRank := linearOf(res.Config.Prerank, defaultlinear.DefaultPrerankWeights()),
		linearOf(res.Config.Rank, defaultlinear.DefaultRankWeights())
	schema := pipeline.NewFeatureSchema(datasetFeatures())
	for c := 0; c < len(reqs); c += datasetChunk {
		chunk := reqs[c:min(c+datasetChunk, len(reqs))]
		rows := make([][][]string, len(chunk))
		var mu sync.Mutex
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(max(d.Concurrency, 1))
		for i, r := range chunk {
			g.Go(func() error {
				out, missing, err := d.replay(gctx, res, providers, r, imps[r.id], items.items, schema, baselinePre, baselineRank)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					rep.Skipped++
					d.Log.Warn("request replay failed; skipped", "request_id", r.id, "err", err)
					return nil
				}
				rows[i], rep.Missing = out, rep.Missing+missing
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return rep, fmt.Errorf("replay: %w", err)
		}
		for _, rs := range rows {
			for _, row := range rs {
				if err := cw.Write(row); err != nil {
					return rep, fmt.Errorf("write row: %w", err)
				}
				rep.Rows++
			}
		}
		if (c/datasetChunk)%10 == 0 {
			d.Log.Info("dataset progress", "requests", c+len(chunk), "of", len(reqs), "rows", rep.Rows)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return rep, fmt.Errorf("write dataset: %w", err)
	}
	if bw, ok := w.(interface{ Flush() error }); ok {
		_ = bw.Flush()
	}
	rep.Took = time.Since(start)
	return rep, nil
}

// replay runs S1 and S2 for r's query restricted to its impressed products, then the S4
// feature builder and the as-of online features, and returns one CSV row per impression.
func (d *Dataset) replay(ctx context.Context, res *tenant.Resolved, providers *pipeline.Providers, r loggedRequest,
	imps []impression, items map[string]features.Item, schema pipeline.FeatureSchema, pre, rank pipeline.Scorer) ([][]string, int, error) {
	ids := make([]any, len(imps))
	for i, im := range imps {
		ids[i] = im.product
	}
	q := &pipeline.Query{
		Tenant: res.Tenant.ID, ConfigVersion: res.ConfigVersion, Type: pipeline.QueryText, Raw: r.query, Depth: len(imps),
		Filter: filter.Expr{Cond: &filter.Condition{Field: "product_id", Op: filter.In, Value: ids, Type: catalog.TypeKeyword}},
	}
	st := &pipeline.State{Resolved: res, Providers: providers, Query: q}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := d.Understand.Run(rctx, st); err != nil {
		return nil, 0, fmt.Errorf("S1: %w", err)
	}
	rep, err := d.Retrieve.Run(rctx, st)
	if err != nil {
		return nil, 0, fmt.Errorf("S2: %w", err)
	}
	if rep.Fallback {
		return nil, 0, fmt.Errorf("S2 fell back: %s", rep.FallbackReason)
	}
	prerank.BuildFeatures(st, r.ts)
	vendorIDs := make([]string, 0, len(st.Candidates))
	for _, c := range st.Candidates {
		if c.Doc != nil {
			vendorIDs = append(vendorIDs, c.Doc.VendorID)
		}
	}
	online := &pipeline.OnlineFeatures{Items: map[string]map[string]float64{}}
	if d.Vendors != nil {
		v, err := d.Vendors.Read(rctx, res.Tenant.ID, pipeline.FeatureRequest{VendorIDs: vendorIDs})
		if err != nil {
			return nil, 0, fmt.Errorf("vendor features: %w", err)
		}
		online.Vendors = v.Vendors
	}
	for _, c := range st.Candidates {
		if it, ok := items[c.ProductID]; ok {
			online.Items[c.ProductID] = map[string]float64{feature.CTR7d: it.CTR7d, feature.CTR30d: it.CTR30d,
				feature.CVR7d: it.CVR7d, feature.CVR30d: it.CVR30d, feature.Orders30dLog: it.Orders30dLog}
		}
	}
	prerank.ApplyOnline(st.Candidates, online)
	preScores, err := pre.Score(rctx, q, st.Candidates)
	if err != nil {
		return nil, 0, err
	}
	rankScores, err := rank.Score(rctx, q, st.Candidates)
	if err != nil {
		return nil, 0, err
	}
	byID := make(map[string]int, len(st.Candidates))
	for i, c := range st.Candidates {
		byID[c.ProductID] = i
	}
	matrix := schema.Matrix(st.Candidates)
	f := len(schema.Names)
	var rows [][]string
	missing := 0
	for _, im := range imps {
		i, ok := byID[im.product]
		if !ok {
			missing++
			continue
		}
		row := []string{r.id, strconv.FormatInt(r.ts.UnixMilli(), 10), r.split, im.product, strconv.Itoa(im.position), strconv.Itoa(im.label),
			strconv.FormatBool(im.randomized), fmtFloat(float64(preScores[i])), fmtFloat(float64(rankScores[i]))}
		for _, v := range matrix[i*f : (i+1)*f] {
			row = append(row, fmtFloat(float64(v)))
		}
		rows = append(rows, row)
	}
	return rows, missing, nil
}

func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'g', 7, 32) }

// linearOf is the stage's default_linear scorer (its configured weights, else the defaults),
// the baseline a learned model must beat.
func linearOf(c tenant.StageScorerConfig, defaults map[string]float64) pipeline.Scorer {
	if c.Scorer.Type == tenant.ScorerDefaultLinear && len(c.Scorer.Weights) > 0 {
		return defaultlinear.New(c.Scorer.Weights)
	}
	return defaultlinear.New(defaults)
}

// requests lists the window's logged text requests that got at least one click, cart or order,
// in time order, evenly sampled down to MaxRequests.
func (d *Dataset) requests(ctx context.Context, tid tenant.ID, from time.Time) ([]loggedRequest, error) {
	rows, err := d.CH.Query(ctx, `
		SELECT request_id, toUnixTimestamp64Milli(ts), query FROM search_requests FINAL
		WHERE tenant_id = ? AND type = 'text' AND query != '' AND ts >= fromUnixTimestamp64Milli(?, 'UTC')
		  AND request_id IN (
		    SELECT DISTINCT request_id FROM events FINAL
		    WHERE tenant_id = ? AND type IN ('click', 'add_to_cart', 'order') AND ts >= fromUnixTimestamp64Milli(?, 'UTC'))
		ORDER BY ts, request_id`, string(tid), from.UnixMilli(), string(tid), from.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("requests query: %w", err)
	}
	defer rows.Close()
	var out []loggedRequest
	for rows.Next() {
		var r loggedRequest
		var ms int64
		if err := rows.Scan(&r.id, &ms, &r.query); err != nil {
			return nil, fmt.Errorf("requests scan: %w", err)
		}
		r.ts = time.UnixMilli(ms).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("requests rows: %w", err)
	}
	if d.MaxRequests > 0 && len(out) > d.MaxRequests {
		sampled := make([]loggedRequest, d.MaxRequests)
		for i := range sampled {
			sampled[i] = out[i*len(out)/d.MaxRequests]
		}
		out = sampled
	}
	return out, nil
}

// impressions loads each request's impressed products with their position and graded label.
func (d *Dataset) impressions(ctx context.Context, tid tenant.ID, reqs []loggedRequest, from time.Time) (map[string][]impression, error) {
	ids := make([]string, len(reqs))
	for i, r := range reqs {
		ids[i] = r.id
	}
	// The sampled request IDs are sent in the query (about 30 bytes each).
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_query_size": 64 << 20}))
	rows, err := d.CH.Query(ctx, `
		SELECT request_id, product_id, toUInt32(minIf(position, type = 'impression')),
		       toUInt8(max(multiIf(type = 'order', 3, type = 'add_to_cart', 2, type = 'click', 1, 0))),
		       toUInt8(max(randomized))
		FROM events FINAL
		WHERE tenant_id = ? AND ts >= fromUnixTimestamp64Milli(?, 'UTC') AND request_id IN ?
		GROUP BY request_id, product_id
		HAVING countIf(type = 'impression') > 0
		ORDER BY request_id, min(position)`, string(tid), from.UnixMilli(), ids)
	if err != nil {
		return nil, fmt.Errorf("impressions query: %w", err)
	}
	defer rows.Close()
	out := make(map[string][]impression, len(reqs))
	for rows.Next() {
		var req, pid string
		var pos uint32
		var label, randomized uint8
		if err := rows.Scan(&req, &pid, &pos, &label, &randomized); err != nil {
			return nil, fmt.Errorf("impressions scan: %w", err)
		}
		out[req] = append(out[req], impression{product: pid, position: int(pos), label: int(label), randomized: randomized > 0})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("impressions rows: %w", err)
	}
	return out, nil
}

// memStore keeps a run's features in memory (training datasets never write Redis).
type memStore struct {
	mu    sync.Mutex
	items map[string]features.Item
	prop  *features.Propensity
}

func (m *memStore) PutItems(_ context.Context, _ tenant.ID, items map[string]features.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, v := range items {
		m.items[k] = v
	}
	return nil
}

func (m *memStore) PutUsers(context.Context, tenant.ID, map[string]features.User) error { return nil }

func (m *memStore) PutPropensity(_ context.Context, _ tenant.ID, p features.Propensity) error {
	m.prop = &p
	return nil
}

func (m *memStore) GetPropensity(context.Context, tenant.ID) (features.Propensity, bool, error) {
	if m.prop == nil {
		return features.Propensity{}, false, nil
	}
	return *m.prop, true, nil
}
