package searchapi

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/events"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Body limits of the write endpoints.
const (
	MaxEventsBodyBytes  = 1 << 20
	MaxVendorsBodyBytes = 256 << 10
	MaxVendorsPerBatch  = 500
)

// EventSink writes interaction events to Kafka.
type EventSink interface {
	// Produce writes evs and waits for the acknowledgement.
	Produce(ctx context.Context, tid tenant.ID, evs []events.Event) error
	// Emit queues a served page's request record and impressions without waiting; failures
	// drop them.
	Emit(tid tenant.ID, req events.Request, evs []events.Event)
}

// VendorStore applies partial vendor feature updates.
type VendorStore interface {
	PutVendors(ctx context.Context, tid tenant.ID, vendors []catalog.Vendor) error
}

// EventsRequest is the body of POST /v1/events.
type EventsRequest struct {
	Events []events.Event `json:"events"`
}

// EventsResponse is the 202 body of POST /v1/events.
type EventsResponse struct {
	Accepted int `json:"accepted"`
}

func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _ := auth.FromContext(ctx)
	if s.Events == nil {
		platform.WriteProblem(w, platform.Problem{Status: http.StatusServiceUnavailable, Detail: "event ingestion is disabled"})
		return
	}
	var req EventsRequest
	if err := decodeStrictN(r, &req, MaxEventsBodyBytes); err != nil {
		badRequest(w, platform.FieldError{Path: "", Code: "invalid_json", Message: err.Error()})
		return
	}
	evs, errs := events.ValidateBatch(req.Events, time.Now())
	if len(errs) > 0 {
		badRequest(w, errs...)
		return
	}
	// The plan's events rate counts events; the burst admits one full batch.
	res, err := s.Resolver.Get(ctx, p.Tenant)
	if !s.resolved(w, p, err) || !s.admit(ctx, w, res, "events", res.Plan.EventsRPS, events.MaxBatchSize, len(evs)) {
		return
	}
	s.settleSponsoredClicks(ctx, p.Tenant, evs, time.Now())
	if err := s.Events.Produce(ctx, p.Tenant, evs); err != nil {
		s.Log.Error("produce events", "tenant", p.Tenant, "events", len(evs), "err", err)
		platform.WriteProblem(w, platform.Problem{Status: http.StatusServiceUnavailable, Detail: "event ingestion is unavailable; retry the batch"})
		return
	}
	platform.WriteJSON(w, http.StatusAccepted, EventsResponse{Accepted: len(evs)})
}

// settleSponsoredClicks charges sponsored clicks at the clearing price recorded when the ad was
// served (CPC, PLAN.md R10) and sets it as the click's value_minor, which the ads report sums.
// The value is the platform's, never the client's: a sponsored click without a recorded
// placement (expired, or never served) carries no value.
func (s *Service) settleSponsoredClicks(ctx context.Context, tid tenant.ID, evs []events.Event, now time.Time) {
	if s.AdsPacing == nil {
		return
	}
	for i := range evs {
		e := &evs[i]
		if e.Type != events.TypeClick || !e.Sponsored {
			continue
		}
		e.ValueMinor = nil
		if e.RequestID == "" {
			continue
		}
		_, price, ok, err := s.AdsPacing.Charge(ctx, tid, e.RequestID, e.ProductID, now)
		if err != nil {
			s.Log.Warn("settle sponsored click", "tenant", tid, "err", err)
			continue
		}
		if ok {
			e.ValueMinor = &price
		}
	}
}

// VendorsRequest is the body of PUT /v1/vendors.
type VendorsRequest struct {
	Vendors []catalog.Vendor `json:"vendors"`
}

// VendorsResponse is the body of a successful PUT /v1/vendors.
type VendorsResponse struct {
	Updated int `json:"updated"`
}

func (s *Service) handleVendors(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _ := auth.FromContext(ctx)
	if s.Vendors == nil {
		platform.WriteProblem(w, platform.Problem{Status: http.StatusServiceUnavailable, Detail: "vendor updates are disabled"})
		return
	}
	var req VendorsRequest
	if err := decodeStrictN(r, &req, MaxVendorsBodyBytes); err != nil {
		badRequest(w, platform.FieldError{Path: "", Code: "invalid_json", Message: err.Error()})
		return
	}
	var errs []platform.FieldError
	switch n := len(req.Vendors); {
	case n == 0:
		errs = append(errs, platform.FieldError{Path: "/vendors", Code: "required", Message: "vendors must not be empty"})
	case n > MaxVendorsPerBatch:
		errs = append(errs, platform.FieldError{Path: "/vendors", Code: "max_items", Message: fmt.Sprintf("at most %d vendors per batch", MaxVendorsPerBatch)})
	}
	for i, v := range req.Vendors {
		errs = append(errs, v.Validate(fmt.Sprintf("/vendors/%d", i))...)
	}
	if len(errs) > 0 {
		badRequest(w, errs...)
		return
	}
	res, err := s.Resolver.Get(ctx, p.Tenant)
	if !s.resolved(w, p, err) || !s.admit(ctx, w, res, "vendors", res.Plan.EventsRPS, MaxVendorsPerBatch, len(req.Vendors)) {
		return
	}
	if err := s.Vendors.PutVendors(ctx, p.Tenant, req.Vendors); err != nil {
		s.Log.Error("put vendors", "tenant", p.Tenant, "vendors", len(req.Vendors), "err", err)
		platform.WriteProblem(w, platform.Problem{Status: http.StatusServiceUnavailable, Detail: "vendor updates are unavailable; retry the batch"})
		return
	}
	platform.WriteJSON(w, http.StatusOK, VendorsResponse{Updated: len(req.Vendors)})
}

// emitServed queues the page's request record (the request log training joins with
// impressions) and one impression per returned result (docs/ARCHITECTURE.md §2). Tenants send
// clicks, carts and orders that reference resp.RequestID.
func (s *Service) emitServed(tid tenant.ID, resp *SearchResponse, q *pipeline.Query) {
	if s.Events == nil {
		return
	}
	now, u := time.Now().UTC(), q.User
	req := events.Request{
		RequestID: resp.RequestID, TS: now, Type: string(q.Type), Query: q.Raw, AnchorID: q.AnchorID,
		UserID: u.ID, SessionID: u.SessionID, ConfigVersion: resp.ConfigVersion, PageOffset: resp.Offset, Results: len(resp.Results),
	}
	evs := make([]events.Event, len(resp.Results))
	for i, r := range resp.Results {
		evs[i] = events.Event{
			EventID: uuid.NewString(), Type: events.TypeImpression, TS: now, RequestID: resp.RequestID,
			ProductID: r.ProductID, Position: r.Position, UserID: u.ID, SessionID: u.SessionID, Sponsored: r.Sponsored,
		}
	}
	s.Events.Emit(tid, req, evs)
}
