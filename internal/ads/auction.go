package ads

import (
	"math"
	"sort"
)

// Default quality weights and prior (PLAN.md R10).
const (
	DefaultRelevanceWeight = 0.6
	DefaultPCTRWeight      = 0.4
	DefaultPriorStrength   = 100.0
	DefaultMeanCTR         = 0.02
)

// QualityConfig allows tuning weights for quality score.
type QualityConfig struct {
	RelevanceWeight float64 `json:"relevance_weight"`
	PCTRWeight      float64 `json:"pctr_weight"`
}

// ComputeQuality returns quality = relWeight * relevance + pctrWeight * pCTR.
func ComputeQuality(relevance, pCTR, relWeight, pctrWeight float64) float64 {
	if relWeight <= 0 && pctrWeight <= 0 {
		relWeight, pctrWeight = DefaultRelevanceWeight, DefaultPCTRWeight
	}
	return (relWeight * relevance) + (pctrWeight * pCTR)
}

// SmoothedPCTR computes Bayesian-smoothed CTR toward meanCTR with prior impressions.
func SmoothedPCTR(rawCTR float64, impressions float64, meanCTR, prior float64) float64 {
	if meanCTR <= 0 {
		meanCTR = DefaultMeanCTR
	}
	if prior <= 0 {
		prior = DefaultPriorStrength
	}
	if impressions <= 0 {
		// When candidate has only a point CTR estimate without impression count,
		// weight it by prior.
		return (rawCTR*prior + meanCTR*prior) / (prior * 2)
	}
	clicks := rawCTR * impressions
	return (clicks + prior*meanCTR) / (impressions + prior)
}

// Candidate is an eligible ad candidate evaluated for auction.
type Candidate struct {
	CampaignID string
	VendorID   string
	ProductID  string
	BidMinor   int64
	Relevance  float64
	PCTR       float64
	Quality    float64
	RankScore  float64
	ClearedCPC int64
}

// MinMaxNormalize normalizes values to [0, 1].
func MinMaxNormalize(val, minVal, maxVal float64) float64 {
	if maxVal <= minVal {
		return 1.0
	}
	norm := (val - minVal) / (maxVal - minVal)
	if norm < 0 {
		return 0
	}
	if norm > 1 {
		return 1
	}
	return norm
}

// RunGSPAuction runs a Generalized Second Price auction on candidates (PLAN.md R10).
// Each candidate's clearing price is:
// price_i = min(bid_i, max(reserveCPC, ceil(bid_{i+1} * quality_{i+1} / quality_i) + 1 minor unit)).
// The last placed ad pays reserveCPC (capped at its bid).
func RunGSPAuction(candidates []Candidate, reserveCPC int64) []Candidate {
	if len(candidates) == 0 {
		return nil
	}
	if reserveCPC <= 0 {
		reserveCPC = 1
	}

	// Compute rank score = bid * quality
	for i := range candidates {
		candidates[i].RankScore = float64(candidates[i].BidMinor) * candidates[i].Quality
	}

	// Sort descending by rank score
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].RankScore > candidates[j].RankScore
	})

	for i := range candidates {
		bid := candidates[i].BidMinor
		q := candidates[i].Quality
		if q <= 0 {
			candidates[i].ClearedCPC = min(bid, reserveCPC)
			continue
		}

		var price int64
		if i+1 < len(candidates) {
			nextBid := candidates[i+1].BidMinor
			nextQ := candidates[i+1].Quality
			// price = ceil(nextBid * nextQ / q) + 1 minor unit
			nextScore := float64(nextBid) * nextQ
			price = int64(math.Ceil(nextScore/q)) + 1
		} else {
			price = reserveCPC
		}

		if price < reserveCPC {
			price = reserveCPC
		}
		if price > bid {
			price = bid
		}
		candidates[i].ClearedCPC = price
	}

	return candidates
}
