package ads

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/tenant"
)

// Pacing determines whether a campaign is throttled based on budget and current spend.
type Pacing interface {
	Admit(ctx context.Context, tid tenant.ID, campaignID string, dailyBudgetMinor int64, now time.Time) (bool, error)
	RecordSpend(ctx context.Context, tid tenant.ID, campaignID string, spendMinor int64, now time.Time) error
	RecordPrice(ctx context.Context, tid tenant.ID, requestID, productID, campaignID string, priceMinor int64) error
	GetPrice(ctx context.Context, tid tenant.ID, requestID, productID string) (campaignID string, priceMinor int64, ok bool, err error)
	// Charge settles a sponsored click on (requestID, productID): it returns the clearing
	// price recorded when the ad was served and adds it to the campaign's spend only the first
	// time, so a resent click batch is never charged twice.
	Charge(ctx context.Context, tid tenant.ID, requestID, productID string, now time.Time) (campaignID string, priceMinor int64, ok bool, err error)
}

// RedisPacing implements Pacing against Redis.
type RedisPacing struct {
	rdb redis.Cmdable
}

// NewRedisPacing returns a Pacing store backed by Redis.
func NewRedisPacing(rdb redis.Cmdable) *RedisPacing {
	return &RedisPacing{
		rdb: rdb,
	}
}

func randomFloat64() float64 {
	var b [8]byte
	_, _ = crand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}

// spendKey returns the key tracking spend for a campaign on a given UTC date.
func spendKey(tid tenant.ID, campaignID string, t time.Time) string {
	date := t.UTC().Format("20060102")
	return fmt.Sprintf("f:{%s}:ad:spend:%s:%s", string(tid), campaignID, date)
}

// priceKey returns the key storing an ad's clearing price for CPC click billing.
func priceKey(tid tenant.ID, requestID, productID string) string {
	return fmt.Sprintf("f:{%s}:ad:price:%s:%s", string(tid), requestID, productID)
}

// chargedKey marks a placement whose click has been charged.
func chargedKey(tid tenant.ID, requestID, productID string) string {
	return fmt.Sprintf("f:{%s}:ad:charged:%s:%s", string(tid), requestID, productID)
}

// priceTTL is how long a served ad's clearing price, and its charged marker, are kept.
const priceTTL = time.Hour

// ClearingPriceRecord is stored in Redis for click settlement.
type ClearingPriceRecord struct {
	CampaignID string `json:"campaign_id"`
	PriceMinor int64  `json:"price_minor"`
}

// Admit checks probabilistic throttling against the hourly spend curve (PLAN.md R10).
func (p *RedisPacing) Admit(ctx context.Context, tid tenant.ID, campaignID string, dailyBudgetMinor int64, now time.Time) (bool, error) {
	if dailyBudgetMinor <= 0 {
		return false, nil
	}
	key := spendKey(tid, campaignID, now)
	val, err := p.rdb.Get(ctx, key).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return true, fmt.Errorf("read spend: %w", err)
	}

	var currentSpend int64
	if val != "" {
		currentSpend, _ = strconv.ParseInt(val, 10, 64)
	}

	if currentSpend >= dailyBudgetMinor {
		return false, nil
	}

	// Hourly spend curve: uniform progression through 24 hours.
	hour := now.UTC().Hour()
	targetSpend := (dailyBudgetMinor * int64(hour+1)) / 24

	// If spend is below the target spend for this hour, admit unconditionally.
	if currentSpend <= targetSpend {
		return true, nil
	}

	// Spend is ahead of curve: throttle probabilistically.
	hourlySlice := dailyBudgetMinor / 24
	if hourlySlice <= 0 {
		hourlySlice = 1
	}
	excess := currentSpend - targetSpend
	prob := 1.0 - (float64(excess) / float64(hourlySlice))
	if prob <= 0 {
		return false, nil
	}

	return randomFloat64() < prob, nil
}

// RecordSpend records spend charged to a campaign.
func (p *RedisPacing) RecordSpend(ctx context.Context, tid tenant.ID, campaignID string, spendMinor int64, now time.Time) error {
	if spendMinor <= 0 {
		return nil
	}
	key := spendKey(tid, campaignID, now)
	pipe := p.rdb.Pipeline()
	pipe.IncrBy(ctx, key, spendMinor)
	pipe.Expire(ctx, key, 48*time.Hour)
	_, err := pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("record spend: %w", err)
	}
	return nil
}

// RecordPrice caches the ad clearing price for 1 hour to charge clicks.
func (p *RedisPacing) RecordPrice(ctx context.Context, tid tenant.ID, requestID, productID, campaignID string, priceMinor int64) error {
	key := priceKey(tid, requestID, productID)
	b, err := json.Marshal(ClearingPriceRecord{
		CampaignID: campaignID,
		PriceMinor: priceMinor,
	})
	if err != nil {
		return fmt.Errorf("marshal price: %w", err)
	}
	if err := p.rdb.Set(ctx, key, b, priceTTL).Err(); err != nil {
		return fmt.Errorf("redis set: %w", err)
	}
	return nil
}

// GetPrice retrieves the clearing price cached for request and product.
func (p *RedisPacing) GetPrice(ctx context.Context, tid tenant.ID, requestID, productID string) (string, int64, bool, error) {
	key := priceKey(tid, requestID, productID)
	val, err := p.rdb.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("redis get: %w", err)
	}
	var rec ClearingPriceRecord
	if err := json.Unmarshal([]byte(val), &rec); err != nil {
		return "", 0, false, fmt.Errorf("unmarshal price: %w", err)
	}
	return rec.CampaignID, rec.PriceMinor, true, nil
}

// Charge implements Pacing.
func (p *RedisPacing) Charge(ctx context.Context, tid tenant.ID, requestID, productID string, now time.Time) (string, int64, bool, error) {
	campaignID, price, ok, err := p.GetPrice(ctx, tid, requestID, productID)
	if err != nil || !ok {
		return "", 0, false, err
	}
	first, err := p.rdb.SetNX(ctx, chargedKey(tid, requestID, productID), 1, priceTTL).Result()
	if err != nil {
		return "", 0, false, fmt.Errorf("mark charged: %w", err)
	}
	if first {
		if err := p.RecordSpend(ctx, tid, campaignID, price, now); err != nil {
			return "", 0, false, err
		}
	}
	return campaignID, price, true, nil
}
