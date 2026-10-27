package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// ChannelSettingsChanged is published when a platform settings section changes.
const ChannelSettingsChanged = "platform.settings.changed"

// Duration marshals as a Go duration string ("15m", "168h").
type Duration time.Duration

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(time.Duration(d).String())
	if err != nil {
		return nil, fmt.Errorf("duration: %w", err)
	}
	return b, nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	*d = Duration(v)
	return nil
}

// AuthSettings are the admin-controlled auth policies (PLAN.md R7).
type AuthSettings struct {
	Password             auth.PasswordParams `json:"password"`
	PasswordMinLength    int                 `json:"password_min_length"`
	AccessTokenTTL       Duration            `json:"access_token_ttl"`
	RefreshTokenTTL      Duration            `json:"refresh_token_ttl"`
	APITokenTTL          Duration            `json:"api_token_ttl"`
	KeyRotation          Duration            `json:"key_rotation"`
	KeyRetention         Duration            `json:"key_retention"`
	EmailVerificationTTL Duration            `json:"email_verification_ttl"`
}

// AdsSettings are the admin-controlled ad policies (PLAN.md R10).
type AdsSettings struct {
	TenantFields      []string            `json:"tenant_fields"`
	Disclosure        SponsoredDisclosure `json:"disclosure"`
	MaxSlots          int                 `json:"max_slots"`
	MinRelevanceFloor float64             `json:"min_relevance_floor"`
	ReserveBounds     ReserveBounds       `json:"reserve_bounds"`
	PerPlan           map[string]any      `json:"per_plan,omitempty"`
}

// SponsoredDisclosure defines disclosure requirements for sponsored results.
type SponsoredDisclosure struct {
	Label          string `json:"label"`
	VendorName     bool   `json:"vendor_name"`
	DisclosureLink string `json:"disclosure_link"`
}

// ReserveBounds bounds acceptable reserve CPC prices.
type ReserveBounds struct {
	MinMinor int64 `json:"min_minor"`
	MaxMinor int64 `json:"max_minor"`
}

// PlatformSettings holds every section.
type PlatformSettings struct {
	Auth AuthSettings `json:"auth"`
	Ads  AdsSettings  `json:"ads"`
}

// DefaultAdsSettings returns R10 defaults for ad policies.
func DefaultAdsSettings() AdsSettings {
	return AdsSettings{
		TenantFields: []string{"bid_minor", "daily_budget_minor", "status", "targeting", "start_at", "end_at"},
		Disclosure: SponsoredDisclosure{
			Label:          "Sponsored",
			VendorName:     true,
			DisclosureLink: "https://example.com/ads-info",
		},
		MaxSlots:          10,
		MinRelevanceFloor: 0.10,
		ReserveBounds: ReserveBounds{
			MinMinor: 1,
			MaxMinor: 10000,
		},
	}
}

// DefaultSettings are the R7 & R10 defaults.
func DefaultSettings() PlatformSettings {
	return PlatformSettings{
		Auth: AuthSettings{
			Password:             auth.DefaultPasswordParams(),
			PasswordMinLength:    12,
			AccessTokenTTL:       Duration(15 * time.Minute),
			RefreshTokenTTL:      Duration(7 * 24 * time.Hour),
			APITokenTTL:          Duration(15 * time.Minute),
			KeyRotation:          Duration(30 * 24 * time.Hour),
			KeyRetention:         Duration(7 * 24 * time.Hour),
			EmailVerificationTTL: Duration(24 * time.Hour),
		},
		Ads: DefaultAdsSettings(),
	}
}

// Validate checks every setting against its bounds.
func (s PlatformSettings) Validate() []platform.FieldError {
	var errs []platform.FieldError
	day := 24 * time.Hour
	durs := []struct {
		path     string
		v        Duration
		min, max time.Duration
	}{
		{"/auth/access_token_ttl", s.Auth.AccessTokenTTL, 5 * time.Minute, time.Hour},
		{"/auth/refresh_token_ttl", s.Auth.RefreshTokenTTL, day, 30 * day},
		{"/auth/api_token_ttl", s.Auth.APITokenTTL, 5 * time.Minute, time.Hour},
		{"/auth/key_rotation", s.Auth.KeyRotation, 7 * day, 90 * day},
		{"/auth/key_retention", s.Auth.KeyRetention, day, 30 * day},
		{"/auth/email_verification_ttl", s.Auth.EmailVerificationTTL, time.Hour, 7 * day},
	}
	for _, d := range durs {
		if time.Duration(d.v) < d.min || time.Duration(d.v) > d.max {
			errs = append(errs, platform.FieldError{Path: d.path, Code: "out_of_range", Message: fmt.Sprintf("must be within [%s, %s]", d.min, d.max)})
		}
	}
	if time.Duration(s.Auth.KeyRetention) < max(time.Duration(s.Auth.AccessTokenTTL), time.Duration(s.Auth.APITokenTTL)) {
		errs = append(errs, platform.FieldError{Path: "/auth/key_retention", Code: "too_short", Message: "must outlive the longest token lifetime"})
	}
	p := s.Auth.Password
	if p.MemoryKiB < 19456 || p.MemoryKiB > 262144 {
		errs = append(errs, platform.FieldError{Path: "/auth/password/memory_kib", Code: "out_of_range", Message: "must be within [19456, 262144] (19–256 MiB)"})
	}
	if p.Time < 1 || p.Time > 10 {
		errs = append(errs, platform.FieldError{Path: "/auth/password/time", Code: "out_of_range", Message: "must be within [1, 10]"})
	}
	if p.Threads < 1 || p.Threads > 8 {
		errs = append(errs, platform.FieldError{Path: "/auth/password/threads", Code: "out_of_range", Message: "must be within [1, 8]"})
	}
	if s.Auth.PasswordMinLength < 8 || s.Auth.PasswordMinLength > 64 {
		errs = append(errs, platform.FieldError{Path: "/auth/password_min_length", Code: "out_of_range", Message: "must be within [8, 64]"})
	}
	if s.Ads.MaxSlots < 1 || s.Ads.MaxSlots > 50 {
		errs = append(errs, platform.FieldError{Path: "/ads/max_slots", Code: "out_of_range", Message: "must be within [1, 50]"})
	}
	if s.Ads.MinRelevanceFloor < 0 || s.Ads.MinRelevanceFloor > 1 {
		errs = append(errs, platform.FieldError{Path: "/ads/min_relevance_floor", Code: "out_of_range", Message: "must be within [0, 1]"})
	}
	if s.Ads.ReserveBounds.MinMinor < 0 {
		errs = append(errs, platform.FieldError{Path: "/ads/reserve_bounds/min_minor", Code: "out_of_range", Message: "must be >= 0"})
	}
	if s.Ads.ReserveBounds.MaxMinor < s.Ads.ReserveBounds.MinMinor {
		errs = append(errs, platform.FieldError{Path: "/ads/reserve_bounds/max_minor", Code: "out_of_range", Message: "must be >= min_minor"})
	}
	if s.Ads.Disclosure.Label == "" {
		errs = append(errs, platform.FieldError{Path: "/ads/disclosure/label", Code: "required", Message: "label is required"})
	}
	if len(s.Ads.TenantFields) == 0 {
		errs = append(errs, platform.FieldError{Path: "/ads/tenant_fields", Code: "required", Message: "at least one tenant field is required"})
	}
	return errs
}

// Settings caches the platform settings and reloads them when another replica publishes a
// change.
type Settings struct {
	q   *tenantdb.Queries
	rdb *redis.Client
	log *slog.Logger

	mu  sync.RWMutex
	cur PlatformSettings
}

// NewSettings returns a settings service; call Load before use.
func NewSettings(q *tenantdb.Queries, rdb *redis.Client, log *slog.Logger) *Settings {
	return &Settings{q: q, rdb: rdb, log: log, cur: DefaultSettings()}
}

// Get returns the current settings.
func (s *Settings) Get() PlatformSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Load reads the stored sections over the defaults.
func (s *Settings) Load(ctx context.Context) error {
	rows, err := s.q.ListPlatformSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	cur := DefaultSettings()
	for _, r := range rows {
		switch r.Section {
		case "auth":
			if err := json.Unmarshal(r.Document, &cur.Auth); err != nil {
				return fmt.Errorf("settings section %s: %w", r.Section, err)
			}
		case "ads":
			if err := json.Unmarshal(r.Document, &cur.Ads); err != nil {
				return fmt.Errorf("settings section %s: %w", r.Section, err)
			}
		}
	}
	s.mu.Lock()
	s.cur = cur
	s.mu.Unlock()
	return nil
}

// Update validates and stores new settings, then tells the other replicas.
func (s *Settings) Update(ctx context.Context, next PlatformSettings, by string) []platform.FieldError {
	if errs := next.Validate(); len(errs) > 0 {
		return errs
	}
	authDoc, err := json.Marshal(next.Auth)
	if err == nil {
		err = s.q.UpsertPlatformSetting(ctx, tenantdb.UpsertPlatformSettingParams{Section: "auth", Document: authDoc, UpdatedBy: nullString(by)})
	}
	if err != nil {
		return []platform.FieldError{{Path: "", Code: "storage", Message: err.Error()}}
	}
	adsDoc, err := json.Marshal(next.Ads)
	if err == nil {
		err = s.q.UpsertPlatformSetting(ctx, tenantdb.UpsertPlatformSettingParams{Section: "ads", Document: adsDoc, UpdatedBy: nullString(by)})
	}
	if err != nil {
		return []platform.FieldError{{Path: "", Code: "storage", Message: err.Error()}}
	}
	s.mu.Lock()
	s.cur = next
	s.mu.Unlock()
	if err := s.rdb.Publish(ctx, ChannelSettingsChanged, "all").Err(); err != nil {
		s.log.Warn("publish settings change", "err", err)
	}
	return nil
}

// Watch reloads on change notifications until ctx ends.
func (s *Settings) Watch(ctx context.Context) {
	sub := s.rdb.Subscribe(ctx, ChannelSettingsChanged)
	defer sub.Close()
	ch := sub.Channel()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if err := s.Load(ctx); err != nil {
				s.log.Warn("reload settings", "err", err)
			}
		}
	}
}
