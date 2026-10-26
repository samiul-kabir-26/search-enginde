package featurejob

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/providers/features"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Aggregation settings (docs/MODELS.md §4).
const (
	// Window is the longest feature window; events older than it are not read.
	Window      = 30 * 24 * time.Hour
	ShortWindow = 7 * 24 * time.Hour
	// PriorExaminations and PriorClicks are the strengths of the Bayesian priors that shrink CTR
	// and CVR toward the tenant's mean, so an item with a handful of impressions is not ranked by
	// noise.
	PriorExaminations = 20.0
	PriorClicks       = 10.0
	// Propensity fit: at least MinPropensityClicks randomized clicks over MinPropensityPositions
	// positions with MinPositionImpressions each; Eta is clamped to [0, MaxEta].
	MinPropensityClicks    = 200
	MinPropensityPositions = 5
	MinPositionImpressions = 50
	MaxEta                 = 2.0
	// TopCategories is how many category affinities are kept per user.
	TopCategories = 20
	// MinPriceInteractions is how many interactions a user needs for a price band.
	MinPriceInteractions = 3
	// userChunk is how many users are resolved against the catalog per batch.
	userChunk = 1000
	docBatch  = 1000
)

// Querier runs ClickHouse queries (driver.Conn).
type Querier interface {
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
}

// Store is where features are written.
type Store interface {
	PutItems(ctx context.Context, tid tenant.ID, items map[string]features.Item) error
	PutUsers(ctx context.Context, tid tenant.ID, users map[string]features.User) error
	PutPropensity(ctx context.Context, tid tenant.ID, p features.Propensity) error
	GetPropensity(ctx context.Context, tid tenant.ID) (features.Propensity, bool, error)
}

// DocSource fetches stored product data (categories, price) from a tenant's lexical index.
type DocSource interface {
	Docs(ctx context.Context, ids []string) (map[string]*pipeline.Doc, error)
}

// Job aggregates one tenant's ClickHouse events into Redis online features (PLAN.md R9).
type Job struct {
	CH    Querier
	Store Store
	// Docs returns the tenant's catalog for user features.
	Docs func(tid tenant.ID) DocSource
	Log  *slog.Logger
	Now  func() time.Time
}

// Report summarizes one run.
type Report struct {
	Tenant tenant.ID
	// Eta is the propensity exponent used; EtaSource is estimated, previous or none (no
	// debiasing until randomized traffic exists).
	Eta              float64
	EtaSource        string
	RandomizedClicks int64
	Items            int
	Users            int
	Took             time.Duration
}

// Run refreshes the tenant's propensity, item and user features.
func (j *Job) Run(ctx context.Context, tid tenant.ID) (Report, error) {
	start := j.Now()
	rep := Report{Tenant: tid}
	from := start.Add(-Window)
	prop, src, err := j.propensity(ctx, tid, from)
	if err != nil {
		return rep, err
	}
	rep.Eta, rep.EtaSource, rep.RandomizedClicks = prop.Eta, src, prop.Clicks
	if rep.Items, err = j.items(ctx, tid, prop.Eta, start); err != nil {
		return rep, err
	}
	if rep.Users, err = j.users(ctx, tid, from); err != nil {
		return rep, err
	}
	rep.Took = j.Now().Sub(start)
	return rep, nil
}

// PositionStat is one position's randomized impressions and clicks.
type PositionStat struct {
	Position    int
	Impressions int64
	Clicks      int64
}

// FitPropensity estimates p(pos) = pos^-eta from randomized-slot traffic: in a randomized
// request every item is equally relevant in expectation at every position, so CTR at position
// k is proportional to the examination probability. It fits ln CTR_k = c − eta·ln k by least
// squares weighted by clicks. ok is false when there is too little traffic.
func FitPropensity(stats []PositionStat) (eta float64, clicks int64, ok bool) {
	var xs, ys, ws []float64
	for _, s := range stats {
		clicks += s.Clicks
		if s.Position < 1 || s.Impressions < MinPositionImpressions || s.Clicks == 0 {
			continue
		}
		xs = append(xs, math.Log(float64(s.Position)))
		ys = append(ys, math.Log(float64(s.Clicks)/float64(s.Impressions)))
		ws = append(ws, float64(s.Clicks))
	}
	if clicks < MinPropensityClicks || len(xs) < MinPropensityPositions {
		return 0, clicks, false
	}
	var sw, sx, sy float64
	for i := range xs {
		sw += ws[i]
		sx += ws[i] * xs[i]
		sy += ws[i] * ys[i]
	}
	mx, my := sx/sw, sy/sw
	var sxy, sxx float64
	for i := range xs {
		sxy += ws[i] * (xs[i] - mx) * (ys[i] - my)
		sxx += ws[i] * (xs[i] - mx) * (xs[i] - mx)
	}
	if sxx == 0 {
		return 0, clicks, false
	}
	return math.Min(math.Max(-sxy/sxx, 0), MaxEta), clicks, true
}

// propensity estimates eta from the window's randomized requests, falling back to the stored
// estimate, then to no debiasing.
func (j *Job) propensity(ctx context.Context, tid tenant.ID, from time.Time) (features.Propensity, string, error) {
	rows, err := j.CH.Query(ctx, `
		SELECT toUInt32(i.position), count(), toUInt64(sum(c.clicked))
		FROM (
			SELECT request_id, product_id, position FROM events FINAL
			WHERE tenant_id = ? AND type = 'impression' AND randomized AND ts >= fromUnixTimestamp64Milli(?, 'UTC')
		) AS i
		LEFT JOIN (
			SELECT DISTINCT request_id, product_id, toUInt8(1) AS clicked FROM events FINAL
			WHERE tenant_id = ? AND type = 'click' AND ts >= fromUnixTimestamp64Milli(?, 'UTC')
		) AS c ON i.request_id = c.request_id AND i.product_id = c.product_id
		GROUP BY i.position ORDER BY i.position`,
		string(tid), from.UnixMilli(), string(tid), from.UnixMilli())
	if err != nil {
		return features.Propensity{}, "", fmt.Errorf("propensity query: %w", err)
	}
	defer rows.Close()
	var stats []PositionStat
	for rows.Next() {
		var pos uint32
		var imps, clicks uint64
		if err := rows.Scan(&pos, &imps, &clicks); err != nil {
			return features.Propensity{}, "", fmt.Errorf("propensity scan: %w", err)
		}
		stats = append(stats, PositionStat{Position: int(pos), Impressions: int64(imps), Clicks: int64(clicks)})
	}
	if err := rows.Err(); err != nil {
		return features.Propensity{}, "", fmt.Errorf("propensity rows: %w", err)
	}
	eta, clicks, ok := FitPropensity(stats)
	if ok {
		p := features.Propensity{Eta: eta, Clicks: clicks, UpdatedAt: j.Now()}
		if err := j.Store.PutPropensity(ctx, tid, p); err != nil {
			return p, "", err
		}
		return p, "estimated", nil
	}
	prev, found, err := j.Store.GetPropensity(ctx, tid)
	if err != nil {
		return features.Propensity{}, "", err
	}
	if found {
		return prev, "previous", nil
	}
	j.Log.Warn("too little randomized traffic to estimate position propensity; CTR is not debiased",
		"tenant", tid, "randomized_clicks", clicks)
	return features.Propensity{Clicks: clicks}, "none", nil
}

// Counts are one item's (or the tenant's) interactions in the short and long windows.
// Examinations are impressions weighted by the propensity of their position.
type Counts struct {
	Exams7, Exams30   float64
	Clicks7, Clicks30 uint64
	Orders7, Orders30 uint64
}

// ItemFeatures turns an item's counts into features, shrinking CTR (clicks per examination) and
// CVR (orders per click) toward the tenant's means, taken from tenantTotals. A click already
// implies examination, so CVR needs no position correction of its own.
func ItemFeatures(c, tenantTotals Counts) features.Item {
	rate := func(num, den, prior, mean float64) float64 {
		return math.Min(math.Max((num+prior*mean)/(den+prior), 0), 1)
	}
	ctrMean7 := safeDiv(float64(tenantTotals.Clicks7), tenantTotals.Exams7)
	ctrMean30 := safeDiv(float64(tenantTotals.Clicks30), tenantTotals.Exams30)
	cvrMean7 := safeDiv(float64(tenantTotals.Orders7), float64(tenantTotals.Clicks7))
	cvrMean30 := safeDiv(float64(tenantTotals.Orders30), float64(tenantTotals.Clicks30))
	return features.Item{
		CTR7d:        rate(float64(c.Clicks7), c.Exams7, PriorExaminations, ctrMean7),
		CTR30d:       rate(float64(c.Clicks30), c.Exams30, PriorExaminations, ctrMean30),
		CVR7d:        rate(float64(c.Orders7), float64(c.Clicks7), PriorClicks, cvrMean7),
		CVR30d:       rate(float64(c.Orders30), float64(c.Clicks30), PriorClicks, cvrMean30),
		Orders30dLog: math.Min(math.Log1p(float64(c.Orders30))/10, 1),
	}
}

// itemColumns aggregate one group's counts; the propensity exponent and the 7-day cutoff are
// the first two arguments.
const itemColumns = `
	toFloat64(sumIf(pow(greatest(position, 1), -?), type = 'impression' AND ts >= fromUnixTimestamp64Milli(?, 'UTC'))),
	toFloat64(sumIf(pow(greatest(position, 1), -?), type = 'impression')),
	countIf(type = 'click' AND ts >= fromUnixTimestamp64Milli(?, 'UTC')),
	countIf(type = 'click'),
	countIf(type = 'order' AND ts >= fromUnixTimestamp64Milli(?, 'UTC')),
	countIf(type = 'order')`

const itemWhere = `FROM events FINAL
	WHERE tenant_id = ? AND type IN ('impression', 'click', 'order') AND ts >= fromUnixTimestamp64Milli(?, 'UTC')`

// items aggregates every product with events in the window and writes its features.
func (j *Job) items(ctx context.Context, tid tenant.ID, eta float64, now time.Time) (int, error) {
	w7, w30 := now.Add(-ShortWindow).UnixMilli(), now.Add(-Window).UnixMilli()
	args := []any{eta, w7, eta, w7, w7, string(tid), w30}
	totals, err := j.itemTotals(ctx, args)
	if err != nil {
		return 0, err
	}
	rows, err := j.CH.Query(ctx, "SELECT product_id, "+itemColumns+" "+itemWhere+" GROUP BY product_id", args...)
	if err != nil {
		return 0, fmt.Errorf("item features: %w", err)
	}
	defer rows.Close()
	n := 0
	batch := make(map[string]features.Item, docBatch)
	flush := func() error {
		if err := j.Store.PutItems(ctx, tid, batch); err != nil {
			return err
		}
		n += len(batch)
		clear(batch)
		return nil
	}
	for rows.Next() {
		var id string
		var c Counts
		if err := rows.Scan(&id, &c.Exams7, &c.Exams30, &c.Clicks7, &c.Clicks30, &c.Orders7, &c.Orders30); err != nil {
			return n, fmt.Errorf("item scan: %w", err)
		}
		batch[id] = ItemFeatures(c, totals)
		if len(batch) >= docBatch {
			if err := flush(); err != nil {
				return n, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("item rows: %w", err)
	}
	return n, flush()
}

// itemTotals sums the counts over every product: the tenant means the priors shrink toward.
func (j *Job) itemTotals(ctx context.Context, args []any) (Counts, error) {
	var c Counts
	rows, err := j.CH.Query(ctx, "SELECT "+itemColumns+" "+itemWhere, args...)
	if err != nil {
		return c, fmt.Errorf("item totals: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&c.Exams7, &c.Exams30, &c.Clicks7, &c.Clicks30, &c.Orders7, &c.Orders30); err != nil {
			return c, fmt.Errorf("item totals scan: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return c, fmt.Errorf("item totals rows: %w", err)
	}
	return c, nil
}

// Interaction is a user's weighted interest in one product: order 3, add_to_cart 2, click 1,
// summed over the window (the graded labels of docs/MODELS.md §5).
type Interaction struct {
	ProductID string
	Weight    float64
}

// UserFeatures derives a user's category affinities (share of interaction weight per category,
// top TopCategories) and price band (weighted 20th–80th percentile of interacted prices, with at
// least MinPriceInteractions interactions) from products found in docs.
func UserFeatures(ins []Interaction, docs map[string]*pipeline.Doc) (features.User, bool) {
	cats := map[string]float64{}
	type pricePoint struct {
		price  int64
		weight float64
	}
	var prices []pricePoint
	var total float64
	for _, in := range ins {
		d := docs[in.ProductID]
		if d == nil || in.Weight <= 0 {
			continue
		}
		total += in.Weight
		for _, c := range d.CategoryIDs {
			cats[c] += in.Weight
		}
		prices = append(prices, pricePoint{d.PriceMinor, in.Weight})
	}
	if total == 0 {
		return features.User{}, false
	}
	ids := make([]string, 0, len(cats))
	for id := range cats {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool {
		if cats[ids[a]] != cats[ids[b]] {
			return cats[ids[a]] > cats[ids[b]]
		}
		return ids[a] < ids[b]
	})
	u := features.User{CategoryAffinity: make(map[string]float64, min(len(ids), TopCategories))}
	for _, id := range ids[:min(len(ids), TopCategories)] {
		u.CategoryAffinity[id] = math.Round(cats[id]/total*1e4) / 1e4
	}
	if len(prices) >= MinPriceInteractions {
		slices.SortFunc(prices, func(a, b pricePoint) int { return cmp.Compare(a.price, b.price) })
		quantile := func(q float64) int64 {
			acc := 0.0
			for _, p := range prices {
				if acc += p.weight; acc >= q*total {
					return p.price
				}
			}
			return prices[len(prices)-1].price
		}
		u.PriceLo, u.PriceHi = quantile(0.2), quantile(0.8)
	}
	return u, true
}

// users aggregates each user's interactions in the window and writes their features, resolving
// products against the tenant's catalog a chunk of users at a time.
func (j *Job) users(ctx context.Context, tid tenant.ID, from time.Time) (int, error) {
	rows, err := j.CH.Query(ctx, `
		SELECT user_id, product_id, toFloat64(sum(multiIf(type = 'order', 3, type = 'add_to_cart', 2, 1)))
		FROM events FINAL
		WHERE tenant_id = ? AND user_id != '' AND type IN ('click', 'add_to_cart', 'order')
		  AND ts >= fromUnixTimestamp64Milli(?, 'UTC')
		GROUP BY user_id, product_id
		ORDER BY user_id`, string(tid), from.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("user query: %w", err)
	}
	defer rows.Close()
	src := j.Docs(tid)
	docs := map[string]*pipeline.Doc{}
	chunk := map[string][]Interaction{}
	n := 0
	flush := func() error {
		var missing []string
		seen := map[string]bool{}
		for _, ins := range chunk {
			for _, in := range ins {
				if _, ok := docs[in.ProductID]; !ok && !seen[in.ProductID] {
					seen[in.ProductID] = true
					missing = append(missing, in.ProductID)
				}
			}
		}
		for i := 0; i < len(missing); i += docBatch {
			part := missing[i:min(i+docBatch, len(missing))]
			found, err := src.Docs(ctx, part)
			if err != nil {
				return fmt.Errorf("catalog lookup: %w", err)
			}
			for _, id := range part {
				docs[id] = found[id] // nil marks a product no longer in the catalog
			}
		}
		out := make(map[string]features.User, len(chunk))
		for uid, ins := range chunk {
			if u, ok := UserFeatures(ins, docs); ok {
				out[uid] = u
			}
		}
		if err := j.Store.PutUsers(ctx, tid, out); err != nil {
			return err
		}
		n += len(out)
		clear(chunk)
		return nil
	}
	for rows.Next() {
		var uid string
		var in Interaction
		if err := rows.Scan(&uid, &in.ProductID, &in.Weight); err != nil {
			return n, fmt.Errorf("user scan: %w", err)
		}
		if _, ok := chunk[uid]; !ok && len(chunk) >= userChunk {
			if err := flush(); err != nil {
				return n, err
			}
		}
		chunk[uid] = append(chunk[uid], in)
	}
	if err := rows.Err(); err != nil {
		return n, fmt.Errorf("user rows: %w", err)
	}
	return n, flush()
}

func safeDiv(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b
}
