package events

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/zavisoft/search-engine/internal/tenant"
)

// Topic carries interaction events for all tenants (PLAN.md D5); ClickHouse consumes it.
const Topic = "search.events.v1"

// SchemaVersion is sent in the schema-version header.
const SchemaVersion = 1

// wire is the Kafka value: the event plus the tenant from the caller's token.
type wire struct {
	TenantID string `json:"tenant_id"`
	Event
}

// Record builds e's Kafka record, keyed {tenant_id}:{event_id}.
func Record(topic string, tid tenant.ID, e Event) (*kgo.Record, error) {
	val, err := json.Marshal(wire{TenantID: string(tid), Event: e})
	if err != nil {
		return nil, fmt.Errorf("encode event %s: %w", e.EventID, err)
	}
	return &kgo.Record{
		Topic: topic,
		Key:   []byte(string(tid) + ":" + e.EventID),
		Value: val,
		Headers: []kgo.RecordHeader{
			{Key: "tenant-id", Value: []byte(tid)},
			{Key: "event-id", Value: []byte(e.EventID)},
			{Key: "type", Value: []byte(e.Type)},
			{Key: "schema-version", Value: []byte(strconv.Itoa(SchemaVersion))},
		},
	}, nil
}

// Producer writes events to the events topic.
type Producer struct {
	client  *kgo.Client
	topic   string
	dropped metric.Int64Counter
}

// NewProducer wraps client for topic.
func NewProducer(client *kgo.Client, topic string, meter metric.Meter) (*Producer, error) {
	dropped, err := meter.Int64Counter("events_dropped_total",
		metric.WithDescription("Search-emitted events dropped because Kafka was unavailable or the producer buffer was full."))
	if err != nil {
		return nil, fmt.Errorf("dropped counter: %w", err)
	}
	return &Producer{client: client, topic: topic, dropped: dropped}, nil
}

// Produce writes evs and waits until Kafka acknowledges all of them (POST /v1/events).
func (p *Producer) Produce(ctx context.Context, tid tenant.ID, evs []Event) error {
	recs := make([]*kgo.Record, len(evs))
	for i, e := range evs {
		r, err := Record(p.topic, tid, e)
		if err != nil {
			return err
		}
		recs[i] = r
	}
	if err := p.client.ProduceSync(ctx, recs...).FirstErr(); err != nil {
		return fmt.Errorf("produce events: %w", err)
	}
	return nil
}

// Emit queues evs without waiting (impressions emitted by search-api). When the client's buffer
// is full or a write fails, the events are dropped and counted, so a Kafka outage never slows
// or fails a search.
func (p *Producer) Emit(tid tenant.ID, evs []Event) {
	attrs := metric.WithAttributes(attribute.String("tenant", string(tid)))
	for _, e := range evs {
		r, err := Record(p.topic, tid, e)
		if err != nil {
			p.dropped.Add(context.Background(), 1, attrs)
			continue
		}
		// The record outlives the request by design.
		p.client.TryProduce(context.Background(), r, func(_ *kgo.Record, err error) {
			if err != nil {
				p.dropped.Add(context.Background(), 1, attrs)
			}
		})
	}
}

// Flush waits for queued events, for graceful shutdown.
func (p *Producer) Flush(ctx context.Context) error {
	if err := p.client.Flush(ctx); err != nil {
		return fmt.Errorf("flush events: %w", err)
	}
	return nil
}
