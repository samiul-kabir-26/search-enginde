package prerank

import (
	"context"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/resilience"
	"github.com/zavisoft/search-engine/internal/stages/retrieval"
	"github.com/zavisoft/search-engine/internal/stages/scoring"
)

// ProviderOnlineFeatures names the online feature read in breaker keys and trace reasons.
const ProviderOnlineFeatures = "online_features"

// Stage is S4.
type Stage struct {
	breakers *resilience.Breakers
	features pipeline.FeatureReader
	now      func() time.Time
}

// New returns the S4 stage. A nil features reader leaves every online feature at its declared
// default.
func New(b *resilience.Breakers, features pipeline.FeatureReader) *Stage {
	return &Stage{breakers: b, features: features, now: time.Now}
}

// Name implements pipeline.Stage.
func (*Stage) Name() string { return pipeline.StagePrerank }

// Run computes every candidate's request-local and online features, then scores and trims to
// prerank.keep. A failed feature read is reported as a fallback; scoring still runs on defaults.
func (s *Stage) Run(ctx context.Context, st *pipeline.State) (pipeline.Report, error) {
	BuildFeatures(st, s.now())
	featReason := s.readOnline(ctx, st)
	cfg := st.Resolved.Config
	rep := scoring.Apply(ctx, s.breakers, st, s.Name(), pipeline.StageRetrieval, st.Providers.Prerank,
		st.Providers.Names["prerank"], cfg.Prerank.Keep)
	if featReason != "" {
		rep.Fallback = true
		if rep.FallbackReason != "" {
			featReason += "," + rep.FallbackReason
		}
		rep.FallbackReason = featReason
	}
	return rep, nil
}

// readOnline adds stored item, vendor and user features to the candidates (PLAN.md R9). The
// read gets at most half of the stage's remaining budget so scoring always has time left; on
// failure the candidates keep the declared defaults and the fallback reason is returned.
func (s *Stage) readOnline(ctx context.Context, st *pipeline.State) string {
	if s.features == nil || len(st.Candidates) == 0 {
		return ""
	}
	req := pipeline.FeatureRequest{ProductIDs: make([]string, 0, len(st.Candidates)), UserID: st.Query.User.ID}
	seen := map[string]bool{}
	for _, c := range st.Candidates {
		req.ProductIDs = append(req.ProductIDs, c.ProductID)
		if c.Doc != nil && c.Doc.VendorID != "" && !seen[c.Doc.VendorID] {
			seen[c.Doc.VendorID] = true
			req.VendorIDs = append(req.VendorIDs, c.Doc.VendorID)
		}
	}
	if dl, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Until(dl)/2)
		defer cancel()
	}
	tid := st.Resolved.Tenant.ID
	key := resilience.Key{Tenant: string(tid), Stage: s.Name(), Provider: ProviderOnlineFeatures}
	online, out, err := resilience.Call(ctx, s.breakers, key, func(ctx context.Context) (*pipeline.OnlineFeatures, error) {
		return s.features.Read(ctx, tid, req)
	}, nil)
	if err != nil {
		if out.Reason == "" {
			out.Reason = resilience.ReasonError
		}
		return ProviderOnlineFeatures + ":" + out.Reason
	}
	ApplyOnline(st.Candidates, online)
	return ""
}

// ApplyOnline merges online features into each candidate's features. User features are set only
// when the request's user has stored preferences.
func ApplyOnline(cs []pipeline.Candidate, f *pipeline.OnlineFeatures) {
	for i := range cs {
		c := &cs[i]
		if c.Features == nil {
			c.Features = map[string]float64{}
		}
		for k, v := range f.Items[c.ProductID] {
			c.Features[k] = v
		}
		if c.Doc == nil {
			continue
		}
		for k, v := range f.Vendors[c.Doc.VendorID] {
			c.Features[k] = v
		}
		if u := f.User; u != nil {
			best := 0.0
			for _, id := range c.Doc.CategoryIDs {
				best = max(best, u.CategoryAffinity[id])
			}
			c.Features[feature.UserCategoryAffinity] = best
			match := 0.0
			if u.PriceHi > 0 && c.Doc.PriceMinor >= u.PriceLo && c.Doc.PriceMinor <= u.PriceHi {
				match = 1
			}
			c.Features[feature.UserPriceBandMatch] = match
		}
	}
}

// BuildFeatures fills Candidate.Features from retrieval scores, the stored doc and the query.
func BuildFeatures(st *pipeline.State, now time.Time) {
	q := st.Query
	cats := make(map[string]float64, len(q.Categories))
	for _, c := range q.Categories {
		cats[c.ID] = c.Confidence
	}
	total := len(retrieval.Retrievers(st.Resolved.Config, q))
	in := make([]feature.Input, len(st.Candidates))
	for i, c := range st.Candidates {
		scores := make(map[string]float64, len(c.Retrievers))
		for name, h := range c.Retrievers {
			scores[name] = h.Score
		}
		fi := feature.Input{RRF: c.RRF, Retrievers: scores}
		if d := c.Doc; d != nil {
			fi.PriceMinor, fi.Rating, fi.RatingCount = d.PriceMinor, d.Rating, d.RatingCount
			fi.ListingCompleteness, fi.ImageCount, fi.CreatedAt, fi.CategoryIDs = d.ListingCompleteness, d.ImageCount, d.CreatedAt, d.CategoryIDs
		}
		in[i] = fi
	}
	for i, f := range feature.Build(feature.QueryInput{Categories: cats, RetrieverTotal: total}, in, now) {
		st.Candidates[i].Features = f
	}
}
