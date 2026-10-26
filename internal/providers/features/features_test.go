package features

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/tenant"
)

const tid = tenant.ID("t1")

func newStore(t *testing.T) (*Store, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	return New(redis.NewClient(&redis.Options{Addr: mr.Addr()})), mr
}

func ptr[T any](v T) *T { return &v }

func TestKeysShareTheTenantSlot(t *testing.T) {
	for _, k := range []string{ItemKey(tid, "p"), VendorKey(tid, "v"), UserKey(tid, "u"), PropensityKey(tid)} {
		if k[:6] != "f:{t1}" {
			t.Errorf("%s lacks the {tid} hash tag", k)
		}
	}
}

func TestReadRoundTripAndTTLs(t *testing.T) {
	s, mr := newStore(t)
	ctx := context.Background()
	items := map[string]Item{}
	for i := range 1200 { // more than one write pipeline
		items[fmt.Sprintf("p%d", i)] = Item{CTR7d: 0.1, CTR30d: 0.2, CVR7d: 0.3, CVR30d: 0.4, Orders30dLog: 0.5}
	}
	if err := s.PutItems(ctx, tid, items); err != nil {
		t.Fatal(err)
	}
	if err := s.PutVendors(ctx, tid, []catalog.Vendor{
		{ID: "v1", Score: ptr(0.8), ResponseHours: ptr(96.0), Verified: ptr(true)},
		{ID: "v2", Verified: ptr(false)},
		{ID: "skipped"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutUsers(ctx, tid, map[string]User{"u1": {CategoryAffinity: map[string]float64{"bags": 0.6}, PriceLo: 1000, PriceHi: 5000}}); err != nil {
		t.Fatal(err)
	}
	if ttl := mr.TTL(ItemKey(tid, "p7")); ttl != ItemTTL {
		t.Errorf("item TTL %v", ttl)
	}
	if ttl := mr.TTL(UserKey(tid, "u1")); ttl != UserTTL {
		t.Errorf("user TTL %v", ttl)
	}
	if ttl := mr.TTL(VendorKey(tid, "v1")); ttl != 0 {
		t.Errorf("vendor features must not expire, TTL %v", ttl)
	}
	if mr.Exists(VendorKey(tid, "skipped")) {
		t.Error("an empty vendor update must not create a key")
	}

	got, err := s.Read(ctx, tid, pipeline.FeatureRequest{
		ProductIDs: []string{"p7", "missing"}, VendorIDs: []string{"v1", "v2", "nope"}, UserID: "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if f := got.Items["p7"]; f[feature.CTR7d] != 0.1 || f[feature.Orders30dLog] != 0.5 || len(f) != 5 {
		t.Errorf("item = %v", f)
	}
	if _, ok := got.Items["missing"]; ok {
		t.Error("a product without features must be absent")
	}
	if v := got.Vendors["v1"]; v[feature.VendorScore] != 0.8 || v[feature.VendorResponseHours] != 1 || v[feature.VendorVerified] != 1 {
		t.Errorf("v1 = %v (96 h caps at 1)", v)
	}
	if v := got.Vendors["v2"]; len(v) != 1 || v[feature.VendorVerified] != 0 {
		t.Errorf("v2 = %v; only verified was set", v)
	}
	if got.User == nil || got.User.CategoryAffinity["bags"] != 0.6 || got.User.PriceLo != 1000 || got.User.PriceHi != 5000 {
		t.Errorf("user = %+v", got.User)
	}

	other, err := s.Read(ctx, "t2", pipeline.FeatureRequest{ProductIDs: []string{"p7"}, UserID: "u1"})
	if err != nil || len(other.Items) != 0 || other.User != nil {
		t.Errorf("another tenant must see nothing: %+v %v", other, err)
	}
}

func TestReadCapsVendors(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	ids := make([]string, MaxVendorsPerRequest+5)
	var vs []catalog.Vendor
	for i := range ids {
		ids[i] = fmt.Sprintf("v%d", i)
		vs = append(vs, catalog.Vendor{ID: ids[i], Score: ptr(0.5)})
	}
	if err := s.PutVendors(ctx, tid, vs); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(ctx, tid, pipeline.FeatureRequest{VendorIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Vendors) != MaxVendorsPerRequest {
		t.Errorf("read %d vendors, want the first %d", len(got.Vendors), MaxVendorsPerRequest)
	}
}

func TestReadFailsWhenRedisIsDown(t *testing.T) {
	s, mr := newStore(t)
	mr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := s.Read(ctx, tid, pipeline.FeatureRequest{ProductIDs: []string{"p"}}); err == nil {
		t.Error("want an error so S4 reports the fallback")
	}
	if got, err := s.Read(ctx, tid, pipeline.FeatureRequest{}); err != nil || got == nil {
		t.Errorf("an empty request needs no round trip: %v", err)
	}
}

func TestPropensity(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, ok, err := s.GetPropensity(ctx, tid); ok || err != nil {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := s.PutPropensity(ctx, tid, Propensity{Eta: 0.71, Clicks: 1234, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	p, ok, err := s.GetPropensity(ctx, tid)
	if err != nil || !ok || p.Eta != 0.71 || p.Clicks != 1234 || !p.UpdatedAt.Equal(at) {
		t.Errorf("got %+v ok=%v err=%v", p, ok, err)
	}
}
