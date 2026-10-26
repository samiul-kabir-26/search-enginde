package features

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// TTLs per PLAN.md R9. Vendor features never expire: the tenant is their source of truth.
const (
	ItemTTL = 48 * time.Hour
	UserTTL = 30 * 24 * time.Hour
)

// MaxVendorsPerRequest bounds the vendor lookups of one request (PLAN.md R9); candidates past
// it keep the vendor defaults.
const MaxVendorsPerRequest = 300

// ResponseHoursCap normalizes vendor_response_hours: min(hours / cap, 1).
const ResponseHoursCap = 48.0

// writeBatch is how many keys one pipelined write sends.
const writeBatch = 500

// Item hash fields are the catalog feature names.
var itemFields = []string{feature.CTR7d, feature.CTR30d, feature.CVR7d, feature.CVR30d, feature.Orders30dLog}

// Hash fields of user and propensity hashes, plus updated_at on every hash (PLAN.md R9). Vendor
// hashes use the catalog.Vendor field names.
const (
	userAffinity     = "category_affinity"
	userPriceLo      = "price_lo"
	userPriceHi      = "price_hi"
	fieldUpdatedAt   = "updated_at"
	propensityEta    = "eta"
	propensityClicks = "clicks"
)

var vendorFields = catalog.VendorFields

// ItemKey is the item feature hash of a product. The {tid} hash tag keeps a tenant's keys in one
// Redis Cluster slot, so one pipeline serves a request.
func ItemKey(tid tenant.ID, productID string) string { return "f:{" + string(tid) + "}:i:" + productID }

// VendorKey is the vendor feature hash.
func VendorKey(tid tenant.ID, vendorID string) string { return "f:{" + string(tid) + "}:v:" + vendorID }

// UserKey is the user feature hash.
func UserKey(tid tenant.ID, userID string) string { return "f:{" + string(tid) + "}:u:" + userID }

// PropensityKey holds the tenant's estimated examination propensity curve.
func PropensityKey(tid tenant.ID) string { return "f:{" + string(tid) + "}:propensity" }

// Item is a product's aggregated interaction features, already normalized to [0, 1].
type Item struct {
	CTR7d, CTR30d, CVR7d, CVR30d, Orders30dLog float64
}

// User is a user's preferences derived from recent interactions.
type User struct {
	CategoryAffinity map[string]float64
	PriceLo, PriceHi int64
}

// Propensity is the examination curve p(pos) = pos^-Eta estimated from randomized traffic.
type Propensity struct {
	Eta float64
	// Clicks is how many randomized-slot clicks the estimate used.
	Clicks    int64
	UpdatedAt time.Time
}

// Store reads and writes online features in Redis (PLAN.md R9).
type Store struct {
	rdb redis.Cmdable
	now func() time.Time
}

var _ pipeline.FeatureReader = (*Store)(nil)

// New returns a Store on rdb.
func New(rdb redis.Cmdable) *Store { return &Store{rdb: rdb, now: time.Now} }

// PutItems writes item features with ItemTTL.
func (s *Store) PutItems(ctx context.Context, tid tenant.ID, items map[string]Item) error {
	now := s.now().UTC().Format(time.RFC3339)
	return s.batched(ctx, len(items), func(yield func(func(redis.Pipeliner))) {
		for id, it := range items {
			key := ItemKey(tid, id)
			yield(func(p redis.Pipeliner) {
				p.HSet(ctx, key, feature.CTR7d, it.CTR7d, feature.CTR30d, it.CTR30d, feature.CVR7d, it.CVR7d,
					feature.CVR30d, it.CVR30d, feature.Orders30dLog, it.Orders30dLog, fieldUpdatedAt, now)
				p.Expire(ctx, key, ItemTTL)
			})
		}
	})
}

// PutVendors applies partial vendor updates: only the fields an update sets are written, and
// later updates of the same vendor win.
func (s *Store) PutVendors(ctx context.Context, tid tenant.ID, vendors []catalog.Vendor) error {
	now := s.now().UTC().Format(time.RFC3339)
	return s.batched(ctx, len(vendors), func(yield func(func(redis.Pipeliner))) {
		for _, v := range vendors {
			if v.ID == "" || v.Empty() {
				continue
			}
			f := map[string]any{fieldUpdatedAt: now}
			for k, x := range v.Floats() {
				f[k] = x
			}
			if v.Verified != nil {
				f[catalog.VendorVerified] = strconv.FormatBool(*v.Verified)
			}
			key := VendorKey(tid, v.ID)
			yield(func(p redis.Pipeliner) { p.HSet(ctx, key, f) })
		}
	})
}

// PutUsers writes user features with UserTTL.
func (s *Store) PutUsers(ctx context.Context, tid tenant.ID, users map[string]User) error {
	now := s.now().UTC().Format(time.RFC3339)
	var encErr error
	err := s.batched(ctx, len(users), func(yield func(func(redis.Pipeliner))) {
		for id, u := range users {
			aff, err := json.Marshal(u.CategoryAffinity)
			if err != nil {
				encErr = fmt.Errorf("user %s: %w", id, err)
				return
			}
			key := UserKey(tid, id)
			yield(func(p redis.Pipeliner) {
				p.HSet(ctx, key, userAffinity, string(aff), userPriceLo, u.PriceLo, userPriceHi, u.PriceHi, fieldUpdatedAt, now)
				p.Expire(ctx, key, UserTTL)
			})
		}
	})
	return errors.Join(encErr, err)
}

// PutPropensity stores the tenant's propensity estimate.
func (s *Store) PutPropensity(ctx context.Context, tid tenant.ID, p Propensity) error {
	err := s.rdb.HSet(ctx, PropensityKey(tid), propensityEta, p.Eta, propensityClicks, p.Clicks,
		fieldUpdatedAt, p.UpdatedAt.UTC().Format(time.RFC3339)).Err()
	if err != nil {
		return fmt.Errorf("put propensity: %w", err)
	}
	return nil
}

// GetPropensity returns the stored estimate; ok is false when there is none.
func (s *Store) GetPropensity(ctx context.Context, tid tenant.ID) (Propensity, bool, error) {
	vals, err := s.rdb.HMGet(ctx, PropensityKey(tid), propensityEta, propensityClicks, fieldUpdatedAt).Result()
	if err != nil {
		return Propensity{}, false, fmt.Errorf("get propensity: %w", err)
	}
	eta, ok := parseFloat(vals[0])
	if !ok {
		return Propensity{}, false, nil
	}
	p := Propensity{Eta: eta}
	if c, ok := parseFloat(vals[1]); ok {
		p.Clicks = int64(c)
	}
	if s, ok := vals[2].(string); ok {
		p.UpdatedAt, _ = time.Parse(time.RFC3339, s)
	}
	return p, true, nil
}

// batched runs the writes gen yields in pipelines of writeBatch.
func (s *Store) batched(ctx context.Context, n int, gen func(yield func(func(redis.Pipeliner)))) error {
	if n == 0 {
		return nil
	}
	pipe := s.rdb.Pipeline()
	queued := 0
	var err error
	flush := func() {
		if queued == 0 || err != nil {
			return
		}
		if _, e := pipe.Exec(ctx); e != nil {
			err = fmt.Errorf("write features: %w", e)
		}
		queued = 0
	}
	gen(func(write func(redis.Pipeliner)) {
		if err != nil {
			return
		}
		write(pipe)
		if queued++; queued >= writeBatch {
			flush()
		}
	})
	flush()
	return err
}

// Read fetches item, vendor and user features in one pipelined round trip. At most
// MaxVendorsPerRequest vendors are read, in the order given.
func (s *Store) Read(ctx context.Context, tid tenant.ID, req pipeline.FeatureRequest) (*pipeline.OnlineFeatures, error) {
	vendors := req.VendorIDs[:min(len(req.VendorIDs), MaxVendorsPerRequest)]
	pipe := s.rdb.Pipeline()
	items := make([]*redis.SliceCmd, len(req.ProductIDs))
	for i, id := range req.ProductIDs {
		items[i] = pipe.HMGet(ctx, ItemKey(tid, id), itemFields...)
	}
	vcmds := make([]*redis.SliceCmd, len(vendors))
	for i, id := range vendors {
		vcmds[i] = pipe.HMGet(ctx, VendorKey(tid, id), vendorFields...)
	}
	var ucmd *redis.SliceCmd
	if req.UserID != "" {
		ucmd = pipe.HMGet(ctx, UserKey(tid, req.UserID), userAffinity, userPriceLo, userPriceHi)
	}
	if len(items)+len(vcmds) == 0 && ucmd == nil {
		return &pipeline.OnlineFeatures{}, nil
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("read features: %w", err)
	}

	out := &pipeline.OnlineFeatures{
		Items:   make(map[string]map[string]float64, len(items)),
		Vendors: make(map[string]map[string]float64, len(vcmds)),
	}
	for i, cmd := range items {
		if f := floats(cmd.Val(), itemFields); f != nil {
			out.Items[req.ProductIDs[i]] = f
		}
	}
	for i, cmd := range vcmds {
		if f := vendorFeatures(cmd.Val()); f != nil {
			out.Vendors[vendors[i]] = f
		}
	}
	if ucmd != nil {
		out.User = userFeatures(ucmd.Val())
	}
	return out, nil
}

// floats maps HMGET values to names, skipping missing or unparsable ones; nil when none is set.
func floats(vals []any, names []string) map[string]float64 {
	var f map[string]float64
	for i, v := range vals {
		if x, ok := parseFloat(v); ok {
			if f == nil {
				f = make(map[string]float64, len(names))
			}
			f[names[i]] = x
		}
	}
	return f
}

func vendorFeatures(vals []any) map[string]float64 {
	raw := floats(vals[:4], []string{feature.VendorScore, feature.VendorOnTimeDispatch, feature.VendorDisputeRate, feature.VendorResponseHours})
	verified, isSet := vals[4].(string)
	if raw == nil && !isSet {
		return nil
	}
	if raw == nil {
		raw = map[string]float64{}
	}
	if h, ok := raw[feature.VendorResponseHours]; ok {
		raw[feature.VendorResponseHours] = math.Min(h/ResponseHoursCap, 1)
	}
	if isSet {
		raw[feature.VendorVerified] = 0
		if b, _ := strconv.ParseBool(verified); b {
			raw[feature.VendorVerified] = 1
		}
	}
	return raw
}

func userFeatures(vals []any) *pipeline.UserFeatures {
	raw, ok := vals[0].(string)
	if !ok {
		return nil
	}
	u := &pipeline.UserFeatures{}
	if err := json.Unmarshal([]byte(raw), &u.CategoryAffinity); err != nil {
		return nil
	}
	lo, okLo := parseFloat(vals[1])
	hi, okHi := parseFloat(vals[2])
	if okLo && okHi && hi >= lo {
		u.PriceLo, u.PriceHi = int64(lo), int64(hi)
	}
	return u
}

func parseFloat(v any) (float64, bool) {
	s, ok := v.(string)
	if !ok {
		return 0, false
	}
	x, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
		return 0, false
	}
	return x, true
}
