package indexer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/tenant"
)

// Tuning for the consumer loops.
const (
	pollMax        = 512
	targetTTL      = 30 * time.Second
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 30 * time.Second
)

// Consumer is the streaming indexer: a consumer group on catalog.product.v1 that applies
// events to each tenant's active (aliased) index and commits offsets after they are applied.
type Consumer struct {
	client  *kgo.Client
	builder *Builder
	proc    *Processor
	log     *slog.Logger
	targets map[tenant.ID]cachedTarget
}

type cachedTarget struct {
	target  *Target
	builtAt time.Time
}

// NewConsumer joins consumer group `group` on the catalog topic.
func NewConsumer(brokers []string, group string, builder *Builder, proc *Processor, log *slog.Logger) (*Consumer, error) {
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(group),
		kgo.ConsumeTopics(catalog.Topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
	)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return &Consumer{client: cl, builder: builder, proc: proc, log: log, targets: map[tenant.ID]cachedTarget{}}, nil
}

// Close leaves the group.
func (c *Consumer) Close() { c.client.Close() }

// Run consumes until ctx is cancelled. A failing batch is retried with backoff and never
// skipped, so delivery is at-least-once; every write is idempotent.
func (c *Consumer) Run(ctx context.Context) error {
	for {
		fetches := c.client.PollRecords(ctx, pollMax)
		if ctx.Err() != nil {
			return nil
		}
		fetches.EachError(func(t string, p int32, err error) {
			c.log.Error("fetch failed", "topic", t, "partition", p, "err", err)
		})
		records := fetches.Records()
		if len(records) > 0 {
			if err := c.applyWithRetry(ctx, records); err != nil {
				return err
			}
			if err := c.client.CommitRecords(ctx, records...); err != nil && ctx.Err() == nil {
				c.log.Error("commit failed", "err", err)
			}
		}
		c.client.AllowRebalance()
	}
}

func (c *Consumer) applyWithRetry(ctx context.Context, records []*kgo.Record) error {
	delay := retryBaseDelay
	for {
		res, err := c.apply(ctx, records)
		if err == nil {
			c.log.Info("applied catalog events", "records", len(records), "upserted", len(res.UpsertedIDs),
				"deleted", len(res.DeletedIDs), "stale", res.Stale, "invalid", res.Invalid, "missing_image", res.MissingImage)
			return nil
		}
		c.log.Error("apply failed; retrying", "err", err, "delay", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, retryMaxDelay)
	}
}

func (c *Consumer) apply(ctx context.Context, records []*kgo.Record) (Result, error) {
	var res Result
	byTenant, invalid := groupByTenant(records, c.log)
	res.Invalid += invalid
	for id, events := range byTenant {
		t, err := c.target(ctx, id)
		if errors.Is(err, tenant.ErrNotFound) {
			c.log.Warn("events for unknown tenant skipped", "tenant", id, "events", len(events))
			res.Invalid += len(events)
			continue
		}
		if err != nil {
			return res, err
		}
		r, err := c.proc.ProcessAll(ctx, t, events)
		if err != nil {
			return res, fmt.Errorf("tenant %s: %w", id, err)
		}
		res.Add(r)
	}
	return res, nil
}

// target returns the tenant's cached Target, rebuilding it when older than targetTTL so config
// activations are picked up. The first use creates and promotes v1 of the tenant's stores.
func (c *Consumer) target(ctx context.Context, id tenant.ID) (*Target, error) {
	if ct, ok := c.targets[id]; ok && time.Since(ct.builtAt) < targetTTL {
		return ct.target, nil
	}
	t, stores, err := c.builder.Build(ctx, id)
	if err != nil {
		return nil, err
	}
	v, err := stores.EnsureActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant %s: %w", id, err)
	}
	if old, ok := c.targets[id]; !ok || old.target.ConfigVersion != t.ConfigVersion {
		c.log.Info("indexing target ready", "tenant", id, "config_version", t.ConfigVersion, "index_version", v)
	}
	c.targets[id] = cachedTarget{target: t, builtAt: time.Now()}
	return t, nil
}

// BackfillReport summarizes a backfill.
type BackfillReport struct {
	Tenant       tenant.ID
	Version      int
	Events       int
	Result       Result
	Expected     int
	LexicalCount int64
	VectorCount  int64
	Promoted     bool
	Duration     time.Duration
}

// Backfill rebuilds a tenant's index from the start of the catalog topic into a new version
// v{n+1}, verifies that both stores hold exactly the live products, then promotes v{n+1} and
// drops versions older than the previous one (kept for rollback). Events produced while the
// backfill runs are read until the end offsets observed when it finished the first pass.
func Backfill(ctx context.Context, brokers []string, id tenant.ID, builder *Builder, proc *Processor, log *slog.Logger) (BackfillReport, error) {
	start := time.Now()
	rep := BackfillReport{Tenant: id}
	t, stores, err := builder.Build(ctx, id)
	if err != nil {
		return rep, err
	}
	previous, err := stores.Lexical.Active(ctx)
	if err != nil {
		return rep, err
	}
	if rep.Version, err = stores.nextVersion(ctx); err != nil {
		return rep, err
	}
	if err := stores.create(ctx, rep.Version); err != nil {
		return rep, err
	}
	bt := *t
	bt.Lexical, bt.Vectors = stores.Lexical.Version(rep.Version), stores.Vectors.Version(rep.Version)
	log.Info("backfill started", "tenant", id, "version", rep.Version, "config_version", t.ConfigVersion)

	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...), kgo.ConsumeTopics(catalog.Topic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		return rep, fmt.Errorf("kafka client: %w", err)
	}
	defer cl.Close()

	alive := map[string]bool{}
	ends := map[int32]*partitionProgress{}
	handle := func(records []*kgo.Record) error {
		byTenant, invalid := groupByTenant(records, log)
		rep.Events += len(records)
		rep.Result.Invalid += invalid
		r, err := proc.ProcessAll(ctx, &bt, byTenant[id])
		if err != nil {
			return err
		}
		for _, pid := range r.UpsertedIDs {
			alive[pid] = true
		}
		for _, pid := range r.DeletedIDs {
			delete(alive, pid)
		}
		rep.Result.Add(r)
		return nil
	}
	for range 2 {
		if err := catchUp(ctx, cl, ends, handle); err != nil {
			return rep, err
		}
	}

	rep.Expected = len(alive)
	if rep.LexicalCount, err = stores.Lexical.Count(ctx, rep.Version); err != nil {
		return rep, err
	}
	if rep.VectorCount, err = stores.Vectors.Count(ctx, rep.Version); err != nil {
		return rep, err
	}
	rep.Duration = time.Since(start)
	if rep.LexicalCount != int64(rep.Expected) || rep.VectorCount != int64(rep.Expected) {
		return rep, fmt.Errorf("count mismatch in v%d: expected %d live products, lexical %d, vector %d; not promoted",
			rep.Version, rep.Expected, rep.LexicalCount, rep.VectorCount)
	}
	if err := stores.promote(ctx, rep.Version); err != nil {
		return rep, err
	}
	rep.Promoted = true
	for v := 1; v < previous; v++ {
		if err := stores.drop(ctx, v); err != nil {
			log.Warn("dropping old index version failed", "tenant", id, "version", v, "err", err)
		}
	}
	return rep, nil
}

// catchUp reads the topic until every partition reaches the end offset observed on entry,
// handing each polled batch to handle. ends carries consumption progress across calls.
func catchUp(ctx context.Context, cl *kgo.Client, ends map[int32]*partitionProgress, handle func([]*kgo.Record) error) error {
	if err := refreshEndOffsets(ctx, cl, ends); err != nil {
		return err
	}
	for !reached(ends) {
		fetches := cl.PollRecords(ctx, pollMax)
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("catch-up interrupted: %w", err)
		}
		if errs := fetches.Errors(); len(errs) > 0 {
			return fmt.Errorf("fetch %s[%d]: %w", errs[0].Topic, errs[0].Partition, errs[0].Err)
		}
		records := fetches.Records()
		for _, r := range records {
			if p, ok := ends[r.Partition]; ok {
				p.next = r.Offset + 1
			}
		}
		if err := handle(records); err != nil {
			return err
		}
	}
	return nil
}

type partitionProgress struct {
	end  int64
	next int64
}

// refreshEndOffsets records each partition's current end offset in ps. A partition seen for the
// first time starts at its log start offset; known partitions keep how far they were consumed.
func refreshEndOffsets(ctx context.Context, cl *kgo.Client, ps map[int32]*partitionProgress) error {
	adm := kadm.NewClient(cl)
	startOffs, err := adm.ListStartOffsets(ctx, catalog.Topic)
	if err != nil {
		return fmt.Errorf("list start offsets: %w", err)
	}
	endOffs, err := adm.ListEndOffsets(ctx, catalog.Topic)
	if err != nil {
		return fmt.Errorf("list end offsets: %w", err)
	}
	if err := endOffs.Error(); err != nil {
		return fmt.Errorf("list end offsets: %w", err)
	}
	endOffs.Each(func(o kadm.ListedOffset) {
		if p, ok := ps[o.Partition]; ok {
			p.end = o.Offset
			return
		}
		s, _ := startOffs.Lookup(o.Topic, o.Partition)
		ps[o.Partition] = &partitionProgress{end: o.Offset, next: s.Offset}
	})
	return nil
}

func reached(ps map[int32]*partitionProgress) bool {
	for _, p := range ps {
		if p.next < p.end {
			return false
		}
	}
	return true
}

func groupByTenant(records []*kgo.Record, log *slog.Logger) (map[tenant.ID][]catalog.ProductEvent, int) {
	out := map[tenant.ID][]catalog.ProductEvent{}
	invalid := 0
	for _, r := range records {
		headers := make(map[string]string, len(r.Headers))
		for _, h := range r.Headers {
			headers[h.Key] = string(h.Value)
		}
		e, err := catalog.DecodeProductEvent(r.Key, r.Value, headers)
		if err != nil {
			invalid++
			log.Warn("undecodable catalog event skipped", "partition", r.Partition, "offset", r.Offset, "err", err)
			continue
		}
		id := tenant.ID(e.TenantID)
		out[id] = append(out[id], e)
	}
	return out, invalid
}
