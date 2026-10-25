package indexer

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// Index job statuses.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
)

// JobRunner executes index jobs queued by control-api (POST /index/jobs and the onboarding
// index-build step): it claims one job at a time across indexer replicas, publishes uploaded
// products when the job has an upload, then backfills a new index version for the job's config
// version and promotes it.
type JobRunner struct {
	DB           *sql.DB
	Builder      *Builder
	Processor    *Processor
	Brokers      []string
	S3           *s3.Client
	UploadBucket string
	Log          *slog.Logger
}

// Run polls for queued jobs until ctx ends.
func (j *JobRunner) Run(ctx context.Context, poll time.Duration) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		for {
			job, ok, err := j.claim(ctx)
			if err != nil {
				j.Log.Warn("claim index job", "err", err)
				break
			}
			if !ok {
				break
			}
			j.execute(ctx, job)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (j *JobRunner) claim(ctx context.Context) (tenantdb.IndexJob, bool, error) {
	tx, err := j.DB.BeginTx(ctx, nil)
	if err != nil {
		return tenantdb.IndexJob{}, false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	q := tenantdb.New(tx)
	job, err := q.NextQueuedIndexJob(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return job, false, nil
	}
	if err != nil {
		return job, false, fmt.Errorf("next job: %w", err)
	}
	if n, err := q.StartIndexJob(ctx, job.ID); err != nil || n == 0 {
		return job, false, err
	}
	if err := tx.Commit(); err != nil {
		return job, false, fmt.Errorf("commit: %w", err)
	}
	return job, true, nil
}

func (j *JobRunner) execute(ctx context.Context, job tenantdb.IndexJob) {
	q := tenantdb.New(j.DB)
	log := j.Log.With("job", job.ID, "tenant", job.TenantID, "config_version", job.ConfigVersion)
	finish := func(status string, err error, version int) {
		p := tenantdb.FinishIndexJobParams{Status: status, ID: job.ID}
		if err != nil {
			p.Error = sql.NullString{String: err.Error(), Valid: true}
		}
		if version > 0 {
			p.IndexVersion = sql.NullInt32{Int32: int32(version), Valid: true} //nolint:gosec // small
		}
		if ferr := q.FinishIndexJob(context.WithoutCancel(ctx), p); ferr != nil {
			log.Error("record job result", "err", ferr)
		}
	}
	id := tenant.ID(job.TenantID)
	total, failed := 0, 0
	if job.Source == tenantdb.IndexJobsSourceUpload {
		var err error
		if total, failed, err = j.publishUpload(ctx, id, int(job.ConfigVersion), job.UploadKey.String); err != nil {
			log.Error("upload publish failed", "err", err)
			finish(JobFailed, err, 0)
			return
		}
		_ = q.UpdateIndexJobProgress(ctx, tenantdb.UpdateIndexJobProgressParams{Total: int32(total), Failed: int32(failed), ID: job.ID}) //nolint:gosec // counts fit
	}
	progress := func(r Result) {
		_ = q.UpdateIndexJobProgress(ctx, tenantdb.UpdateIndexJobProgressParams{
			Total: int32(max(total, len(r.UpsertedIDs))), Done: int32(len(r.UpsertedIDs)), Failed: int32(failed + r.Invalid), ID: job.ID, //nolint:gosec // counts fit
		})
	}
	log.Info("index job started", "source", job.Source)
	rep, err := BackfillVersion(ctx, j.Brokers, id, int(job.ConfigVersion), j.Builder, j.Processor, log, progress)
	if err != nil {
		log.Error("index job failed", "err", err)
		finish(JobFailed, err, rep.Version)
		return
	}
	progress(rep.Result)
	_ = q.UpdateIndexJobProgress(ctx, tenantdb.UpdateIndexJobProgressParams{
		Total: int32(max(total, rep.Expected)), Done: int32(rep.Expected), Failed: int32(failed + rep.Result.Invalid), ID: job.ID, //nolint:gosec // counts fit
	})
	log.Info("index job finished", "index_version", rep.Version, "products", rep.Expected, "took", rep.Duration.Round(time.Millisecond).String())
	finish(JobSucceeded, nil, rep.Version)
}

// publishUpload turns an uploaded JSONL file (one tenant product document per line) into
// catalog events. The source version is the publish time, so the upload supersedes older data.
func (j *JobRunner) publishUpload(ctx context.Context, id tenant.ID, version int, key string) (int, int, error) {
	t, _, err := j.Builder.BuildVersion(ctx, id, version)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = t.Close() }()
	obj, err := j.S3.GetObject(ctx, &s3.GetObjectInput{Bucket: &j.UploadBucket, Key: &key})
	if err != nil {
		return 0, 0, fmt.Errorf("read upload %s: %w", key, err)
	}
	defer obj.Body.Close()
	cl, err := kgo.NewClient(kgo.SeedBrokers(j.Brokers...))
	if err != nil {
		return 0, 0, fmt.Errorf("kafka client: %w", err)
	}
	defer cl.Close()

	sourceVersion := time.Now().UnixMilli()
	sc := bufio.NewScanner(obj.Body)
	sc.Buffer(make([]byte, 1<<20), 8<<20)
	total, failed := 0, 0
	var batch []*kgo.Record
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := cl.ProduceSync(ctx, batch...).FirstErr()
		batch = batch[:0]
		if err != nil {
			return fmt.Errorf("produce: %w", err)
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		total++
		p, errs := t.Mapper.Map(string(id), line)
		if len(errs) > 0 {
			failed++
			continue
		}
		e := catalog.ProductEvent{EventID: uuid.NewString(), TenantID: string(id), ProductID: p.ProductID, Op: catalog.OpUpsert,
			TS: time.Now().UTC(), SourceVersion: sourceVersion, Product: json.RawMessage(append([]byte(nil), line...))}
		value, err := json.Marshal(e)
		if err != nil {
			return total, failed, fmt.Errorf("encode event: %w", err)
		}
		r := &kgo.Record{Topic: catalog.Topic, Key: e.Key(), Value: value}
		for k, v := range e.Headers() {
			r.Headers = append(r.Headers, kgo.RecordHeader{Key: k, Value: []byte(v)})
		}
		batch = append(batch, r)
		if len(batch) == 500 {
			if err := flush(); err != nil {
				return total, failed, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return total, failed, fmt.Errorf("read upload: %w", err)
	}
	return total, failed, flush()
}
