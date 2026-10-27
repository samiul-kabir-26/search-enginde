package ads

import (
	"context"
	"slices"
	"time"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Blender blends sponsored placements into organic results (PLAN.md R10).
type Blender struct {
	store  Store
	pacing Pacing
}

// NewBlender returns an ad blender using store and pacing.
func NewBlender(store Store, pacing Pacing) *Blender {
	return &Blender{store: store, pacing: pacing}
}

// Blend evaluates eligible ads, runs the GSP auction, and places winners into slots,
// deduplicating them against organic candidates. Only candidates scored by S5 (or S6) are
// eligible: relevance is their score, min-max normalized among them, so the unscored tail
// (pre-rank or RRF scores, another scale) neither competes nor skews the normalization.
func (b *Blender) Blend(ctx context.Context, st *pipeline.State, cfg tenant.AdsConfig, now time.Time) {
	if !cfg.Enabled || len(cfg.Slots) == 0 || len(st.Candidates) == 0 || b.store == nil {
		return
	}

	creatives, err := b.store.ListActiveCreatives(ctx, st.Resolved.Tenant.ID)
	if err != nil || len(creatives) == 0 {
		return
	}

	byID := make(map[string]int, len(st.Candidates))
	minScore, maxScore, scored := 0.0, 0.0, 0
	for i, c := range st.Candidates {
		if c.Stage != pipeline.StageRank && c.Stage != pipeline.StageRerank {
			continue
		}
		if scored == 0 || c.Score < minScore {
			minScore = c.Score
		}
		if scored == 0 || c.Score > maxScore {
			maxScore = c.Score
		}
		byID[c.ProductID] = i
		scored++
	}
	if scored == 0 {
		return
	}

	var qCats []string
	for _, cat := range st.Query.Categories {
		qCats = append(qCats, cat.ID)
	}
	qText := st.Query.Raw
	if qText == "" {
		qText = st.Query.Text
	}

	type candidateChoice struct {
		creative  ActiveCreative
		candidate pipeline.Candidate
		normRel   float64
	}
	bestByProduct := make(map[string]candidateChoice)
	// Pacing is decided once per campaign and request: one Redis read, one draw.
	admitted := make(map[string]bool)

	for _, cr := range creatives {
		if now.Before(cr.StartAt) || (cr.EndAt != nil && now.After(*cr.EndAt)) {
			continue
		}
		idx, ok := byID[cr.ProductID]
		if !ok {
			continue
		}
		cand := st.Candidates[idx]

		cats := slices.Clone(qCats)
		if cand.Doc != nil {
			cats = append(cats, cand.Doc.CategoryIDs...)
		}
		if !cr.Targeting.Matches(cats, qText) {
			continue
		}

		normRel := MinMaxNormalize(cand.Score, minScore, maxScore)
		if normRel < cfg.RelevanceFloor {
			continue
		}

		if b.pacing != nil {
			admit, seen := admitted[cr.CampaignID]
			if !seen {
				var err error
				admit, err = b.pacing.Admit(ctx, st.Resolved.Tenant.ID, cr.CampaignID, cr.DailyBudgetMinor, now)
				admit = admit && err == nil
				admitted[cr.CampaignID] = admit
			}
			if !admit {
				continue
			}
		}

		prev, exists := bestByProduct[cr.ProductID]
		if !exists || cr.BidMinor > prev.creative.BidMinor {
			bestByProduct[cr.ProductID] = candidateChoice{creative: cr, candidate: cand, normRel: normRel}
		}
	}
	if len(bestByProduct) == 0 {
		return
	}

	auctionCandidates := make([]Candidate, 0, len(bestByProduct))
	for pid, choice := range bestByProduct {
		rawCTR := choice.candidate.Features[feature.CTR30d]
		pctr := SmoothedPCTR(rawCTR, 0, DefaultMeanCTR, DefaultPriorStrength)
		auctionCandidates = append(auctionCandidates, Candidate{
			CampaignID: choice.creative.CampaignID,
			VendorID:   choice.creative.VendorID,
			ProductID:  pid,
			BidMinor:   choice.creative.BidMinor,
			Relevance:  choice.normRel,
			PCTR:       pctr,
			Quality:    ComputeQuality(choice.normRel, pctr, DefaultRelevanceWeight, DefaultPCTRWeight),
		})
	}
	// Map iteration order is random; ties in the auction must not depend on it.
	slices.SortFunc(auctionCandidates, func(a, b Candidate) int {
		if a.ProductID < b.ProductID {
			return -1
		}
		if a.ProductID > b.ProductID {
			return 1
		}
		return 0
	})

	winners := RunGSPAuction(auctionCandidates, cfg.ReserveCPCMinor)
	slots := slices.Clone(cfg.Slots)
	slices.Sort(slots)
	winners = winners[:min(len(winners), len(slots))]

	// Remove every winner's organic copy first, then insert in ascending slot order: removing
	// after an insertion would shift the ads already placed above it out of their slots.
	won := make(map[string]bool, len(winners))
	for _, w := range winners {
		won[w.ProductID] = true
	}
	organic := make([]pipeline.Candidate, 0, len(st.Candidates))
	for _, c := range st.Candidates {
		if !won[c.ProductID] {
			organic = append(organic, c)
		}
	}
	out := make([]pipeline.Candidate, 0, len(st.Candidates))
	next := 0
	for i, w := range winners {
		slotIdx := slots[i] - 1
		for len(out) < slotIdx && next < len(organic) {
			out = append(out, organic[next])
			next++
		}
		ad := bestByProduct[w.ProductID].candidate
		ad.Sponsored = true
		out = append(out, ad)

		// Cache the clearing price for click settlement.
		if b.pacing != nil && st.Query.RequestID != "" {
			_ = b.pacing.RecordPrice(ctx, st.Resolved.Tenant.ID, st.Query.RequestID, w.ProductID, w.CampaignID, w.ClearedCPC)
		}
	}
	out = append(out, organic[next:]...)
	st.Candidates = out
}
