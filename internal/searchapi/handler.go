// Package searchapi is the search-api HTTP layer: request validation, per-tenant rate limits,
// the pipeline run, and paging over cached result snapshots (PLAN.md R4).
package searchapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-redis/redis_rate/v10"
	"github.com/oklog/ulid/v2"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/stages/queryunderstanding"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Request limits.
const (
	MaxBodyBytes    = 64 << 10
	MaxQueryLength  = 512
	DefaultPageSize = 24
	MaxPageSize     = 100
)

// Resolver returns a tenant's resolved config: the active one, or a pinned version or the
// draft for playground requests.
type Resolver interface {
	Get(ctx context.Context, id tenant.ID) (*tenant.Resolved, error)
	Pinned(ctx context.Context, id tenant.ID, ref string) (*tenant.Resolved, error)
}

// HeaderConfigVersion pins a playground request to a config version or "draft"; it is honoured
// only for tokens with search:playground (minted by control-api's playground proxy).
const HeaderConfigVersion = "X-Config-Version"

// ProviderSource returns a tenant's providers for its config version.
type ProviderSource interface {
	For(ctx context.Context, res *tenant.Resolved) (*pipeline.Providers, error)
}

// Runner executes the pipeline.
type Runner interface {
	Run(ctx context.Context, res *tenant.Resolved, p *pipeline.Providers, q *pipeline.Query) (*pipeline.State, *pipeline.Trace)
}

// RateLimiter admits requests (or n events) per tenant (GCRA).
type RateLimiter interface {
	AllowN(ctx context.Context, key string, limit redis_rate.Limit, n int) (*redis_rate.Result, error)
}

// Service serves the data-plane search endpoints.
type Service struct {
	Resolver  Resolver
	Providers ProviderSource
	Engine    Runner
	Snapshots *Snapshots
	Cursors   *Cursors
	Limiter   RateLimiter
	// Shadow runs sampled requests on the tenant's shadow version (nil disables).
	Shadow *Shadow
	// Events ingests POST /v1/events and receives impressions of served results (nil disables).
	Events EventSink
	// Vendors stores PUT /v1/vendors updates (nil disables).
	Vendors VendorStore
	Log     *slog.Logger
}

// Routes registers the search routes behind bearer auth.
func (s *Service) Routes(mux *http.ServeMux, v *auth.Verifier) {
	read := auth.Middleware(v, auth.ScopeSearchRead)
	mux.Handle("POST /v1/search", read(http.HandlerFunc(s.handleSearch)))
	mux.Handle("POST /v1/search/image", read(http.HandlerFunc(s.handleImageSearch)))
	mux.Handle("GET /v1/products/{product_id}/similar", read(http.HandlerFunc(s.handleSimilar)))
	mux.Handle("POST /v1/events", auth.Middleware(v, auth.ScopeEventsWrite)(http.HandlerFunc(s.handleEvents)))
	mux.Handle("PUT /v1/vendors", auth.Middleware(v, auth.ScopeCatalogWrite)(http.HandlerFunc(s.handleVendors)))
}

// SearchRequest is the body of POST /v1/search.
type SearchRequest struct {
	Q       string               `json:"q"`
	Filters *filter.Expr         `json:"filters,omitempty"`
	User    pipeline.UserContext `json:"user"`
	Page    PageRequest          `json:"page"`
	Explain bool                 `json:"explain"`
}

// PageRequest selects one paging mode: cursor, number or offset.
type PageRequest struct {
	Size   int     `json:"size"`
	Cursor *string `json:"cursor"`
	Number *int    `json:"number"`
	Offset *int    `json:"offset"`
}

// SearchResponse is the body of a successful search.
type SearchResponse struct {
	RequestID     string                           `json:"request_id"`
	ConfigVersion int                              `json:"config_version"`
	Degraded      bool                             `json:"degraded"`
	Results       []Result                         `json:"results"`
	Facets        map[string][]pipeline.FacetValue `json:"facets,omitempty"`
	Offset        int                              `json:"offset"`
	Total         int                              `json:"total"`
	TotalPages    int                              `json:"total_pages"`
	TotalMatches  int64                            `json:"total_matches"`
	Query         QueryInfo                        `json:"query"`
	NextCursor    *string                          `json:"next_cursor"`
	Trace         *pipeline.Trace                  `json:"trace"`
}

// Result is one ranked product.
type Result struct {
	ProductID string          `json:"product_id"`
	Score     float64         `json:"score"`
	Sponsored bool            `json:"sponsored"`
	Position  int             `json:"position"`
	Product   *ProductSummary `json:"product,omitempty"`
}

// ProductSummary is the display data returned with a result.
type ProductSummary struct {
	Title      string `json:"title"`
	Brand      string `json:"brand,omitempty"`
	PriceMinor int64  `json:"price_minor"`
	Currency   string `json:"currency"`
	ImageURL   string `json:"image_url,omitempty"`
	VendorID   string `json:"vendor_id"`
}

func badRequest(w http.ResponseWriter, errs ...platform.FieldError) {
	platform.WriteProblem(w, platform.Problem{Status: http.StatusBadRequest, Detail: "the request is invalid", Errors: errs})
}

func decodeStrict(r *http.Request, dst any) error { return decodeStrictN(r, dst, MaxBodyBytes) }

// decodeStrictN decodes a JSON body of at most limit bytes, rejecting unknown fields.
func decodeStrictN(r *http.Request, dst any, limit int) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(limit)+1))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if len(body) > limit {
		return fmt.Errorf("body exceeds %d bytes", limit)
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

func validate(req *SearchRequest) []platform.FieldError {
	var errs []platform.FieldError
	q := strings.TrimSpace(req.Q)
	switch {
	case q == "":
		errs = append(errs, platform.FieldError{Path: "/q", Code: "required", Message: "q must not be empty"})
	case utf8.RuneCountInString(req.Q) > MaxQueryLength:
		errs = append(errs, platform.FieldError{Path: "/q", Code: "too_long", Message: fmt.Sprintf("q is at most %d characters", MaxQueryLength)})
	}
	return append(errs, validatePage(&req.Page)...)
}

func validatePage(pg *PageRequest) []platform.FieldError {
	var errs []platform.FieldError
	if pg.Size == 0 {
		pg.Size = DefaultPageSize
	}
	if pg.Size < 1 || pg.Size > MaxPageSize {
		errs = append(errs, platform.FieldError{Path: "/page/size", Code: "out_of_range", Message: fmt.Sprintf("size must be within [1, %d]", MaxPageSize)})
	}
	modes := 0
	if pg.Cursor != nil && *pg.Cursor != "" {
		modes++
	}
	if pg.Number != nil {
		modes++
		if *pg.Number < 1 {
			errs = append(errs, platform.FieldError{Path: "/page/number", Code: "out_of_range", Message: "number starts at 1"})
		}
	}
	if pg.Offset != nil {
		modes++
		if *pg.Offset < 0 {
			errs = append(errs, platform.FieldError{Path: "/page/offset", Code: "out_of_range", Message: "offset must be >= 0"})
		}
	}
	if modes > 1 {
		errs = append(errs, platform.FieldError{Path: "/page", Code: "conflict", Message: "set at most one of cursor, number or offset"})
	}
	return errs
}

func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _ := auth.FromContext(ctx)
	var req SearchRequest
	if err := decodeStrict(r, &req); err != nil {
		badRequest(w, platform.FieldError{Path: "", Code: "invalid_json", Message: err.Error()})
		return
	}
	if errs := validate(&req); len(errs) > 0 {
		badRequest(w, errs...)
		return
	}
	res, ok := s.resolve(ctx, w, r, p)
	if !ok {
		return
	}
	reqFilter, ok := compileFilter(w, req.Filters, res)
	if !ok {
		return
	}
	q := &pipeline.Query{
		Tenant: p.Tenant, ConfigVersion: res.ConfigVersion, Type: pipeline.QueryText, Raw: req.Q,
		Filter: reqFilter, BaseFilter: res.AlwaysFilters, User: req.User, Facets: facetRequests(res.Config),
	}
	resp, ok := s.servePaged(ctx, w, p, res, q, req.Page, req.Explain,
		pipeline.QueryText, queryunderstanding.Normalize(req.Q), reqFilter, req.User.ID)
	if ok {
		platform.WriteJSON(w, http.StatusOK, resp)
	}
}

// resolve loads the caller's tenant config (the active one, or the pinned playground version)
// and applies its rate limit, writing the error response when either fails.
func (s *Service) resolve(ctx context.Context, w http.ResponseWriter, r *http.Request, p auth.Principal) (*tenant.Resolved, bool) {
	var res *tenant.Resolved
	var err error
	if ref := r.Header.Get(HeaderConfigVersion); ref != "" {
		if !p.Has(auth.ScopeSearchPlayground) {
			platform.WriteProblem(w, platform.Problem{Status: http.StatusForbidden, Detail: HeaderConfigVersion + " is for the playground only"})
			return nil, false
		}
		res, err = s.Resolver.Pinned(ctx, p.Tenant, ref)
		if errors.Is(err, tenant.ErrNotFound) {
			platform.WriteProblem(w, platform.Problem{Status: http.StatusNotFound, Detail: "no such config version"})
			return nil, false
		}
	} else {
		res, err = s.Resolver.Get(ctx, p.Tenant)
	}
	if !s.resolved(w, p, err) {
		return nil, false
	}
	return res, s.admit(ctx, w, res, "search", res.Plan.SearchRPS, res.Plan.SearchBurst, 1)
}

// resolved writes the problem for a failed tenant resolution.
func (s *Service) resolved(w http.ResponseWriter, p auth.Principal, err error) bool {
	if errors.Is(err, tenant.ErrNotFound) {
		platform.WriteProblem(w, platform.Problem{Status: http.StatusForbidden, Detail: "the tenant is unknown or inactive"})
		return false
	}
	if err != nil {
		s.Log.Error("resolve tenant", "tenant", p.Tenant, "err", err)
		platform.WriteProblem(w, platform.Problem{Status: http.StatusServiceUnavailable, Detail: "tenant configuration is unavailable"})
		return false
	}
	return true
}

func compileFilter(w http.ResponseWriter, f *filter.Expr, res *tenant.Resolved) (filter.Expr, bool) {
	if f == nil {
		return filter.Expr{}, true
	}
	c, errs := filter.Compile(*f, res.Registry, "/filters")
	if len(errs) > 0 {
		badRequest(w, errs...)
		return filter.Expr{}, false
	}
	return c, true
}

// servePaged returns one page of q's results from its snapshot, running the pipeline when the
// snapshot is missing. keyParts identify the query (everything that changes the order).
func (s *Service) servePaged(ctx context.Context, w http.ResponseWriter, p auth.Principal, res *tenant.Resolved,
	q *pipeline.Query, page PageRequest, explainRequested bool, keyParts ...any) (*SearchResponse, bool) {
	playground := p.Has(auth.ScopeSearchPlayground)
	explain := playground || (explainRequested && p.Has(auth.ScopeSearchDebug))
	tid := string(p.Tenant)
	requestID := ulid.Make().String()
	key, err := SnapshotKey(append([]any{res.ConfigVersion}, keyParts...)...)
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	hasCursor := page.Cursor != nil && *page.Cursor != ""
	offset := 0
	switch {
	case hasCursor:
		cur, err := s.Cursors.Decode(tid, *page.Cursor)
		if err != nil || cur.Snapshot != key || cur.ConfigVersion != res.ConfigVersion {
			badRequest(w, platform.FieldError{Path: "/page/cursor", Code: "invalid_cursor", Message: "the cursor is invalid or belongs to a different query"})
			return nil, false
		}
		offset = cur.Offset
	case page.Number != nil:
		offset = (*page.Number - 1) * page.Size
	case page.Offset != nil:
		offset = *page.Offset
	}

	var snap *Snapshot
	var tr *pipeline.Trace
	if !explain {
		if snap, _, err = s.Snapshots.Get(ctx, tid, key); err != nil {
			s.Log.Warn("snapshot read failed", "tenant", tid, "err", err)
		}
	}
	if snap == nil && hasCursor && !explain {
		badRequest(w, platform.FieldError{Path: "/page/cursor", Code: "invalid_cursor", Message: "the cursor has expired; search again"})
		return nil, false
	}
	if snap == nil {
		providers, ok := s.providers(ctx, w, res)
		if !ok {
			return nil, false
		}
		q.Depth, q.Explain = max(res.Plan.MaxPageDepth, page.Size), explain
		shadowQ := *q
		start := time.Now()
		var st *pipeline.State
		st, tr = s.Engine.Run(ctx, res, providers, q)
		snap = snapshotOf(res.ConfigVersion, st)
		if !playground {
			if err := s.Snapshots.Put(ctx, tid, key, snap); err != nil {
				s.Log.Warn("snapshot write failed", "tenant", tid, "err", err)
			}
			s.Shadow.Maybe(res, shadowQ, st.Candidates, time.Since(start), requestID) //nolint:contextcheck // shadow runs detached from the request by design
		}
	}
	resp, err := s.page(ctx, res, snap, key, offset, page.Size)
	if err != nil {
		s.fail(w, err)
		return nil, false
	}
	resp.RequestID = requestID
	if playground {
		resp.NextCursor = nil
	}
	if explain {
		resp.Trace = tr
	}
	if !playground {
		s.emitServed(p.Tenant, resp, q)
	}
	return resp, true
}

func (s *Service) providers(ctx context.Context, w http.ResponseWriter, res *tenant.Resolved) (*pipeline.Providers, bool) {
	providers, err := s.Providers.For(ctx, res)
	if err != nil {
		s.Log.Error("build providers", "tenant", res.Tenant.ID, "err", err)
		platform.WriteProblem(w, platform.Problem{Status: http.StatusServiceUnavailable, Detail: "search providers are unavailable"})
		return nil, false
	}
	return providers, true
}

// admit applies the plan's GCRA limit for kind: n searches, events or vendor updates. A limiter
// outage fails open: availability beats strict limits.
func (s *Service) admit(ctx context.Context, w http.ResponseWriter, res *tenant.Resolved, kind string, rps, burst, n int) bool {
	if s.Limiter == nil || rps <= 0 {
		return true
	}
	lim := redis_rate.Limit{Rate: rps, Burst: max(burst, n, 1), Period: time.Second}
	r, err := s.Limiter.AllowN(ctx, "rl:"+kind+":{"+string(res.Tenant.ID)+"}", lim, n)
	if err != nil {
		s.Log.Warn("rate limiter unavailable; admitting", "tenant", res.Tenant.ID, "err", err)
		return true
	}
	if r.Allowed > 0 {
		return true
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(r.RetryAfter.Seconds()))))
	platform.WriteProblem(w, platform.Problem{Status: http.StatusTooManyRequests, Detail: fmt.Sprintf("the %s plan allows %d %s per second", res.Plan.ID, rps, kind)})
	return false
}

func (s *Service) fail(w http.ResponseWriter, err error) {
	s.Log.Error("search failed", "err", err)
	platform.WriteProblem(w, platform.Problem{Status: http.StatusInternalServerError})
}

func facetRequests(cfg *tenant.Config) []pipeline.FacetRequest {
	fs := cfg.EffectiveFacets()
	out := make([]pipeline.FacetRequest, len(fs))
	for i, f := range fs {
		fr := pipeline.FacetRequest{Field: f.Field, Size: f.Size}
		for _, r := range f.Ranges {
			fr.Ranges = append(fr.Ranges, pipeline.FacetRange{From: r.From, To: r.To})
		}
		out[i] = fr
	}
	return out
}

func snapshotOf(version int, st *pipeline.State) *Snapshot {
	snap := &Snapshot{
		ConfigVersion: version, Facets: st.Facets, TotalMatches: st.TotalMatches, Degraded: st.Degraded,
		Items: make([]SnapshotItem, len(st.Candidates)),
		Query: QueryInfo{Normalized: st.Query.Text, Attributes: st.Query.Attributes, Categories: st.Query.Categories},
	}
	for i, c := range st.Candidates {
		snap.Items[i] = SnapshotItem{ID: c.ProductID, Score: math.Round(c.Score*1e6) / 1e6, Sponsored: c.Sponsored}
	}
	return snap
}

// page cuts one page from the snapshot and hydrates its display fields from the lexical index
// (retrieval only loads ranking fields).
func (s *Service) page(ctx context.Context, res *tenant.Resolved, snap *Snapshot, key string, offset, size int) (*SearchResponse, error) {
	total := len(snap.Items)
	resp := &SearchResponse{
		RequestID: ulid.Make().String(), ConfigVersion: snap.ConfigVersion, Degraded: snap.Degraded,
		Results: []Result{}, Facets: snap.Facets, Offset: offset, Total: total,
		TotalPages: (total + size - 1) / size, TotalMatches: snap.TotalMatches, Query: snap.Query,
	}
	if offset >= total {
		return resp, nil
	}
	items := snap.Items[offset:min(offset+size, total)]
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	providers, err := s.Providers.For(ctx, res)
	if err != nil {
		return nil, err
	}
	docs, err := providers.Lexical.Docs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i, it := range items {
		r := Result{ProductID: it.ID, Score: it.Score, Sponsored: it.Sponsored, Position: offset + i + 1}
		if d := docs[it.ID]; d != nil {
			r.Product = &ProductSummary{Title: d.Title, Brand: d.Brand, PriceMinor: d.PriceMinor, Currency: d.Currency, ImageURL: d.ImageURL, VendorID: d.VendorID}
		}
		resp.Results = append(resp.Results, r)
	}
	if next := offset + len(items); next < total {
		c := s.Cursors.Encode(string(res.Tenant.ID), Cursor{Snapshot: key, Offset: next, ConfigVersion: snap.ConfigVersion})
		resp.NextCursor = &c
	}
	return resp, nil
}
