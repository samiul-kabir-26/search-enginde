package ads

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/zavisoft/search-engine/internal/tenant"
)

// ReportRow is one campaign's aggregate performance row (api/openapi/control.yaml AdsReport).
type ReportRow struct {
	CampaignID  string  `json:"campaign_id"`
	Impressions int64   `json:"impressions"`
	Clicks      int64   `json:"clicks"`
	SpendMinor  int64   `json:"spend_minor"`
	CTR         float64 `json:"ctr"`
}

// Reports reads ad impressions, clicks and spend from ClickHouse and groups by campaign.
type Reports struct {
	ch driver.Conn
}

// NewReports returns a Reports service.
func NewReports(ch driver.Conn) *Reports {
	return &Reports{ch: ch}
}

// Generate aggregates sponsored performance between the from and to dates (inclusive) for the
// tenant. Events carry no campaign, so a product's sponsored events are attributed through
// productToCampaign (its creative); products that no campaign promotes are left out.
func (r *Reports) Generate(ctx context.Context, tid tenant.ID, from, to time.Time, productToCampaign map[string]string) ([]ReportRow, error) {
	if r.ch == nil {
		return []ReportRow{}, nil
	}
	const q = `
SELECT product_id,
       countIf(type = 'impression' AND sponsored) AS impressions,
       countIf(type = 'click' AND sponsored) AS clicks,
       sumIf(value_minor, type = 'click' AND sponsored) AS spend
FROM events FINAL
WHERE tenant_id = ? AND sponsored AND toDate(ts) >= ? AND toDate(ts) <= ?
GROUP BY product_id
`
	rows, err := r.ch.Query(ctx, q, string(tid), from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("query ads report: %w", err)
	}
	defer rows.Close()

	byCampaign := make(map[string]*ReportRow)
	for rows.Next() {
		var (
			productID   string
			impressions uint64
			clicks      uint64
			spend       int64
		)
		if err := rows.Scan(&productID, &impressions, &clicks, &spend); err != nil {
			return nil, fmt.Errorf("scan ads report row: %w", err)
		}

		campID, ok := productToCampaign[productID]
		if !ok {
			continue
		}
		row, ok := byCampaign[campID]
		if !ok {
			row = &ReportRow{CampaignID: campID}
			byCampaign[campID] = row
		}
		row.Impressions += int64(impressions)
		row.Clicks += int64(clicks)
		row.SpendMinor += spend
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ads report rows: %w", err)
	}

	out := make([]ReportRow, 0, len(byCampaign))
	for _, row := range byCampaign {
		if row.Impressions > 0 {
			row.CTR = float64(row.Clicks) / float64(row.Impressions)
		}
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b ReportRow) int { return strings.Compare(a.CampaignID, b.CampaignID) })
	return out, nil
}
