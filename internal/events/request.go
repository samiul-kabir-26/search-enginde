package events

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/zavisoft/search-engine/internal/tenant"
)

// RequestTopic carries one record per served search page (the request log), which ClickHouse
// keeps in search_requests: training joins it with impressions to recover each request's query
// (docs/MODELS.md §5).
const RequestTopic = "search.requests.v1"

// Request types.
const (
	RequestText    = "text"
	RequestImage   = "image"
	RequestSimilar = "similar"
	// RequestBrowse is a category page served outside search-api (the simulator's browse
	// sessions); Query holds the category.
	RequestBrowse = "browse"
)

// Request is one served search page.
type Request struct {
	RequestID string    `json:"request_id"`
	TS        time.Time `json:"ts"`
	Type      string    `json:"type"`
	// Query is the request text (empty for image-only queries).
	Query string `json:"query,omitempty"`
	// AnchorID is the product a similar-items request started from.
	AnchorID      string `json:"anchor_id,omitempty"`
	UserID        string `json:"user_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	ConfigVersion int    `json:"config_version,omitempty"`
	// PageOffset is the page's first position minus one; Results is how many were returned.
	PageOffset int `json:"page_offset"`
	Results    int `json:"results"`
}

type requestWire struct {
	TenantID string `json:"tenant_id"`
	Request
}

// RequestRecord builds r's Kafka record, keyed {tenant_id}:{request_id}.
func RequestRecord(topic string, tid tenant.ID, r Request) (*kgo.Record, error) {
	val, err := json.Marshal(requestWire{TenantID: string(tid), Request: r})
	if err != nil {
		return nil, fmt.Errorf("encode request %s: %w", r.RequestID, err)
	}
	return &kgo.Record{
		Topic: topic, Key: []byte(string(tid) + ":" + r.RequestID), Value: val,
		Headers: []kgo.RecordHeader{
			{Key: "tenant-id", Value: []byte(tid)},
			{Key: "schema-version", Value: []byte(strconv.Itoa(SchemaVersion))},
		},
	}, nil
}
