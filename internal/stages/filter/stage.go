package filter

import (
	"context"
	"fmt"
	"maps"
	"math"
	"runtime"
	"strings"
	"sync"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/stages/online"
)

// chunk is how many candidates a worker evaluates between deadline checks.
const chunk = 32

// Stage is S3. Structured filters (request filters and always_filters) are already pushed down
// into S2; the tenant's CEL filter and boost rules run here on the candidates.
type Stage struct {
	breakers *resilience.Breakers
	features pipeline.FeatureReader
}

// New returns the S3 stage. features supplies item.vendor (nil leaves vendor defaults).
func New(b *resilience.Breakers, features pipeline.FeatureReader) *Stage {
	return &Stage{breakers: b, features: features}
}

// Name implements pipeline.Stage.
func (*Stage) Name() string { return pipeline.StageFilter }

// Run evaluates the rules on every candidate, in parallel and within the stage deadline: filter
// rules drop candidates, boost rules set PolicyBoost. Candidates not evaluated before the
// deadline, and rules that fail on a candidate, leave it unchanged; both are reported.
func (s *Stage) Run(ctx context.Context, st *pipeline.State) (pipeline.Report, error) {
	pol := st.Providers.Policy
	if pol == nil || pol.Len() == 0 || len(st.Candidates) == 0 {
		return pipeline.Report{Provider: "pushdown"}, nil
	}
	rep := pipeline.Report{Provider: "cel"}
	var reasons []string
	if r := online.Read(ctx, s.breakers, s.features, s.Name(), st); r != "" {
		reasons = append(reasons, r)
	}
	query := QueryVars(st.Query)
	cs := st.Candidates
	keep := make([]bool, len(cs))
	for i := range keep {
		keep[i] = true
	}
	var mu sync.Mutex
	errs, skipped := 0, 0
	workers := min(runtime.GOMAXPROCS(0), (len(cs)+chunk-1)/chunk)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			e, sk := 0, 0
			for start := w * chunk; start < len(cs); start += workers * chunk {
				end := min(start+chunk, len(cs))
				if ctx.Err() != nil {
					sk += end - start
					continue
				}
				for i := start; i < end; i++ {
					k, boost, n := pol.Eval(ItemVars(cs[i].Doc, cs[i].ProductID, st.Online), query)
					keep[i], cs[i].PolicyBoost, e = k, boost, e+n
				}
			}
			mu.Lock()
			errs, skipped = errs+e, skipped+sk
			mu.Unlock()
		})
	}
	wg.Wait()
	out := cs[:0]
	for i, c := range cs {
		if keep[i] {
			out = append(out, c)
		}
	}
	st.Candidates = out
	if skipped > 0 {
		reasons = append(reasons, fmt.Sprintf("cel:timeout(%d unevaluated)", skipped))
	}
	if errs > 0 {
		reasons = append(reasons, fmt.Sprintf("cel:errors(%d)", errs))
	}
	if len(reasons) > 0 {
		rep.Fallback, rep.FallbackReason = true, strings.Join(reasons, ",")
	}
	return rep, nil
}

// QueryVars is the CEL `query` variable: the normalized text, query type, predicted category
// IDs and recognized attributes.
func QueryVars(q *pipeline.Query) map[string]any {
	cats := make([]string, len(q.Categories))
	for i, c := range q.Categories {
		cats[i] = c.ID
	}
	attrs := make(map[string]any, len(q.Attributes))
	for k, v := range q.Attributes {
		attrs[k] = v
	}
	return map[string]any{"text": q.Text, "type": string(q.Type), "categories": cats, "attributes": attrs}
}

// ItemVars is the CEL `item` variable of one candidate: its ranking fields, `vendor` (the
// tenant's vendor values, with the declared defaults for fields never sent) and `custom`.
// Optional fields the product lacks (rating, stock, created_at) are absent; rules test them with
// has(item.rating).
func ItemVars(d *pipeline.Doc, productID string, on *pipeline.OnlineFeatures) map[string]any {
	item := map[string]any{"product_id": productID, "custom": map[string]any{}}
	if d == nil {
		item["vendor"] = vendorVars(nil)
		return item
	}
	item["vendor_id"], item["brand"], item["price_minor"] = d.VendorID, d.Brand, d.PriceMinor
	item["category_ids"], item["rating_count"] = nonNil(d.CategoryIDs), d.RatingCount
	item["listing_completeness"], item["image_count"] = d.ListingCompleteness, int64(d.ImageCount)
	item["status"], item["ship_regions"] = d.Status, nonNil(d.ShipRegions)
	if d.Rating != nil {
		item["rating"] = *d.Rating
	}
	if d.Stock != nil {
		item["stock"] = *d.Stock
	}
	if d.CreatedAt != nil {
		item["created_at"] = *d.CreatedAt
	}
	if d.Custom != nil {
		item["custom"] = d.Custom
	}
	var attrs map[string]any
	if on != nil {
		attrs = on.VendorAttrs[d.VendorID]
	}
	item["vendor"] = vendorVars(attrs)
	return item
}

// defaultVendor is a vendor the tenant never sent, in CEL form: the declared feature defaults,
// so a rule sees the same vendor the scorers do. It is never modified.
var defaultVendor = func() map[string]any {
	d := feature.Defaults()
	return map[string]any{
		catalog.VendorScore:         d[feature.VendorScore],
		catalog.VendorOnTime:        d[feature.VendorOnTimeDispatch],
		catalog.VendorDisputeRate:   d[feature.VendorDisputeRate],
		catalog.VendorResponseHours: math.Round(d[feature.VendorResponseHours]*feature.ResponseHoursCap*1e6) / 1e6,
		catalog.VendorVerified:      d[feature.VendorVerified] > 0,
	}
}()

// vendorVars overlays the tenant's stored values on defaultVendor.
func vendorVars(attrs map[string]any) map[string]any {
	if len(attrs) == len(defaultVendor) {
		return attrs
	}
	v := maps.Clone(defaultVendor)
	maps.Copy(v, attrs)
	return v
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}
