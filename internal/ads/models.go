package ads

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/zavisoft/search-engine/internal/tenant"
)

// Status values of campaigns.
const (
	StatusActive    = "active"
	StatusPaused    = "paused"
	StatusDraft     = "draft"
	StatusCompleted = "completed"
)

// Campaign is an ad campaign (api/openapi/control.yaml Campaign).
type Campaign struct {
	ID               string     `json:"id"`
	TenantID         tenant.ID  `json:"tenant_id,omitempty"`
	VendorID         string     `json:"vendor_id"`
	Status           string     `json:"status"`
	DailyBudgetMinor int64      `json:"daily_budget_minor"`
	Currency         string     `json:"currency"`
	BidMinor         int64      `json:"bid_minor"`
	Targeting        Targeting  `json:"targeting"`
	StartAt          time.Time  `json:"start_at"`
	EndAt            *time.Time `json:"end_at,omitempty"`
}

// Targeting is targeting criteria for a campaign.
type Targeting struct {
	Categories []string `json:"categories,omitempty"`
	Keywords   []string `json:"keywords,omitempty"`
	Queries    []string `json:"queries,omitempty"`
}

// Matches checks if targeting matches candidate category IDs or query text.
// An empty targeting matches all queries and categories.
func (t Targeting) Matches(categoryIDs []string, queryText string) bool {
	if len(t.Categories) > 0 {
		catMatch := false
		for _, reqCat := range t.Categories {
			reqNorm := strings.ToLower(strings.TrimSpace(reqCat))
			for _, c := range categoryIDs {
				if strings.EqualFold(c, reqNorm) {
					catMatch = true
					break
				}
			}
			if catMatch {
				break
			}
		}
		if !catMatch {
			return false
		}
	}

	if len(t.Keywords) > 0 || len(t.Queries) > 0 {
		qNorm := strings.ToLower(strings.TrimSpace(queryText))
		textMatch := false
		for _, kw := range t.Keywords {
			kwNorm := strings.ToLower(strings.TrimSpace(kw))
			if kwNorm != "" && strings.Contains(qNorm, kwNorm) {
				textMatch = true
				break
			}
		}
		if !textMatch {
			for _, q := range t.Queries {
				qCheck := strings.ToLower(strings.TrimSpace(q))
				if qCheck != "" && (strings.Contains(qNorm, qCheck) || strings.Contains(qCheck, qNorm)) {
					textMatch = true
					break
				}
			}
		}
		if !textMatch {
			return false
		}
	}

	return true
}

// Creative is an ad product creative within a campaign.
type Creative struct {
	ID         string `json:"id"`
	CampaignID string `json:"campaign_id"`
	ProductID  string `json:"product_id"`
}

// ActiveCreative is an active campaign joined with one of its creatives.
type ActiveCreative struct {
	CampaignID       string
	TenantID         tenant.ID
	VendorID         string
	DailyBudgetMinor int64
	Currency         string
	BidMinor         int64
	Targeting        Targeting
	StartAt          time.Time
	EndAt            *time.Time
	CreativeID       string
	ProductID        string
}

// TargetingFromRaw parses raw JSON targeting.
func TargetingFromRaw(raw json.RawMessage) Targeting {
	var t Targeting
	if len(raw) > 0 && string(raw) != "null" {
		_ = json.Unmarshal(raw, &t)
	}
	return t
}
