package events

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.opentelemetry.io/otel/metric/noop"

	"github.com/zavisoft/search-engine/internal/tenant"
)

var now = time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)

const (
	id1 = "a0000000-0000-4000-8000-000000000001"
	id2 = "a0000000-0000-4000-8000-000000000002"
)

func ptr[T any](v T) *T { return &v }

func TestValidateBatch(t *testing.T) {
	order := Event{EventID: id1, Type: TypeOrder, TS: now, ProductID: "p1", OrderID: ptr("o1"), Quantity: ptr(2), ValueMinor: ptr(int64(4500))}
	for name, tc := range map[string]struct {
		events   []Event
		want     int
		errPaths []string
	}{
		"empty":    {nil, 0, []string{"/events"}},
		"valid":    {[]Event{{EventID: id1, Type: TypeImpression, TS: now, ProductID: "p1", Position: 1, Randomized: true}, {EventID: id2, Type: TypeClick, TS: now, ProductID: "p1"}}, 2, nil},
		"dedup":    {[]Event{{EventID: id1, Type: TypeClick, TS: now, ProductID: "p1"}, {EventID: id1, Type: TypeClick, TS: now, ProductID: "p1"}}, 1, nil},
		"order":    {[]Event{order}, 1, nil},
		"bad uuid": {[]Event{{EventID: "x", Type: TypeClick, TS: now, ProductID: "p1"}}, 0, []string{"/events/0/event_id"}},
		"bad type": {[]Event{{EventID: id1, Type: "view", TS: now, ProductID: "p1"}}, 0, []string{"/events/0/type"}},
		"every missing field": {[]Event{{EventID: id1, Type: TypeOrder}}, 0,
			[]string{"/events/0/ts", "/events/0/product_id", "/events/0/order_id", "/events/0/quantity", "/events/0/value_minor"}},
		"future":            {[]Event{{EventID: id1, Type: TypeClick, TS: now.Add(time.Hour), ProductID: "p1"}}, 0, []string{"/events/0/ts"}},
		"impression no pos": {[]Event{{EventID: id1, Type: TypeImpression, TS: now, ProductID: "p1"}}, 0, []string{"/events/0/position"}},
		"position too deep": {[]Event{{EventID: id1, Type: TypeClick, TS: now, ProductID: "p1", Position: MaxPosition + 1}}, 0, []string{"/events/0/position"}},
		"randomized click":  {[]Event{{EventID: id1, Type: TypeClick, TS: now, ProductID: "p1", Randomized: true}}, 0, []string{"/events/0/randomized"}},
	} {
		t.Run(name, func(t *testing.T) {
			got, errs := ValidateBatch(tc.events, now)
			if len(got) != tc.want || len(errs) != len(tc.errPaths) {
				t.Fatalf("got %d events, errors %+v", len(got), errs)
			}
			for i, p := range tc.errPaths {
				if errs[i].Path != p {
					t.Errorf("error %d path %s, want %s", i, errs[i].Path, p)
				}
			}
		})
	}

	big := make([]Event, MaxBatchSize+1)
	if _, errs := ValidateBatch(big, now); len(errs) != 1 || errs[0].Code != "max_items" {
		t.Errorf("oversized batch: %+v", errs)
	}
}

func TestRecordCarriesTheTokenTenant(t *testing.T) {
	r, err := Record(Topic, tenant.ID("t1"), Event{EventID: id1, Type: TypeClick, TS: now, ProductID: "p1", Position: 3})
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Key) != "t1:"+id1 || r.Topic != Topic {
		t.Errorf("key %s topic %s", r.Key, r.Topic)
	}
	headers := map[string]string{}
	for _, h := range r.Headers {
		headers[h.Key] = string(h.Value)
	}
	if headers["tenant-id"] != "t1" || headers["type"] != "click" || headers["schema-version"] != "1" {
		t.Errorf("headers %v", headers)
	}
	var body map[string]any
	if err := json.Unmarshal(r.Value, &body); err != nil {
		t.Fatal(err)
	}
	if body["tenant_id"] != "t1" || body["position"] != 3.0 || body["sponsored"] != false {
		t.Errorf("value %s", r.Value)
	}
	for _, k := range []string{"order_id", "quantity", "value_minor", "randomized"} {
		if _, ok := body[k]; ok {
			t.Errorf("%s must be omitted (NULL / default in ClickHouse): %s", k, r.Value)
		}
	}
}

func TestNewProducer(t *testing.T) {
	p, err := NewProducer(nil, Topic, RequestTopic, noop.NewMeterProvider().Meter("t"))
	if err != nil || p.topic != Topic {
		t.Fatalf("%v %+v", err, p)
	}
	_ = context.Background()
}
