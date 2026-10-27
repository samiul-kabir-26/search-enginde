package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// UploadURLTTL is how long a presigned catalog upload URL works.
const UploadURLTTL = 15 * time.Minute

func (s *Server) catalogRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/tenants/{tid}/catalog/mapping/preview", s.tenantRoute(auth.ScopeTenantRead, s.handleMappingPreview))
	mux.Handle("POST /v1/tenants/{tid}/policy/test", s.tenantRoute(auth.ScopeTenantRead, s.handlePolicyTest))
	mux.Handle("POST /v1/tenants/{tid}/catalog/uploads", s.tenantRoute(auth.ScopeTenantWrite, s.tenantIdem(s.handleUpload).ServeHTTP))
	mux.Handle("POST /v1/tenants/{tid}/index/jobs", s.tenantRoute(auth.ScopeTenantWrite, s.tenantIdem(s.handleCreateJob).ServeHTTP))
	mux.Handle("GET /v1/tenants/{tid}/index/jobs", s.tenantRoute(auth.ScopeTenantRead, s.handleListJobs))
	mux.Handle("GET /v1/tenants/{tid}/index/jobs/{job_id}", s.tenantRoute(auth.ScopeTenantRead, s.handleGetJob))
}

func (s *Server) handleMappingPreview(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FieldMapping catalog.FieldMapping  `json:"field_mapping"`
		CustomFields []catalog.CustomField `json:"custom_fields"`
		Samples      []json.RawMessage     `json:"samples"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	if len(req.Samples) == 0 || len(req.Samples) > 20 {
		invalid(w, fieldErr("/samples", "invalid", "1 to 20 samples"))
		return
	}
	if req.FieldMapping == nil {
		d, err := s.Q.GetConfigDraft(r.Context(), r.PathValue("tid"))
		if err == nil {
			if cfg, err := tenant.ParseConfig(d.Document); err == nil {
				req.FieldMapping, req.CustomFields = cfg.Catalog.FieldMapping, cfg.Catalog.CustomFields
			}
		}
	}
	mapper, err := catalog.NewMapper(req.FieldMapping, req.CustomFields)
	if err != nil {
		invalid(w, append(catalog.ValidateMapping(req.FieldMapping), catalog.ValidateCustomFields(req.CustomFields, 0)...)...)
		return
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"results": mapper.Preview(r.PathValue("tid"), req.Samples)})
}

// PresignedUpload is where the client PUTs a catalog file.
type PresignedUpload struct {
	UploadID  string            `json:"upload_id"`
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

func uploadKey(tid tenant.ID, id string) string {
	return "uploads/" + string(tid) + "/" + id + ".jsonl"
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Format string `json:"format"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	if req.Format != "" && req.Format != "jsonl" {
		invalid(w, fieldErr("/format", "unsupported", "only jsonl (one tenant product JSON per line) is supported"))
		return
	}
	id := tenant.NewID().String()
	key := uploadKey(pathTenant(r), id)
	out, err := s.Presign.PresignPutObject(r.Context(), &s3.PutObjectInput{
		Bucket: aws.String(s.UploadBucket), Key: aws.String(key), ContentType: aws.String("application/x-ndjson"),
	}, s3.WithPresignExpires(UploadURLTTL))
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, PresignedUpload{
		UploadID: id, Method: http.MethodPut, URL: out.URL, Headers: map[string]string{"Content-Type": "application/x-ndjson"},
		ExpiresAt: time.Now().Add(UploadURLTTL).UTC(),
	})
}

// JobView is an index job as the API returns it.
type JobView struct {
	ID            string     `json:"id"`
	ConfigVersion int        `json:"config_version"`
	Source        string     `json:"source"`
	Status        string     `json:"status"`
	Total         int        `json:"total"`
	Done          int        `json:"done"`
	Failed        int        `json:"failed"`
	IndexVersion  *int       `json:"index_version,omitempty"`
	Error         string     `json:"error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

func jobView(j tenantdb.IndexJob) JobView {
	v := JobView{ID: j.ID, ConfigVersion: int(j.ConfigVersion), Source: string(j.Source), Status: j.Status, Total: int(j.Total),
		Done: int(j.Done), Failed: int(j.Failed), Error: j.Error.String, CreatedAt: j.CreatedAt}
	if j.IndexVersion.Valid {
		iv := int(j.IndexVersion.Int32)
		v.IndexVersion = &iv
	}
	if j.StartedAt.Valid {
		v.StartedAt = &j.StartedAt.Time
	}
	if j.FinishedAt.Valid {
		v.FinishedAt = &j.FinishedAt.Time
	}
	return v
}

// createIndexJob queues a backfill of config version v; with an upload ID the indexer first
// publishes the uploaded products as catalog events.
func (s *Server) createIndexJob(ctx context.Context, tid tenant.ID, v int, uploadID string) (JobView, error) {
	id := tenant.NewID().String()
	p := tenantdb.CreateIndexJobParams{ID: id, TenantID: string(tid), ConfigVersion: int32(v), Source: tenantdb.IndexJobsSourceTopic} //nolint:gosec // small
	if uploadID != "" {
		if strings.ContainsAny(uploadID, "/.") {
			return JobView{}, block(fieldErr("/upload_id", "invalid", "unknown upload"))
		}
		p.Source, p.UploadKey = tenantdb.IndexJobsSourceUpload, nullString(uploadKey(tid, uploadID))
	}
	if err := s.Q.CreateIndexJob(ctx, p); err != nil {
		return JobView{}, fmt.Errorf("create index job: %w", err)
	}
	j, err := s.Q.GetIndexJob(ctx, tenantdb.GetIndexJobParams{ID: id, TenantID: string(tid)})
	if err != nil {
		return JobView{}, fmt.Errorf("read index job: %w", err)
	}
	return jobView(j), nil
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ConfigVersion int    `json:"config_version"`
		UploadID      string `json:"upload_id"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	ctx, tid := r.Context(), pathTenant(r)
	if _, err := s.Q.GetConfigVersion(ctx, tenantdb.GetConfigVersionParams{TenantID: string(tid), Version: int32(req.ConfigVersion)}); err != nil { //nolint:gosec // small
		invalid(w, fieldErr("/config_version", "unknown", "publish the version first"))
		return
	}
	j, err := s.createIndexJob(ctx, tid, req.ConfigVersion, req.UploadID)
	if err != nil {
		var se stepError
		if errors.As(err, &se) {
			invalid(w, se.errs...)
			return
		}
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusAccepted, j)
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListIndexJobs(r.Context(), r.PathValue("tid"))
	if err != nil {
		s.internalError(w, err)
		return
	}
	items := make([]JobView, len(rows))
	for i, j := range rows {
		items[i] = jobView(j)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.Q.GetIndexJob(r.Context(), tenantdb.GetIndexJobParams{ID: r.PathValue("job_id"), TenantID: r.PathValue("tid")})
	if err != nil {
		problem(w, http.StatusNotFound, "no such job")
		return
	}
	platform.WriteJSON(w, http.StatusOK, jobView(j))
}
