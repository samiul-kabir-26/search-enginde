package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/oklog/ulid/v2"

	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/feature"
	"github.com/zavisoft/search-engine/internal/platform"
	embedder "github.com/zavisoft/search-engine/internal/providers/embedder/kservev2"
	"github.com/zavisoft/search-engine/internal/providers/scorer/artifacts"
	"github.com/zavisoft/search-engine/internal/providers/scorer/onnx"
	"github.com/zavisoft/search-engine/internal/registry"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

// ModelStore reads learned scorer artifacts (artifacts.Store).
type ModelStore interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Size(ctx context.Context, key string) (int64, error)
	Manifest(ctx context.Context, id string) (artifacts.Manifest, error)
}

// MaxScorerFeatures bounds a learned scorer's feature schema.
const MaxScorerFeatures = 64

// checkFeatures validates a scorer's declared feature schema (docs/TENANT_CONFIG.md §5 rule 9):
// unique catalog features, no user features (re-rank only).
func checkFeatures(path string, names []string, kind string) []platform.FieldError {
	var errs []platform.FieldError
	if len(names) == 0 || len(names) > MaxScorerFeatures {
		errs = append(errs, fieldErr(path, "invalid", fmt.Sprintf("declare 1 to %d features, in the model's input order", MaxScorerFeatures)))
	}
	cat, seen := feature.Catalog(), map[string]bool{}
	for i, n := range names {
		p := fmt.Sprintf("%s/%d", path, i)
		src, ok := cat[n]
		switch {
		case !ok:
			errs = append(errs, fieldErr(p, "unknown_feature", fmt.Sprintf("%q is not in the feature catalog", n)))
		case src == feature.SourceUser && kind != tenant.ModelKindRerank:
			errs = append(errs, fieldErr(p, "unavailable", "user features are only available to the re-ranker"))
		case seen[n]:
			errs = append(errs, fieldErr(p, "duplicate", fmt.Sprintf("%q is listed twice", n)))
		}
		seen[n] = true
	}
	return errs
}

// scorerRequest is the part of POST /models a learned scorer uses.
type scorerRequest struct {
	Kind, Type, Name, Version, Endpoint, SecretRef, UploadID string
	Features                                                 []string
}

// registerScorer registers a prerank or rank model: an uploaded ONNX model, validated by a dry
// run against its declared feature schema now (docs/ARCHITECTURE.md §9), or a kserve_v2
// endpoint, validated later by POST /models/{id}/validate.
func (s *Server) registerScorer(w http.ResponseWriter, r *http.Request, in scorerRequest) {
	var errs []platform.FieldError
	switch in.Kind {
	case tenant.ModelKindPrerank, tenant.ModelKindRank, tenant.ModelKindRerank:
	default:
		errs = append(errs, fieldErr("/kind", "invalid", "kind must be prerank, rank or rerank"))
	}
	if in.Kind == tenant.ModelKindRerank && in.Type != tenant.ModelKServeV2 {
		errs = append(errs, fieldErr("/type", "invalid", "re-ranker models must be kserve_v2"))
	}
	if in.Name == "" || len(in.Name) > 255 {
		errs = append(errs, fieldErr("/name", "invalid", "name must be 1 to 255 characters"))
	}
	if len(in.Version) > 64 {
		errs = append(errs, fieldErr("/version", "invalid", "version must be at most 64 characters"))
	}
	errs = append(errs, checkFeatures("/input_schema/features", in.Features, in.Kind)...)
	switch in.Type {
	case tenant.ModelONNX:
		if _, err := ulid.ParseStrict(in.UploadID); err != nil {
			errs = append(errs, fieldErr("/upload_id", "required", "ONNX models need the upload_id of POST /models/uploads"))
		}
		if in.Endpoint != "" || in.SecretRef != "" {
			errs = append(errs, fieldErr("/endpoint", "invalid", "ONNX models run in-process; they take no endpoint or secret"))
		}
	case tenant.ModelKServeV2:
		if in.UploadID != "" {
			errs = append(errs, fieldErr("/upload_id", "invalid", "kserve_v2 models are remote; they take an endpoint, not an upload"))
		}
	default:
		errs = append(errs, fieldErr("/type", "invalid", "scorer models are onnx or kserve_v2"))
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	plan, ok := s.byoPlan(w, r)
	if !ok {
		return
	}
	tid := pathTenant(r)
	params := tenantdb.CreateTenantModelParams{
		ID: tenant.NewID().String(), TenantID: string(tid), Kind: tenantdb.TenantModelsKind(in.Kind), Type: tenantdb.TenantModelsType(in.Type),
		Name: in.Name, Version: in.Version, InputSchema: settingsJSON(tenant.ModelInput{Features: in.Features}),
		OutputSchema: settingsJSON(tenant.ModelOutput{}), Settings: settingsJSON(tenant.ModelSettings{}), Status: tenant.StatusRegistered,
	}
	if in.Type == tenant.ModelONNX {
		key := artifacts.UploadKey(string(tid), in.UploadID)
		data, ferr, err := s.uploadedModel(r.Context(), key)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if ferr != nil {
			invalid(w, *ferr)
			return
		}
		if !s.ONNXReady {
			problem(w, http.StatusServiceUnavailable, "ONNX Runtime is not available on this control plane")
			return
		}
		if err := onnx.DryRun(data, pipeline.NewFeatureSchema(in.Features)); err != nil {
			invalid(w, fieldErr("/upload_id", "dry_run_failed", err.Error()))
			return
		}
		params.ArtifactUri, params.ArtifactSha256 = nullString(key), nullString(artifacts.SHA256(data))
		params.Status, params.ValidatedAt = tenant.StatusValidated, sql.NullTime{Time: time.Now().UTC(), Valid: true}
	} else {
		if _, err := s.BYO.Egress.CheckURL(r.Context(), in.Endpoint, plan.EgressAllowlist); err != nil {
			errs = append(errs, fieldErr("/endpoint", "egress_denied", err.Error()))
		}
		if in.SecretRef != "" {
			if _, err := s.BYO.Secrets.Get(r.Context(), string(tid), in.SecretRef); err != nil {
				errs = append(errs, fieldErr("/secret_ref", "not_found", "no such secret"))
			}
		}
		if len(errs) > 0 {
			invalid(w, errs...)
			return
		}
		params.Endpoint, params.SecretRef = nullString(in.Endpoint), nullString(in.SecretRef)
	}
	if err := s.Q.CreateTenantModel(r.Context(), params); err != nil {
		s.internalError(w, err)
		return
	}
	s.writeModel(w, r, params.ID, http.StatusCreated)
}

// uploadedModel reads an uploaded artifact; a missing or oversized upload is a field error.
func (s *Server) uploadedModel(ctx context.Context, key string) ([]byte, *platform.FieldError, error) {
	if s.Models == nil {
		return nil, nil, errors.New("no model store configured")
	}
	size, err := s.Models.Size(ctx, key)
	if errors.Is(err, artifacts.ErrNotFound) {
		fe := fieldErr("/upload_id", "not_found", "nothing was uploaded for this upload_id (PUT the file to the presigned URL first)")
		return nil, &fe, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if size > onnx.MaxModelBytes {
		fe := fieldErr("/upload_id", "too_large", fmt.Sprintf("the model is %d MB; the limit is %d MB", size>>20, onnx.MaxModelBytes>>20))
		return nil, &fe, nil
	}
	data, err := s.Models.Get(ctx, key)
	return data, nil, err
}

// handleModelUpload returns a presigned PUT for an ONNX model (≤ 200 MB, 15 minutes).
func (s *Server) handleModelUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Format string `json:"format"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	if req.Format != "" && req.Format != "onnx" {
		invalid(w, fieldErr("/format", "unsupported", "only onnx models can be uploaded"))
		return
	}
	if _, ok := s.byoPlan(w, r); !ok {
		return
	}
	if s.Models == nil || s.Presign == nil {
		problem(w, http.StatusServiceUnavailable, "model uploads are not configured on this control plane")
		return
	}
	id := tenant.NewID().String()
	out, err := s.Presign.PresignPutObject(r.Context(), &s3.PutObjectInput{
		Bucket: aws.String(s.ModelBucket), Key: aws.String(artifacts.UploadKey(string(pathTenant(r)), id)),
		ContentType: aws.String("application/octet-stream"),
	}, s3.WithPresignExpires(UploadURLTTL))
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, PresignedUpload{
		UploadID: id, Method: http.MethodPut, URL: out.URL, Headers: map[string]string{"Content-Type": "application/octet-stream"},
		ExpiresAt: time.Now().Add(UploadURLTTL).UTC(),
	})
}

// validateScorer re-checks a scorer model: an ONNX model's artifact (digest and dry run), or a
// kserve_v2 endpoint's probe inference over gRPC, then REST; the transport that works is kept.
func (s *Server) validateScorer(ctx context.Context, tid tenant.ID, plan tenant.Plan, m tenant.Model) (ModelValidation, tenant.ModelSettings) {
	start := time.Now()
	var res ModelValidation
	settings := m.Settings
	schema := pipeline.NewFeatureSchema(m.Input.Features)
	if m.Type == tenant.ModelONNX {
		check := Check{Name: "dry_run"}
		data, err := s.Models.Get(ctx, m.ArtifactURI)
		switch {
		case err != nil:
			check.Message = err.Error()
		case artifacts.SHA256(data) != m.ArtifactSHA256:
			check.Message = "the stored artifact does not match its recorded sha256"
		case !s.ONNXReady:
			check.Message = "ONNX Runtime is not available on this control plane"
		default:
			if err := onnx.DryRun(data, schema); err != nil {
				check.Message = err.Error()
			} else {
				check.OK, check.Message = true, fmt.Sprintf("%d features, finite scores", len(schema.Names))
			}
		}
		res.Checks, res.OK = []Check{check}, check.OK
	} else {
		for _, tr := range []string{embedder.TransportGRPC, embedder.TransportREST} {
			mm := m
			mm.Settings.Transport = tr
			checks := []Check{{Name: "transport", OK: true, Message: tr}, s.probeScorer(ctx, tid, plan, mm)}
			res.Checks = append(res.Checks, checks...)
			if allOK(checks) {
				res.OK, res.Checks, settings.Transport = true, checks, tr
				break
			}
		}
	}
	res.LatencyMS = math.Round(float64(time.Since(start).Microseconds())/10) / 100
	return res, settings
}

func (s *Server) probeScorer(ctx context.Context, tid tenant.ID, plan tenant.Plan, m tenant.Model) Check {
	c := Check{Name: "probe"}
	sc, err := registry.NewBYOScorer(ctx, s.BYO, tid, plan, m)
	if err != nil {
		c.Message = err.Error()
		return c
	}
	defer func() { _ = sc.Close() }()
	pctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	t0 := time.Now()
	if err := sc.Probe(pctx); err != nil {
		c.Message = err.Error()
		return c
	}
	ms := float64(time.Since(t0).Microseconds()) / 1000
	c.OK, c.LatencyMS = true, &ms
	c.Message = fmt.Sprintf("2 rows × %d features, finite scores", len(m.Input.Features))
	return c
}

// Scorer implements tenant.BYOChecker for learned scorers (rule 9).
func (c byoChecker) Scorer(ctx context.Context, modelID, kind string) (tenant.ScorerInfo, []platform.FieldError) {
	m, err := c.s.Tenants.Model(ctx, c.tid, modelID)
	if errors.Is(err, tenant.ErrNotFound) {
		return tenant.ScorerInfo{}, []platform.FieldError{fieldErr("/model_id", "not_found", "no such model")}
	}
	if err != nil {
		return tenant.ScorerInfo{}, []platform.FieldError{fieldErr("/model_id", "unavailable", err.Error())}
	}
	if m.Kind != kind {
		return tenant.ScorerInfo{}, []platform.FieldError{fieldErr("/model_id", "wrong_kind", fmt.Sprintf("model %s is a %s model, not %s", m.ID, m.Kind, kind))}
	}
	if m.Status != tenant.StatusValidated {
		return tenant.ScorerInfo{}, []platform.FieldError{fieldErr("/model_id", "model_not_validated", fmt.Sprintf("model %s is %s; validate it first", m.ID, m.Status))}
	}
	info := tenant.ScorerInfo{Type: m.Type, Features: m.Input.Features}
	key := string(c.tid) + "/scorer/" + m.ID
	if m.Type != tenant.ModelKServeV2 || c.s.probes.ok(key) {
		return info, nil
	}
	if check := c.s.probeScorer(ctx, c.tid, c.plan, m); !check.OK {
		return tenant.ScorerInfo{}, []platform.FieldError{fieldErr("/model_id", "probe_failed", check.Message)}
	}
	c.s.probes.remember(key)
	return info, nil
}

// PlatformScorer describes a platform scorer for config validation: its type and the feature
// schema in its trained manifest.
func (s *Server) PlatformScorer(ctx context.Context, id string) (tenant.ScorerInfo, error) {
	ps, ok := artifacts.Platform[id]
	if !ok {
		return tenant.ScorerInfo{}, fmt.Errorf("%q is not a platform scorer", id)
	}
	if s.Models == nil {
		return tenant.ScorerInfo{}, errors.New("no model store configured")
	}
	m, err := s.Models.Manifest(ctx, id)
	if errors.Is(err, artifacts.ErrNotFound) {
		return tenant.ScorerInfo{}, fmt.Errorf("%s has not been trained yet (make train)", id)
	}
	if err != nil {
		return tenant.ScorerInfo{}, err
	}
	return tenant.ScorerInfo{Type: ps.Type, Features: m.Features}, nil
}

// platformScorerViews lists the platform scorers with their availability.
func (s *Server) platformScorerViews(ctx context.Context) []PlatformModelView {
	ids := make([]string, 0, len(artifacts.Platform))
	for id := range artifacts.Platform {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]PlatformModelView, 0, len(ids))
	for _, id := range ids {
		ps := artifacts.Platform[id]
		v := PlatformModelView{ID: id, Kind: "scorer", Type: ps.Type,
			Description: "LightGBM lambdarank trained on interaction events (docs/MODELS.md §5)."}
		if info, err := s.PlatformScorer(ctx, id); err == nil {
			v.Available, v.Features = true, info.Features
		}
		out = append(out, v)
	}
	return out
}
