package searchapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// ShadowTopK is how many results a shadow comparison stores per side.
const ShadowTopK = 10

// ShadowRow is one shadow comparison (ClickHouse shadow_results, PLAN.md R8).
type ShadowRow struct {
	TS              time.Time
	RequestID       string
	TenantID        string
	QueryType       string
	QueryHash       string
	ActiveVersion   uint32
	ShadowVersion   uint32
	ActiveTop       []string
	ShadowTop       []string
	OverlapAt10     float32
	ActiveLatencyMS float32
	ShadowLatencyMS float32
	ShadowDegraded  uint8
}

// ShadowSink receives shadow rows.
type ShadowSink interface {
	Write(row ShadowRow)
}

// Shadow runs sampled requests on a tenant's shadow version in the background. Results are
// logged, never returned, and a full worker pool drops the comparison instead of queueing.
type Shadow struct {
	Engine    Runner
	Providers ProviderSource
	Sink      ShadowSink
	Log       *slog.Logger
	sem       chan struct{}
	sample    func() float64
}

// NewShadow returns a shadow runner with at most workers concurrent shadow searches.
func NewShadow(engine Runner, providers ProviderSource, sink ShadowSink, workers int, log *slog.Logger) *Shadow {
	return &Shadow{Engine: engine, Providers: providers, Sink: sink, Log: log, sem: make(chan struct{}, max(workers, 1)), sample: rand.Float64}
}

func topIDs(cs []pipeline.Candidate, k int) []string {
	out := make([]string, 0, min(k, len(cs)))
	for _, c := range cs[:min(k, len(cs))] {
		out = append(out, c.ProductID)
	}
	return out
}

func overlap(a, b []string) float32 {
	if len(a) == 0 {
		return 0
	}
	set := make(map[string]bool, len(b))
	for _, x := range b {
		set[x] = true
	}
	n := 0
	for _, x := range a {
		if set[x] {
			n++
		}
	}
	return float32(n) / float32(len(a))
}

// Maybe samples the request and, when selected, runs q (a copy taken before the primary run)
// against res.Shadow.
func (s *Shadow) Maybe(res *tenant.Resolved, q pipeline.Query, primary []pipeline.Candidate, primaryLatency time.Duration, requestID string) {
	if s == nil || res.Shadow == nil || res.ShadowRate <= 0 || s.sample() >= res.ShadowRate {
		return
	}
	select {
	case s.sem <- struct{}{}:
	default:
		s.Log.Debug("shadow pool full; comparison dropped", "tenant", res.Tenant.ID)
		return
	}
	activeTop := topIDs(primary, ShadowTopK)
	go func() {
		defer func() { <-s.sem }()
		sh := res.Shadow
		budget := time.Duration(sh.Config.Budgets.TotalMS)*time.Millisecond + 200*time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		providers, err := s.Providers.For(ctx, sh)
		if err != nil {
			s.Log.Warn("shadow providers unavailable", "tenant", sh.Tenant.ID, "version", sh.ConfigVersion, "err", err)
			return
		}
		// No request ID: the shadow run must not record ad prices under the live request's.
		q.ConfigVersion, q.BaseFilter, q.Explain, q.RequestID = sh.ConfigVersion, sh.AlwaysFilters, false, ""
		if q.Facets != nil {
			q.Facets = facetRequests(sh.Config)
		}
		start := time.Now()
		st, tr := s.Engine.Run(ctx, sh, providers, &q)
		shadowTop := topIDs(st.Candidates, ShadowTopK)
		sum := sha256.Sum256([]byte(q.Text + "\x00" + q.Raw))
		deg := uint8(0)
		if st.Degraded {
			deg = 1
			for _, stage := range tr.Stages {
				if stage.Error != "" {
					s.Log.Warn("shadow stage failed", "tenant", sh.Tenant.ID, "version", sh.ConfigVersion, "stage", stage.Stage, "err", stage.Error)
				}
			}
		}
		s.Sink.Write(ShadowRow{
			TS: time.Now().UTC(), RequestID: requestID, TenantID: string(res.Tenant.ID), QueryType: string(q.Type),
			QueryHash: hex.EncodeToString(sum[:8]), ActiveVersion: uint32(res.ConfigVersion), ShadowVersion: uint32(sh.ConfigVersion), //nolint:gosec // versions are small
			ActiveTop: activeTop, ShadowTop: shadowTop, OverlapAt10: overlap(activeTop, shadowTop),
			ActiveLatencyMS: float32(primaryLatency.Microseconds()) / 1000, ShadowLatencyMS: float32(time.Since(start).Microseconds()) / 1000,
			ShadowDegraded: deg,
		})
	}()
}

// ClickHouseSink batches shadow rows into ClickHouse.
type ClickHouseSink struct {
	conn driver.Conn
	rows chan ShadowRow
	log  *slog.Logger
}

// ClickHouseConfig locates the analytics database.
type ClickHouseConfig struct {
	Addr, Database, Username, Password string
}

// NewClickHouseSink opens a ClickHouse connection; call Run to flush.
func NewClickHouseSink(c ClickHouseConfig, log *slog.Logger) (*ClickHouseSink, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{c.Addr}, Auth: clickhouse.Auth{Database: c.Database, Username: c.Username, Password: c.Password},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	return &ClickHouseSink{conn: conn, rows: make(chan ShadowRow, 10000), log: log}, nil
}

// Write enqueues a row, dropping it when the buffer is full.
func (s *ClickHouseSink) Write(row ShadowRow) {
	select {
	case s.rows <- row:
	default:
		s.log.Warn("shadow buffer full; row dropped")
	}
}

// Run flushes every interval or batch rows until ctx ends.
func (s *ClickHouseSink) Run(ctx context.Context, interval time.Duration, batch int) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var buf []ShadowRow
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := s.insert(context.WithoutCancel(ctx), buf); err != nil {
			s.log.Warn("shadow rows not written", "rows", len(buf), "err", err)
		}
		buf = buf[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			_ = s.conn.Close()
			return
		case r := <-s.rows:
			buf = append(buf, r)
			if len(buf) >= batch {
				flush()
			}
		case <-t.C:
			flush()
		}
	}
}

func (s *ClickHouseSink) insert(ctx context.Context, rows []ShadowRow) error {
	b, err := s.conn.PrepareBatch(ctx, "INSERT INTO shadow_results")
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	for _, r := range rows {
		if err := b.Append(r.TS, r.RequestID, r.TenantID, r.QueryType, r.QueryHash, r.ActiveVersion, r.ShadowVersion,
			r.ActiveTop, r.ShadowTop, r.OverlapAt10, r.ActiveLatencyMS, r.ShadowLatencyMS, r.ShadowDegraded); err != nil {
			return fmt.Errorf("append: %w", err)
		}
	}
	if err := b.Send(); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	return nil
}
