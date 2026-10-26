// Package online reads a request's online features (PLAN.md R9) once, for S3 (CEL rules read
// item.vendor) and S4 (scorer features).
package online

import (
	"context"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/resilience"
)

// Provider names the online feature read in breaker keys and trace reasons.
const Provider = "online_features"

// Read loads the item, vendor and user features of st's candidates into st.Online unless an
// earlier stage already tried. The read gets at most half of the stage's remaining budget, so
// the stage's own work always has time left; on failure st.Online is empty (declared defaults
// apply) and the fallback reason is returned, else "".
func Read(ctx context.Context, b *resilience.Breakers, r pipeline.FeatureReader, stage string, st *pipeline.State) string {
	if st.Online != nil {
		return ""
	}
	st.Online = &pipeline.OnlineFeatures{}
	if r == nil || len(st.Candidates) == 0 {
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
	key := resilience.Key{Tenant: string(tid), Stage: stage, Provider: Provider}
	got, out, err := resilience.Call(ctx, b, key, func(ctx context.Context) (*pipeline.OnlineFeatures, error) {
		return r.Read(ctx, tid, req)
	}, nil)
	if err != nil {
		if out.Reason == "" {
			out.Reason = resilience.ReasonError
		}
		return Provider + ":" + out.Reason
	}
	st.Online = got
	return ""
}
