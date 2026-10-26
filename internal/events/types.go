package events

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/zavisoft/search-engine/internal/platform"
)

// Type is an interaction event type (docs/API.md §1).
type Type string

// Interaction event types.
const (
	TypeImpression Type = "impression"
	TypeClick      Type = "click"
	TypeAddToCart  Type = "add_to_cart"
	TypeOrder      Type = "order"
	TypeRFQ        Type = "rfq"
)

// Types lists the accepted event types.
var Types = []Type{TypeImpression, TypeClick, TypeAddToCart, TypeOrder, TypeRFQ}

// Batch and field limits of POST /v1/events.
const (
	MaxBatchSize = 500
	MaxIDLength  = 256
	MaxPosition  = 10000
	// MaxClockSkew is how far in the future an event's ts may be.
	MaxClockSkew = 5 * time.Minute
)

// Event is one interaction event (api/openapi/search.yaml Event). The tenant comes from the
// caller's token, never from the body.
type Event struct {
	EventID   string    `json:"event_id"`
	Type      Type      `json:"type"`
	TS        time.Time `json:"ts"`
	RequestID string    `json:"request_id,omitempty"`
	ProductID string    `json:"product_id"`
	Position  int       `json:"position,omitempty"`
	UserID    string    `json:"user_id,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	Sponsored bool      `json:"sponsored"`
	// Randomized marks an impression shown in a randomized order; featurejob estimates the
	// examination propensity per position from these requests (docs/DATA.md §5).
	Randomized bool    `json:"randomized,omitempty"`
	OrderID    *string `json:"order_id,omitempty"`
	Quantity   *int    `json:"quantity,omitempty"`
	ValueMinor *int64  `json:"value_minor,omitempty"`
}

// ValidateBatch checks the batch size and every event, reporting all errors, and drops repeated
// event IDs within the batch (the first wins). Repeats across batches are collapsed by
// ClickHouse (ReplacingMergeTree on event_id).
func ValidateBatch(evs []Event, now time.Time) ([]Event, []platform.FieldError) {
	if len(evs) == 0 {
		return nil, []platform.FieldError{{Path: "/events", Code: "required", Message: "events must not be empty"}}
	}
	if len(evs) > MaxBatchSize {
		return nil, []platform.FieldError{{Path: "/events", Code: "max_items", Message: fmt.Sprintf("at most %d events per batch", MaxBatchSize)}}
	}
	var errs []platform.FieldError
	seen := make(map[string]bool, len(evs))
	out := make([]Event, 0, len(evs))
	for i, e := range evs {
		errs = append(errs, e.validate(fmt.Sprintf("/events/%d", i), now)...)
		if !seen[e.EventID] {
			seen[e.EventID] = true
			out = append(out, e)
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return out, nil
}

func (e Event) validate(ptr string, now time.Time) []platform.FieldError {
	var errs []platform.FieldError
	add := func(field, code, msg string) {
		errs = append(errs, platform.FieldError{Path: ptr + "/" + field, Code: code, Message: msg})
	}
	if _, err := uuid.Parse(e.EventID); err != nil {
		add("event_id", "invalid", "event_id must be a UUID")
	}
	if !slices.Contains(Types, e.Type) {
		add("type", "invalid", fmt.Sprintf("type must be one of %v", Types))
	}
	switch {
	case e.TS.IsZero():
		add("ts", "required", "ts is required")
	case e.TS.After(now.Add(MaxClockSkew)):
		add("ts", "out_of_range", "ts is in the future")
	}
	if e.ProductID == "" {
		add("product_id", "required", "product_id is required")
	}
	for _, f := range [][2]string{{"product_id", e.ProductID}, {"request_id", e.RequestID}, {"user_id", e.UserID}, {"session_id", e.SessionID}} {
		if len(f[1]) > MaxIDLength {
			add(f[0], "too_long", fmt.Sprintf("%s is longer than %d bytes", f[0], MaxIDLength))
		}
	}
	switch {
	case e.Position < 0 || e.Position > MaxPosition:
		add("position", "out_of_range", fmt.Sprintf("position must be within [1, %d]", MaxPosition))
	case e.Type == TypeImpression && e.Position == 0:
		add("position", "required", "impressions need a position")
	}
	if e.Randomized && e.Type != TypeImpression {
		add("randomized", "invalid", "only impressions can be randomized")
	}
	if e.Type == TypeOrder {
		if e.OrderID == nil || *e.OrderID == "" || len(*e.OrderID) > MaxIDLength {
			add("order_id", "required", "orders need an order_id")
		}
		if e.Quantity == nil || *e.Quantity < 1 {
			add("quantity", "required", "orders need a quantity >= 1")
		}
		if e.ValueMinor == nil || *e.ValueMinor < 0 {
			add("value_minor", "required", "orders need a value_minor >= 0")
		}
	}
	return errs
}
