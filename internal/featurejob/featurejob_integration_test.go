//go:build integration

package featurejob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zavisoft/search-engine/internal/events"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/providers/features"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

type staticDocs map[string]*pipeline.Doc

func (d staticDocs) Docs(_ context.Context, ids []string) (map[string]*pipeline.Doc, error) {
	out := map[string]*pipeline.Doc{}
	for _, id := range ids {
		if doc, ok := d[id]; ok {
			out[id] = doc
		}
	}
	return out, nil
}

// TestEventsToOnlineFeatures runs M6's data path on real infrastructure: events are produced to
// Kafka, ClickHouse ingests them through the Kafka engine and materialized view (migration
// 000003), and the job turns them into Redis features that S4 reads.
func TestEventsToOnlineFeatures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	nw := testinfra.Network(t)
	broker := testinfra.RedpandaOn(t, nw)
	kc, err := kgo.NewClient(kgo.SeedBrokers(broker), kgo.ProducerLinger(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer kc.Close()
	if _, err := kadm.NewClient(kc).CreateTopic(ctx, 3, 1, nil, events.Topic); err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
		t.Fatal(err)
	}
	ch, err := clickhouse.Open(&clickhouse.Options{Addr: []string{testinfra.ClickHouse(t, nw)},
		Auth: clickhouse.Auth{Database: "searchengine", Username: "search", Password: "search"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	producer, err := events.NewProducer(kc, events.Topic, noop.NewMeterProvider().Meter("t"))
	if err != nil {
		t.Fatal(err)
	}

	// Randomized traffic: 20 products shuffled over 20 positions, clicked with probability
	// 0.5·pos^-0.7 (the simulator's examination curve). "hot" is clicked twice as often and
	// ordered by u1, who also buys bags; t2's events must not leak into t1's features.
	const tid, other = tenant.ID("t1"), tenant.ID("t2")
	rng := rand.New(rand.NewPCG(42, 1))
	now := time.Now().UTC()
	var evs []events.Event
	newID := func() string {
		b := make([]byte, 16)
		for i := range b {
			b[i] = byte(rng.UintN(256))
		}
		b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	}
	products := []string{"hot"}
	for i := 1; i < 20; i++ {
		products = append(products, fmt.Sprintf("p%02d", i))
	}
	for r := range 400 {
		req := fmt.Sprintf("r%04d", r)
		ts := now.Add(-time.Duration(r) * 30 * time.Minute) // spread over ~8 days
		order := rng.Perm(len(products))
		for pos, idx := range order {
			pid, k := products[idx], pos+1
			evs = append(evs, events.Event{EventID: newID(), Type: events.TypeImpression, TS: ts, RequestID: req,
				ProductID: pid, Position: k, UserID: "u2", Randomized: true})
			p := 0.5 * math.Pow(float64(k), -0.7)
			if pid == "hot" {
				p = math.Min(2*p, 1)
			}
			if rng.Float64() < p {
				evs = append(evs, events.Event{EventID: newID(), Type: events.TypeClick, TS: ts.Add(time.Second), RequestID: req, ProductID: pid, Position: k, UserID: "u2"})
			}
		}
	}
	qty, val := 1, int64(4000)
	for i := range 4 {
		oid := fmt.Sprintf("o%d", i)
		evs = append(evs,
			events.Event{EventID: newID(), Type: events.TypeClick, TS: now.Add(-time.Hour), ProductID: "hot", UserID: "u1"},
			events.Event{EventID: newID(), Type: events.TypeOrder, TS: now.Add(-time.Hour), ProductID: "hot", UserID: "u1", OrderID: &oid, Quantity: &qty, ValueMinor: &val},
			events.Event{EventID: newID(), Type: events.TypeClick, TS: now.Add(-time.Hour), ProductID: "p01", UserID: "u1"},
			events.Event{EventID: newID(), Type: events.TypeClick, TS: now.Add(-time.Hour), ProductID: "p02", UserID: "u1"})
	}
	t.Logf("producing %d events", len(evs))
	start := time.Now()
	for i := 0; i < len(evs); i += events.MaxBatchSize {
		batch, errs := events.ValidateBatch(evs[i:min(i+events.MaxBatchSize, len(evs))], now)
		if len(errs) > 0 {
			t.Fatalf("test events must be valid: %+v", errs[0])
		}
		if err := producer.Produce(ctx, tid, batch); err != nil {
			t.Fatal(err)
		}
	}
	// A resent event (same ID) and another tenant's traffic.
	if err := producer.Produce(ctx, tid, evs[:1]); err != nil {
		t.Fatal(err)
	}
	if err := producer.Produce(ctx, other, []events.Event{{EventID: newID(), Type: events.TypeClick, TS: now, ProductID: "hot", UserID: "u1"}}); err != nil {
		t.Fatal(err)
	}

	var rows uint64
	for rows < uint64(len(evs)) {
		if ctx.Err() != nil {
			t.Fatalf("ClickHouse ingested %d of %d events", rows, len(evs))
		}
		time.Sleep(time.Second)
		if err := ch.QueryRow(ctx, "SELECT count() FROM events FINAL WHERE tenant_id = 't1'").Scan(&rows); err != nil {
			t.Fatal(err)
		}
	}
	ingest := time.Since(start)
	var randomized uint64
	if err := ch.QueryRow(ctx, "SELECT countIf(randomized) FROM events FINAL WHERE tenant_id = 't1'").Scan(&randomized); err != nil || randomized != 8000 {
		t.Errorf("randomized impressions = %d (%v), want 8000", randomized, err)
	}

	mr := miniredis.RunT(t)
	store := features.New(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	docs := staticDocs{"hot": {CategoryIDs: []string{"bags", "handbags"}, PriceMinor: 4000}, "p01": {CategoryIDs: []string{"shoes"}, PriceMinor: 9000}}
	for _, p := range products[2:] {
		docs[p] = &pipeline.Doc{CategoryIDs: []string{"misc"}, PriceMinor: 1000}
	}
	job := &Job{CH: ch, Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: time.Now,
		Docs: func(tenant.ID) DocSource { return docs }}
	rep, err := job.Run(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("ingest %v, job %+v; event → feature latency %v", ingest.Round(time.Millisecond), rep, time.Since(start).Round(time.Millisecond))
	if rep.EtaSource != "estimated" || math.Abs(rep.Eta-0.7) > 0.15 {
		t.Errorf("propensity eta %.3f (%s), want ≈ 0.7 estimated", rep.Eta, rep.EtaSource)
	}
	if rep.Items != len(products) || rep.Users != 2 {
		t.Errorf("items %d users %d, want %d and 2", rep.Items, rep.Users, len(products))
	}

	got, err := store.Read(ctx, tid, pipeline.FeatureRequest{ProductIDs: products, UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	hot := got.Items["hot"]
	var others float64
	for _, p := range products[1:] {
		others += got.Items[p][feature.CTR7d]
	}
	if mean := others / float64(len(products)-1); hot[feature.CTR7d] < 1.5*mean {
		t.Errorf("hot CTR %.3f vs mean %.3f; debiased CTR should show it is clicked twice as often", hot[feature.CTR7d], mean)
	}
	if hot[feature.CVR30d] <= got.Items["p05"][feature.CVR30d] || hot[feature.Orders30dLog] != math.Log1p(4)/10 {
		t.Errorf("hot = %v; it has the only orders", hot)
	}
	// u1's weights: hot 16 (4 clicks + 4 orders at 3), p01 4, p02 4.
	if u := got.User; u == nil || math.Abs(u.CategoryAffinity["bags"]-16.0/24) > 1e-3 || u.PriceLo != 4000 || u.PriceHi != 4000 {
		t.Errorf("u1 = %+v; mostly orders of a 4000 bag", got.User)
	}
	if len(mr.Keys()) == 0 {
		t.Fatal("no keys written")
	}
	for _, k := range mr.Keys() {
		if k[:6] != "f:{t1}" {
			t.Errorf("key %s written for another tenant", k)
		}
	}
	if ingest > 15*time.Minute {
		t.Errorf("freshness budget exceeded: %v", ingest)
	}
}
