//go:build integration

package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/testinfra"
)

func produce(t *testing.T, cl *kgo.Client, events []catalog.ProductEvent, tombstones ...string) {
	t.Helper()
	var records []*kgo.Record
	for _, e := range events {
		v, _ := json.Marshal(e)
		r := &kgo.Record{Topic: catalog.Topic, Key: e.Key(), Value: v}
		for k, hv := range e.Headers() {
			r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(hv)})
		}
		records = append(records, r)
	}
	for _, key := range tombstones {
		records = append(records, &kgo.Record{Topic: catalog.Topic, Key: []byte(key)})
	}
	if err := cl.ProduceSync(context.Background(), records...).FirstErr(); err != nil {
		t.Fatal(err)
	}
}

// TestCatchUpAcrossPasses checks that repeated catch-up passes resume where the previous one
// stopped (a regression: the second pass used to restart from the log start and hang), and
// that tombstones and other tenants' events decode as expected.
func TestCatchUpAcrossPasses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	broker := testinfra.Redpanda(t)
	cl, err := kgo.NewClient(kgo.SeedBrokers(broker), kgo.ConsumeTopics(catalog.Topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()), kgo.AllowAutoTopicCreation())
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	// Three partitions, unless the broker already auto-created the topic on first metadata.
	if _, err := kadm.NewClient(cl).CreateTopic(ctx, 3, 1, nil, catalog.Topic); err != nil && !errors.Is(err, kerr.TopicAlreadyExists) {
		t.Fatal(err)
	}

	var batch []catalog.ProductEvent
	for i := range 30 {
		e := upsert(fmt.Sprintf("p%d", i), 1, "")
		if i%10 == 0 {
			e.TenantID = "01JOTHER00000000000000000B"
		}
		batch = append(batch, e)
	}
	produce(t, cl, batch)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	seen := map[string]catalog.Op{}
	handle := func(records []*kgo.Record) error {
		byTenant, invalid := groupByTenant(records, log)
		if invalid != 0 {
			return fmt.Errorf("%d undecodable records", invalid)
		}
		for _, e := range byTenant[tid] {
			seen[e.ProductID] = e.Op
		}
		return nil
	}

	ends := map[int32]*partitionProgress{}
	if err := catchUp(ctx, cl, ends, handle); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 27 {
		t.Fatalf("pass 1 saw %d products for the tenant, want 27", len(seen))
	}

	produce(t, cl, []catalog.ProductEvent{upsert("late", 1, "")}, string(tid)+":p1")
	if err := catchUp(ctx, cl, ends, handle); err != nil {
		t.Fatal(err)
	}
	if seen["late"] != catalog.OpUpsert || seen["p1"] != catalog.OpDelete {
		t.Fatalf("pass 2 missed new events: late=%q p1=%q", seen["late"], seen["p1"])
	}

	done := make(chan error, 1)
	go func() { done <- catchUp(ctx, cl, ends, handle) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("catch-up with nothing new must return immediately")
	}
}
