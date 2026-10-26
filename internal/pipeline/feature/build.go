package feature

import (
	"math"
	"slices"
	"time"
)

// ResponseHoursCap normalizes vendor_response_hours: min(hours / cap, 1).
const ResponseHoursCap = 48.0

// Defaults are the declared values for features a candidate does not have (docs/MODELS.md §4):
// a product without recent interactions, a vendor the tenant never sent, an anonymous user.
func Defaults() map[string]float64 {
	return map[string]float64{
		RRFScore: 0, BM25Norm: 0, CosineText: 0, CosineImage: 0, RetrieverCount: 0, PolicyBoost: 0,
		PriceMinorLog: 0, Rating: 0.6, RatingCountLog: 0, ListingCompleteness: 0.5, ImageCount: 0, AgeDays: 0.5,
		CTR7d: 0, CTR30d: 0, CVR7d: 0, CVR30d: 0, Orders30dLog: 0,
		VendorScore: 0.5, VendorOnTimeDispatch: 0.9, VendorDisputeRate: 0.02, VendorResponseHours: 0.2, VendorVerified: 0,
		QueryCategoryMatch: 0, UserCategoryAffinity: 0, UserPriceBandMatch: 0,
	}
}

// Input is what the builder needs about one candidate. Retriever scores are keyed by
// retriever name and are raw (BM25 or cosine).
type Input struct {
	RRF float64
	// PolicyBoost is the candidate's clamped CEL boost sum (S3).
	PolicyBoost         float64
	Retrievers          map[string]float64
	PriceMinor          int64
	Rating              *float64
	RatingCount         int64
	ListingCompleteness float64
	ImageCount          int
	CreatedAt           *time.Time
	CategoryIDs         []string
}

// QueryInput is what the builder needs about the query.
type QueryInput struct {
	Categories     map[string]float64
	RetrieverTotal int
}

// Build computes the request-local features of every candidate, normalized to [0, 1] so linear
// weights are comparable: rrf_score and bm25_norm are divided by the request maximum, counts
// and prices are log-scaled. Cosines are used as returned (already within [-1, 1]).
func Build(q QueryInput, in []Input, now time.Time) []map[string]float64 {
	var maxRRF, maxBM25 float64
	for _, c := range in {
		maxRRF = max(maxRRF, c.RRF)
		maxBM25 = max(maxBM25, c.Retrievers["lexical_bm25"])
	}
	out := make([]map[string]float64, len(in))
	for i, c := range in {
		f := map[string]float64{
			RRFScore:            safeDiv(c.RRF, maxRRF),
			BM25Norm:            safeDiv(c.Retrievers["lexical_bm25"], maxBM25),
			CosineText:          c.Retrievers["vector_text"],
			CosineImage:         max(c.Retrievers["vector_text_to_image"], c.Retrievers["vector_image"]),
			RetrieverCount:      safeDiv(float64(len(c.Retrievers)), float64(q.RetrieverTotal)),
			PriceMinorLog:       math.Min(math.Log1p(float64(c.PriceMinor)/100)/10, 1),
			RatingCountLog:      math.Min(math.Log1p(float64(c.RatingCount))/10, 1),
			ListingCompleteness: c.ListingCompleteness,
			ImageCount:          math.Min(float64(c.ImageCount), 10) / 10,
			PolicyBoost:         c.PolicyBoost,
		}
		if c.Rating != nil {
			f[Rating] = *c.Rating / 5
		}
		if c.CreatedAt != nil {
			f[AgeDays] = math.Min(now.Sub(*c.CreatedAt).Hours()/24/730, 1)
		}
		best := 0.0
		for id, conf := range q.Categories {
			if slices.Contains(c.CategoryIDs, id) {
				best = max(best, conf)
			}
		}
		f[QueryCategoryMatch] = best
		out[i] = f
	}
	return out
}

func safeDiv(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b
}
