package controlplane

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/oklog/ulid/v2"

	"github.com/zavisoft/search-engine/internal/ads"
	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// mysqlDuplicateEntry is MySQL's ER_DUP_ENTRY.
const mysqlDuplicateEntry = 1062

func (s *Server) adsRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/tenants/{tid}/ads/campaigns", s.tenantRoute(auth.ScopeTenantRead, s.handleListCampaigns))
	mux.Handle("POST /v1/tenants/{tid}/ads/campaigns", s.tenantRoute(auth.ScopeTenantWrite, s.tenantIdem(s.handleCreateCampaign).ServeHTTP))
	mux.Handle("GET /v1/tenants/{tid}/ads/campaigns/{campaign_id}", s.tenantRoute(auth.ScopeTenantRead, s.handleGetCampaign))
	mux.Handle("PUT /v1/tenants/{tid}/ads/campaigns/{campaign_id}", s.tenantRoute(auth.ScopeTenantWrite, s.handleUpdateCampaign))
	mux.Handle("DELETE /v1/tenants/{tid}/ads/campaigns/{campaign_id}", s.tenantRoute(auth.ScopeTenantWrite, s.handleDeleteCampaign))

	mux.Handle("GET /v1/tenants/{tid}/ads/campaigns/{campaign_id}/creatives", s.tenantRoute(auth.ScopeTenantRead, s.handleListCreatives))
	mux.Handle("POST /v1/tenants/{tid}/ads/campaigns/{campaign_id}/creatives", s.tenantRoute(auth.ScopeTenantWrite, s.tenantIdem(s.handleCreateCreative).ServeHTTP))
	mux.Handle("DELETE /v1/tenants/{tid}/ads/campaigns/{campaign_id}/creatives/{creative_id}", s.tenantRoute(auth.ScopeTenantWrite, s.handleDeleteCreative))

	mux.Handle("GET /v1/tenants/{tid}/ads/reports", s.tenantRoute(auth.ScopeTenantRead, s.handleAdsReport))
}

type campaignInput struct {
	VendorID         string          `json:"vendor_id"`
	Status           string          `json:"status"`
	DailyBudgetMinor int64           `json:"daily_budget_minor"`
	Currency         string          `json:"currency"`
	BidMinor         int64           `json:"bid_minor"`
	Targeting        json.RawMessage `json:"targeting"`
	StartAt          string          `json:"start_at"`
	EndAt            *string         `json:"end_at"`
}

type campaignView struct {
	ID               string          `json:"id"`
	VendorID         string          `json:"vendor_id"`
	Status           string          `json:"status"`
	DailyBudgetMinor int64           `json:"daily_budget_minor"`
	Currency         string          `json:"currency"`
	BidMinor         int64           `json:"bid_minor"`
	Targeting        json.RawMessage `json:"targeting"`
	StartAt          string          `json:"start_at"`
	EndAt            *string         `json:"end_at"`
}

type creativeInput struct {
	ProductID string `json:"product_id"`
}

type creativeView struct {
	ID         string `json:"id"`
	CampaignID string `json:"campaign_id"`
	ProductID  string `json:"product_id"`
}

func (s *Server) checkCampaign(in campaignInput) (time.Time, sql.NullTime, []platform.FieldError) {
	var errs []platform.FieldError
	if strings.TrimSpace(in.VendorID) == "" {
		errs = append(errs, platform.FieldError{Path: "/vendor_id", Code: "required", Message: "vendor_id is required"})
	}
	if in.DailyBudgetMinor <= 0 {
		errs = append(errs, platform.FieldError{Path: "/daily_budget_minor", Code: "out_of_range", Message: "daily_budget_minor must be > 0"})
	}
	if len(in.Currency) != 3 {
		errs = append(errs, platform.FieldError{Path: "/currency", Code: "invalid", Message: "currency must be a 3-letter code"})
	}
	if in.BidMinor <= 0 {
		errs = append(errs, platform.FieldError{Path: "/bid_minor", Code: "out_of_range", Message: "bid_minor must be > 0"})
	}

	// A bid below the platform's lowest reserve price could never clear.
	if s.Settings != nil {
		if lo := s.Settings.Get().Ads.ReserveBounds.MinMinor; in.BidMinor < lo {
			errs = append(errs, platform.FieldError{Path: "/bid_minor", Code: "out_of_range", Message: fmt.Sprintf("bid_minor must be at least the platform's minimum reserve price %d", lo)})
		}
	}
	if len(in.Targeting) > 0 && string(in.Targeting) != "null" {
		dec := json.NewDecoder(bytes.NewReader(in.Targeting))
		dec.DisallowUnknownFields()
		var t ads.Targeting
		if err := dec.Decode(&t); err != nil {
			errs = append(errs, platform.FieldError{Path: "/targeting", Code: "invalid", Message: "targeting takes categories, keywords and queries (arrays of strings)"})
		}
	}

	if in.Status != "" && in.Status != ads.StatusActive && in.Status != ads.StatusPaused && in.Status != ads.StatusDraft && in.Status != ads.StatusCompleted {
		errs = append(errs, platform.FieldError{Path: "/status", Code: "invalid", Message: "status must be active, paused, draft, or completed"})
	}

	startAt, err := time.Parse(time.RFC3339, in.StartAt)
	if err != nil {
		errs = append(errs, platform.FieldError{Path: "/start_at", Code: "invalid", Message: "start_at must be an RFC 3339 timestamp"})
	}

	var endAt sql.NullTime
	if in.EndAt != nil && *in.EndAt != "" {
		t, err := time.Parse(time.RFC3339, *in.EndAt)
		if err != nil {
			errs = append(errs, platform.FieldError{Path: "/end_at", Code: "invalid", Message: "end_at must be an RFC 3339 timestamp"})
		} else {
			if !startAt.IsZero() && t.Before(startAt) {
				errs = append(errs, platform.FieldError{Path: "/end_at", Code: "invalid", Message: "end_at must be after start_at"})
			}
			endAt = sql.NullTime{Time: t, Valid: true}
		}
	}

	return startAt, endAt, errs
}

func campaignRowToView(r tenantdb.AdCampaign) campaignView {
	targeting := r.Targeting
	if len(targeting) == 0 {
		targeting = json.RawMessage("{}")
	}
	var endAt *string
	if r.EndAt.Valid {
		s := r.EndAt.Time.UTC().Format(time.RFC3339)
		endAt = &s
	}
	return campaignView{
		ID:               r.ID,
		VendorID:         r.VendorID,
		Status:           r.Status,
		DailyBudgetMinor: r.DailyBudgetMinor,
		Currency:         r.Currency,
		BidMinor:         r.BidMinor,
		Targeting:        targeting,
		StartAt:          r.StartAt.UTC().Format(time.RFC3339),
		EndAt:            endAt,
	}
}

func (s *Server) handleListCampaigns(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	limit := int32(50)
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.ParseInt(l, 10, 32); err == nil && n > 0 && n <= 100 {
			limit = int32(n)
		}
	}

	cursor := r.URL.Query().Get("cursor")
	var rows []tenantdb.AdCampaign
	var err error
	if cursor != "" {
		rows, err = s.Q.ListAdCampaignsAfter(r.Context(), tenantdb.ListAdCampaignsAfterParams{
			TenantID: string(tid),
			ID:       cursor,
			Limit:    limit + 1,
		})
	} else {
		rows, err = s.Q.ListAdCampaigns(r.Context(), tenantdb.ListAdCampaignsParams{
			TenantID: string(tid),
			Limit:    limit + 1,
		})
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	var nextCursor *string
	if len(rows) > int(limit) {
		next := rows[limit].ID
		nextCursor = &next
		rows = rows[:limit]
	}

	items := make([]campaignView, 0, len(rows))
	for _, row := range rows {
		items = append(items, campaignRowToView(row))
	}

	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

func (s *Server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	var in campaignInput
	if !decodeOrFail(w, r, &in) {
		return
	}

	startAt, endAt, errs := s.checkCampaign(in)
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}

	status := in.Status
	if status == "" {
		status = ads.StatusActive
	}

	targeting := in.Targeting
	if len(targeting) == 0 {
		targeting = json.RawMessage("{}")
	}

	id := ulid.Make().String()
	err := s.Q.CreateAdCampaign(r.Context(), tenantdb.CreateAdCampaignParams{
		ID:               id,
		TenantID:         string(tid),
		VendorID:         in.VendorID,
		Status:           status,
		DailyBudgetMinor: in.DailyBudgetMinor,
		Currency:         strings.ToUpper(in.Currency),
		BidMinor:         in.BidMinor,
		Targeting:        targeting,
		StartAt:          startAt,
		EndAt:            endAt,
	})
	if err != nil {
		s.internalError(w, err)
		return
	}

	row, err := s.Q.GetAdCampaign(r.Context(), tenantdb.GetAdCampaignParams{
		TenantID: string(tid),
		ID:       id,
	})
	if err != nil {
		s.internalError(w, err)
		return
	}

	platform.WriteJSON(w, http.StatusCreated, campaignRowToView(row))
}

func (s *Server) handleGetCampaign(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	id := r.PathValue("campaign_id")
	row, err := s.Q.GetAdCampaign(r.Context(), tenantdb.GetAdCampaignParams{
		TenantID: string(tid),
		ID:       id,
	})
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	platform.WriteJSON(w, http.StatusOK, campaignRowToView(row))
}

func (s *Server) handleUpdateCampaign(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	id := r.PathValue("campaign_id")

	// Ensure campaign exists for this tenant
	_, err := s.Q.GetAdCampaign(r.Context(), tenantdb.GetAdCampaignParams{
		TenantID: string(tid),
		ID:       id,
	})
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	var in campaignInput
	if !decodeOrFail(w, r, &in) {
		return
	}

	startAt, endAt, errs := s.checkCampaign(in)
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}

	status := in.Status
	if status == "" {
		status = ads.StatusActive
	}

	targeting := in.Targeting
	if len(targeting) == 0 {
		targeting = json.RawMessage("{}")
	}

	n, err := s.Q.UpdateAdCampaign(r.Context(), tenantdb.UpdateAdCampaignParams{
		VendorID:         in.VendorID,
		Status:           status,
		DailyBudgetMinor: in.DailyBudgetMinor,
		Currency:         strings.ToUpper(in.Currency),
		BidMinor:         in.BidMinor,
		Targeting:        targeting,
		StartAt:          startAt,
		EndAt:            endAt,
		TenantID:         string(tid),
		ID:               id,
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n == 0 {
		problem(w, http.StatusNotFound, "campaign not found")
		return
	}

	row, err := s.Q.GetAdCampaign(r.Context(), tenantdb.GetAdCampaignParams{
		TenantID: string(tid),
		ID:       id,
	})
	if err != nil {
		s.internalError(w, err)
		return
	}

	platform.WriteJSON(w, http.StatusOK, campaignRowToView(row))
}

func (s *Server) handleDeleteCampaign(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	id := r.PathValue("campaign_id")

	n, err := s.Q.DeleteAdCampaign(r.Context(), tenantdb.DeleteAdCampaignParams{
		TenantID: string(tid),
		ID:       id,
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n == 0 {
		problem(w, http.StatusNotFound, "campaign not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListCreatives(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	campID := r.PathValue("campaign_id")

	// Ensure campaign exists for this tenant
	_, err := s.Q.GetAdCampaign(r.Context(), tenantdb.GetAdCampaignParams{
		TenantID: string(tid),
		ID:       campID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	limit := int32(50)
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.ParseInt(l, 10, 32); err == nil && n > 0 && n <= 100 {
			limit = int32(n)
		}
	}

	cursor := r.URL.Query().Get("cursor")
	var rows []tenantdb.AdCreative
	if cursor != "" {
		rows, err = s.Q.ListAdCreativesAfter(r.Context(), tenantdb.ListAdCreativesAfterParams{
			TenantID:   string(tid),
			CampaignID: campID,
			ID:         cursor,
			Limit:      limit + 1,
		})
	} else {
		rows, err = s.Q.ListAdCreatives(r.Context(), tenantdb.ListAdCreativesParams{
			TenantID:   string(tid),
			CampaignID: campID,
			Limit:      limit + 1,
		})
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	var nextCursor *string
	if len(rows) > int(limit) {
		next := rows[limit].ID
		nextCursor = &next
		rows = rows[:limit]
	}

	items := make([]creativeView, 0, len(rows))
	for _, row := range rows {
		items = append(items, creativeView{
			ID:         row.ID,
			CampaignID: row.CampaignID,
			ProductID:  row.ProductID,
		})
	}

	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

func (s *Server) handleCreateCreative(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	campID := r.PathValue("campaign_id")

	_, err := s.Q.GetAdCampaign(r.Context(), tenantdb.GetAdCampaignParams{
		TenantID: string(tid),
		ID:       campID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, http.StatusNotFound, "campaign not found")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	var in creativeInput
	if !decodeOrFail(w, r, &in) {
		return
	}
	if strings.TrimSpace(in.ProductID) == "" {
		invalid(w, platform.FieldError{Path: "/product_id", Code: "required", Message: "product_id is required"})
		return
	}

	id := ulid.Make().String()
	err = s.Q.CreateAdCreative(r.Context(), tenantdb.CreateAdCreativeParams{
		ID:         id,
		CampaignID: campID,
		ProductID:  in.ProductID,
	})
	var me *mysql.MySQLError
	if errors.As(err, &me) && me.Number == mysqlDuplicateEntry {
		problem(w, http.StatusConflict, "this campaign already promotes the product", fieldErr("/product_id", "duplicate", "already a creative of this campaign"))
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}

	platform.WriteJSON(w, http.StatusCreated, creativeView{
		ID:         id,
		CampaignID: campID,
		ProductID:  in.ProductID,
	})
}

func (s *Server) handleDeleteCreative(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	campID := r.PathValue("campaign_id")
	creativeID := r.PathValue("creative_id")

	n, err := s.Q.DeleteAdCreative(r.Context(), tenantdb.DeleteAdCreativeParams{
		TenantID:   string(tid),
		CampaignID: campID,
		ID:         creativeID,
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n == 0 {
		problem(w, http.StatusNotFound, "creative not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAdsReport(w http.ResponseWriter, r *http.Request) {
	tid := pathTenant(r)
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	var errs []platform.FieldError
	from, err := time.Parse("2006-01-02", fromStr)
	if err != nil {
		errs = append(errs, platform.FieldError{Path: "/from", Code: "invalid", Message: "from must be YYYY-MM-DD"})
	}
	to, err := time.Parse("2006-01-02", toStr)
	if err != nil {
		errs = append(errs, platform.FieldError{Path: "/to", Code: "invalid", Message: "to must be YYYY-MM-DD"})
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}

	if to.Before(from) {
		invalid(w, platform.FieldError{Path: "/to", Code: "invalid", Message: "to must not be before from"})
		return
	}

	// Every campaign's creatives, paused and completed ones too, so their history stays.
	creatives, err := s.Q.ListCreativeCampaigns(r.Context(), string(tid))
	if err != nil {
		s.internalError(w, err)
		return
	}
	mapping := make(map[string]string, len(creatives))
	for _, c := range creatives {
		mapping[c.ProductID] = c.CampaignID
	}

	reports := ads.NewReports(s.ClickHouse)
	rows, err := reports.Generate(r.Context(), tid, from, to, mapping)
	if err != nil {
		s.internalError(w, err)
		return
	}

	platform.WriteJSON(w, http.StatusOK, map[string]any{
		"rows": rows,
	})
}
