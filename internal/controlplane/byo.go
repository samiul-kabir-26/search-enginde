package controlplane

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/security"
	"github.com/zavisoft/search-engine/internal/tenant"
	"github.com/zavisoft/search-engine/internal/tenant/tenantdb"
)

func (s *Server) byoRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/tenants/{tid}/secrets", s.tenantRoute(auth.ScopeTenantWrite, s.tenantIdem(s.handleCreateSecret).ServeHTTP))
	mux.Handle("GET /v1/tenants/{tid}/connections", s.tenantRoute(auth.ScopeTenantRead, s.handleListConnections))
	mux.Handle("POST /v1/tenants/{tid}/connections", s.tenantRoute(auth.ScopeTenantWrite, s.tenantIdem(s.handleCreateConnection).ServeHTTP))
	mux.Handle("GET /v1/tenants/{tid}/connections/{connection_id}", s.tenantRoute(auth.ScopeTenantRead, s.handleGetConnection))
	mux.Handle("PUT /v1/tenants/{tid}/connections/{connection_id}", s.tenantRoute(auth.ScopeTenantWrite, s.handleUpdateConnection))
	mux.Handle("DELETE /v1/tenants/{tid}/connections/{connection_id}", s.tenantRoute(auth.ScopeTenantWrite, s.handleDeleteConnection))
	mux.Handle("POST /v1/tenants/{tid}/connections/{connection_id}/test", s.tenantRoute(auth.ScopeTenantWrite, s.handleTestConnection))
	mux.Handle("GET /v1/tenants/{tid}/models", s.tenantRoute(auth.ScopeTenantRead, s.handleListModels))
	mux.Handle("POST /v1/tenants/{tid}/models", s.tenantRoute(auth.ScopeTenantWrite, s.tenantIdem(s.handleRegisterModel).ServeHTTP))
	mux.Handle("GET /v1/tenants/{tid}/models/{model_id}", s.tenantRoute(auth.ScopeTenantRead, s.handleGetModel))
	mux.Handle("POST /v1/tenants/{tid}/models/{model_id}/validate", s.tenantRoute(auth.ScopeTenantWrite, s.handleValidateModel))
	mux.HandleFunc("GET /v1/platform/models", s.handlePlatformModels)
}

// tenantPlan loads the path tenant's plan.
func (s *Server) tenantPlan(r *http.Request) (tenant.Plan, error) {
	t, err := s.Tenants.Tenant(r.Context(), pathTenant(r))
	if err != nil {
		return tenant.Plan{}, err
	}
	return s.Tenants.Plan(r.Context(), t.Plan)
}

// byoPlan returns the tenant's plan, or writes 403 when the plan does not allow BYO providers.
func (s *Server) byoPlan(w http.ResponseWriter, r *http.Request) (tenant.Plan, bool) {
	if s.BYO == nil {
		problem(w, http.StatusServiceUnavailable, "BYO providers are not configured on this control plane")
		return tenant.Plan{}, false
	}
	plan, err := s.tenantPlan(r)
	if err != nil {
		s.internalError(w, err)
		return plan, false
	}
	if !plan.BYOAllowed {
		problem(w, http.StatusForbidden, fmt.Sprintf("the %s plan does not allow BYO providers", plan.ID),
			fieldErr("", "plan_forbids_byo", "upgrade to a plan with BYO providers"))
		return plan, false
	}
	return plan, true
}

var secretKind = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func (s *Server) handleCreateSecret(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if !decodeOrFail(w, r, &req) {
		return
	}
	var errs []platform.FieldError
	if !secretKind.MatchString(req.Kind) {
		errs = append(errs, fieldErr("/kind", "invalid", "kind must match ^[a-z][a-z0-9_-]{0,63}$ (for example api_key or password)"))
	}
	if len(req.Value) == 0 || len(req.Value) > security.MaxSecretBytes {
		errs = append(errs, fieldErr("/value", "invalid", fmt.Sprintf("value must be 1 to %d bytes", security.MaxSecretBytes)))
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	if _, ok := s.byoPlan(w, r); !ok {
		return
	}
	ref, err := s.BYO.Secrets.Put(r.Context(), string(pathTenant(r)), req.Kind, req.Value)
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, http.StatusCreated, map[string]string{"secret_ref": ref})
}

// ConnectionView is a connection as the API returns it; secrets appear only as references.
type ConnectionView struct {
	ID           string                    `json:"id"`
	Kind         string                    `json:"kind"`
	Type         string                    `json:"type"`
	Endpoint     string                    `json:"endpoint"`
	SecretRef    string                    `json:"secret_ref,omitempty"`
	Settings     tenant.ConnectionSettings `json:"settings"`
	Status       string                    `json:"status"`
	LastTestedAt *time.Time                `json:"last_tested_at"`
	LastError    *string                   `json:"last_error"`
	CreatedAt    time.Time                 `json:"created_at"`
	UpdatedAt    time.Time                 `json:"updated_at"`
}

func connectionView(row tenantdb.TenantConnection) (ConnectionView, error) {
	c, err := tenant.ConnectionFromRow(row)
	if err != nil {
		return ConnectionView{}, err
	}
	v := ConnectionView{ID: c.ID, Kind: c.Kind, Type: c.Type, Endpoint: c.Endpoint, SecretRef: c.SecretRef, Settings: c.Settings,
		Status: c.Status, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if row.LastTestedAt.Valid {
		v.LastTestedAt = &row.LastTestedAt.Time
	}
	if row.LastError.Valid {
		v.LastError = &row.LastError.String
	}
	return v, nil
}

// connectionInput is the create/update body.
type connectionInput struct {
	Kind      string                    `json:"kind"`
	Type      string                    `json:"type"`
	Endpoint  string                    `json:"endpoint"`
	SecretRef string                    `json:"secret_ref"`
	Settings  tenant.ConnectionSettings `json:"settings"`
}

// checkConnection validates a connection body: a supported vector store type, an endpoint
// the egress policy accepts (resolved now; dials are checked again), and an existing secret.
func (s *Server) checkConnection(r *http.Request, plan tenant.Plan, in connectionInput) []platform.FieldError {
	var errs []platform.FieldError
	switch in.Kind {
	case tenant.ConnectionVectorStore:
	case tenant.ConnectionLexical:
		errs = append(errs, fieldErr("/kind", "byo_unavailable", "BYO lexical stores are not supported in v1; the lexical index is platform-hosted"))
	default:
		errs = append(errs, fieldErr("/kind", "invalid", "kind must be vector_store"))
	}
	schemes := []string{"https"}
	switch in.Type {
	case tenant.StoreQdrant, tenant.StoreMilvus:
	case tenant.StorePgvector:
		schemes = []string{"postgres", "postgresql"}
	default:
		errs = append(errs, fieldErr("/type", "unsupported", "type must be qdrant, pgvector or milvus"))
	}
	if u, err := s.BYO.Egress.CheckURL(r.Context(), in.Endpoint, plan.EgressAllowlist, schemes...); err != nil {
		errs = append(errs, fieldErr("/endpoint", "egress_denied", err.Error()))
	} else if in.Type == tenant.StorePgvector && !s.BYO.Egress.Trusted(u.Host) {
		switch u.Query().Get("sslmode") {
		case "require", "verify-ca", "verify-full":
		default:
			errs = append(errs, fieldErr("/endpoint", "tls_required", "pgvector endpoints need sslmode=require, verify-ca or verify-full"))
		}
	}
	if in.SecretRef != "" {
		if _, err := s.BYO.Secrets.Get(r.Context(), string(pathTenant(r)), in.SecretRef); err != nil {
			errs = append(errs, fieldErr("/secret_ref", "not_found", "no such secret"))
		}
	}
	return errs
}

func settingsJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func listLimit(r *http.Request) int32 {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 || n > 100 {
		return 50
	}
	return int32(n) //nolint:gosec // bounded above
}

func (s *Server) handleListConnections(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListTenantConnections(r.Context(), tenantdb.ListTenantConnectionsParams{TenantID: string(pathTenant(r)), Limit: listLimit(r)})
	if err != nil {
		s.internalError(w, err)
		return
	}
	items := make([]ConnectionView, 0, len(rows))
	for _, row := range rows {
		v, err := connectionView(row)
		if err != nil {
			s.internalError(w, err)
			return
		}
		items = append(items, v)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nil})
}

func (s *Server) handleCreateConnection(w http.ResponseWriter, r *http.Request) {
	var in connectionInput
	if !decodeOrFail(w, r, &in) {
		return
	}
	plan, ok := s.byoPlan(w, r)
	if !ok {
		return
	}
	if errs := s.checkConnection(r, plan, in); len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	id := tenant.NewID().String()
	err := s.Q.CreateTenantConnection(r.Context(), tenantdb.CreateTenantConnectionParams{
		ID: id, TenantID: string(pathTenant(r)), Kind: tenantdb.TenantConnectionsKind(in.Kind), Type: in.Type,
		Endpoint: in.Endpoint, SecretRef: nullString(in.SecretRef), Settings: settingsJSON(in.Settings),
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeConnection(w, r, id, http.StatusCreated)
}

func (s *Server) writeConnection(w http.ResponseWriter, r *http.Request, id string, status int) {
	row, err := s.Q.GetTenantConnection(r.Context(), tenantdb.GetTenantConnectionParams{TenantID: string(pathTenant(r)), ID: id})
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, http.StatusNotFound, "no such connection")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	v, err := connectionView(row)
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, status, v)
}

func (s *Server) handleGetConnection(w http.ResponseWriter, r *http.Request) {
	s.writeConnection(w, r, r.PathValue("connection_id"), http.StatusOK)
}

// liveBlock returns the vector store block of the tenant's active or shadow version that
// uses connection id, if any.
func (s *Server) liveBlock(r *http.Request, id string) (*tenant.StoreConfig, error) {
	tid := pathTenant(r)
	versions, err := s.Q.ListVersionsReferencing(r.Context(), tenantdb.ListVersionsReferencingParams{TenantID: string(tid), Ref: id})
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.State != tenantdb.TenantConfigVersionsStateActive && v.State != tenantdb.TenantConfigVersionsStateShadow {
			continue
		}
		cfg, err := s.Tenants.ConfigVersion(r.Context(), tid, int(v.Version))
		if err != nil {
			return nil, err
		}
		if b := cfg.Stores.Vector; b.Provider == tenant.ProviderBYO && b.ConnectionID == id {
			return &b, nil
		}
	}
	return nil, nil
}

// handleUpdateConnection replaces a connection. A connection serving a live (active or
// shadow) version is tested with the new settings first and saved only if the test passes,
// so a credential rotation cannot break search; others are saved as untested.
func (s *Server) handleUpdateConnection(w http.ResponseWriter, r *http.Request) {
	var in connectionInput
	if !decodeOrFail(w, r, &in) {
		return
	}
	id, tid := r.PathValue("connection_id"), pathTenant(r)
	if _, err := s.Tenants.Connection(r.Context(), tid, id); err != nil {
		problem(w, http.StatusNotFound, "no such connection")
		return
	}
	plan, ok := s.byoPlan(w, r)
	if !ok {
		return
	}
	if errs := s.checkConnection(r, plan, in); len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	live, err := s.liveBlock(r, id)
	if err != nil {
		s.internalError(w, err)
		return
	}
	candidate := tenant.Connection{ID: id, Kind: in.Kind, Type: in.Type, Endpoint: in.Endpoint, SecretRef: in.SecretRef, Settings: in.Settings}
	var result *ConnectionTestResult
	if live != nil {
		res := s.testConnection(r.Context(), tid, plan, candidate, *live, nil)
		if !res.OK {
			problem(w, http.StatusUnprocessableEntity, "the connection serves a live config version and the new settings fail its test; nothing was saved",
				fieldErr("", "connection_test_failed", res.firstError()))
			return
		}
		result = &res
	}
	n, err := s.Q.UpdateTenantConnection(r.Context(), tenantdb.UpdateTenantConnectionParams{
		Kind: tenantdb.TenantConnectionsKind(in.Kind), Type: in.Type, Endpoint: in.Endpoint, SecretRef: nullString(in.SecretRef),
		Settings: settingsJSON(in.Settings), TenantID: string(tid), ID: id,
	})
	if err != nil || n == 0 {
		s.internalError(w, fmt.Errorf("update connection %s: %w (rows %d)", id, err, n))
		return
	}
	if result != nil {
		s.recordConnectionTest(r, id, *result)
	}
	s.probes.forget(tid)
	s.writeConnection(w, r, id, http.StatusOK)
}

func (s *Server) handleDeleteConnection(w http.ResponseWriter, r *http.Request) {
	id, tid := r.PathValue("connection_id"), pathTenant(r)
	versions, err := s.Q.ListVersionsReferencing(r.Context(), tenantdb.ListVersionsReferencingParams{TenantID: string(tid), Ref: id})
	if err != nil {
		s.internalError(w, err)
		return
	}
	inDraft, err := s.Q.DraftReferences(r.Context(), tenantdb.DraftReferencesParams{TenantID: string(tid), Ref: id})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if len(versions) > 0 || inDraft > 0 {
		var used []string
		for _, v := range versions {
			used = append(used, fmt.Sprintf("v%d (%s)", v.Version, v.State))
		}
		if inDraft > 0 {
			used = append(used, "the draft")
		}
		problem(w, http.StatusConflict, fmt.Sprintf("the connection is used by %v; switch them to another store first", used),
			fieldErr("", "in_use", "connection in use"))
		return
	}
	n, err := s.Q.DeleteTenantConnection(r.Context(), tenantdb.DeleteTenantConnectionParams{TenantID: string(tid), ID: id})
	if err != nil {
		s.internalError(w, err)
		return
	}
	if n == 0 {
		problem(w, http.StatusNotFound, "no such connection")
		return
	}
	s.probes.forget(tid)
	w.WriteHeader(http.StatusNoContent)
}

// handleTestConnection runs the connection test against a collection: the body's
// {collection, vectors, payload_mapping}, or the draft's vector store block when it uses this
// connection. The result is recorded as the connection's status.
func (s *Server) handleTestConnection(w http.ResponseWriter, r *http.Request) {
	id, tid := r.PathValue("connection_id"), pathTenant(r)
	conn, err := s.Tenants.Connection(r.Context(), tid, id)
	if err != nil {
		problem(w, http.StatusNotFound, "no such connection")
		return
	}
	var body struct {
		Collection     string                       `json:"collection"`
		Vectors        map[string]tenant.VectorSpec `json:"vectors"`
		PayloadMapping map[string]string            `json:"payload_mapping"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		invalid(w, fieldErr("", "invalid_body", err.Error()))
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := strictJSON(raw, &body); err != nil {
			var se stepError
			if errors.As(err, &se) {
				invalid(w, se.errs...)
			}
			return
		}
	}
	block := tenant.StoreConfig{Provider: tenant.ProviderBYO, ConnectionID: id, Collection: body.Collection, Vectors: body.Vectors, PayloadMapping: body.PayloadMapping}
	draft, _ := s.Tenants.Draft(r.Context(), tid)
	if block.Collection == "" {
		if draft == nil || draft.Stores.Vector.ConnectionID != id {
			invalid(w, fieldErr("/collection", "required", "pass {collection, vectors}, or reference this connection from the draft's stores.vector"))
			return
		}
		block = draft.Stores.Vector
	}
	var filterFields []string
	if draft != nil {
		filterFields = draft.PushdownFields()
	}
	plan, ok := s.byoPlan(w, r)
	if !ok {
		return
	}
	res := s.testConnection(r.Context(), tid, plan, conn, block, filterFields)
	s.recordConnectionTest(r, id, res)
	s.probes.forget(tid)
	platform.WriteJSON(w, http.StatusOK, res)
}

func (s *Server) recordConnectionTest(r *http.Request, id string, res ConnectionTestResult) {
	status, lastErr := tenant.StatusValidated, sql.NullString{}
	if !res.OK {
		status, lastErr = tenant.StatusFailed, sql.NullString{String: res.firstError(), Valid: true}
	}
	if err := s.Q.SetTenantConnectionStatus(r.Context(), tenantdb.SetTenantConnectionStatusParams{
		Status: status, LastTestedAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}, LastError: lastErr,
		TenantID: string(pathTenant(r)), ID: id,
	}); err != nil {
		s.Log.Error("record connection test", "connection", id, "err", err)
	}
}

// ModelView is a registered model as the API returns it.
type ModelView struct {
	ID                  string             `json:"id"`
	Kind                string             `json:"kind"`
	Type                string             `json:"type"`
	Name                string             `json:"name"`
	Version             string             `json:"version"`
	Endpoint            string             `json:"endpoint,omitempty"`
	SecretRef           string             `json:"secret_ref,omitempty"`
	SupportsTextToImage bool               `json:"supports_text_to_image"`
	MaxBatchSize        int                `json:"max_batch_size,omitempty"`
	Transport           string             `json:"transport,omitempty"`
	InputSchema         tenant.ModelInput  `json:"input_schema"`
	OutputSchema        tenant.ModelOutput `json:"output_schema"`
	Status              string             `json:"status"`
	ValidatedAt         *time.Time         `json:"validated_at"`
	LastError           *string            `json:"last_error"`
	CreatedAt           time.Time          `json:"created_at"`
}

func modelView(row tenantdb.TenantModel) (ModelView, error) {
	m, err := tenant.ModelFromRow(row)
	if err != nil {
		return ModelView{}, err
	}
	v := ModelView{ID: m.ID, Kind: m.Kind, Type: m.Type, Name: m.Name, Version: m.Version, Endpoint: m.Endpoint, SecretRef: m.SecretRef,
		SupportsTextToImage: m.Settings.SupportsTextToImage, MaxBatchSize: m.Settings.MaxBatchSize, Transport: m.Settings.Transport,
		InputSchema: m.Input, OutputSchema: m.Output, Status: m.Status, CreatedAt: row.CreatedAt}
	if row.ValidatedAt.Valid {
		v.ValidatedAt = &row.ValidatedAt.Time
	}
	if row.LastError.Valid {
		v.LastError = &row.LastError.String
	}
	return v, nil
}

// MaxEmbeddingDim bounds a declared embedding dimension.
const MaxEmbeddingDim = 8192

func (s *Server) handleRegisterModel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind                string             `json:"kind"`
		Type                string             `json:"type"`
		Name                string             `json:"name"`
		Version             string             `json:"version"`
		Endpoint            string             `json:"endpoint"`
		ArtifactURI         string             `json:"artifact_uri"`
		SecretRef           string             `json:"secret_ref"`
		SupportsTextToImage bool               `json:"supports_text_to_image"`
		MaxBatchSize        int                `json:"max_batch_size"`
		InputSchema         tenant.ModelInput  `json:"input_schema"`
		OutputSchema        tenant.ModelOutput `json:"output_schema"`
	}
	if !decodeOrFail(w, r, &in) {
		return
	}
	var errs []platform.FieldError
	if in.Kind != "embedder" {
		errs = append(errs, fieldErr("/kind", "not_available", "prerank, rank and rerank models arrive with learned ranking (M7); register embedders"))
	}
	switch in.Type {
	case tenant.ModelKServeV2, tenant.ModelOpenAIEmbeddings:
	case tenant.ModelONNX:
		errs = append(errs, fieldErr("/type", "not_available", "ONNX uploads arrive with learned ranking (M7)"))
	default:
		errs = append(errs, fieldErr("/type", "invalid", "type must be kserve_v2 or openai_embeddings"))
	}
	if in.Name == "" || len(in.Name) > 255 {
		errs = append(errs, fieldErr("/name", "invalid", "name must be 1 to 255 characters (the KServe model name, or the embeddings `model`)"))
	}
	if len(in.Version) > 64 {
		errs = append(errs, fieldErr("/version", "invalid", "version must be at most 64 characters"))
	}
	if in.ArtifactURI != "" {
		errs = append(errs, fieldErr("/artifact_uri", "not_available", "artifacts are for ONNX models (M7)"))
	}
	switch in.InputSchema.Modality {
	case tenant.ModalityText:
		if in.SupportsTextToImage {
			errs = append(errs, fieldErr("/supports_text_to_image", "invalid", "only image embedders can have a text tower"))
		}
	case tenant.ModalityImage:
		if in.Type == tenant.ModelOpenAIEmbeddings {
			errs = append(errs, fieldErr("/input_schema/modality", "unsupported", "openai_embeddings models embed text only"))
		}
	default:
		errs = append(errs, fieldErr("/input_schema/modality", "invalid", "modality must be text or image"))
	}
	if in.OutputSchema.Dim <= 0 || in.OutputSchema.Dim > MaxEmbeddingDim {
		errs = append(errs, fieldErr("/output_schema/dim", "out_of_range", fmt.Sprintf("dim must be within [1, %d]", MaxEmbeddingDim)))
	}
	if in.MaxBatchSize < 0 || in.MaxBatchSize > 1024 {
		errs = append(errs, fieldErr("/max_batch_size", "out_of_range", "max_batch_size must be within [0, 1024] (0 uses the default)"))
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	plan, ok := s.byoPlan(w, r)
	if !ok {
		return
	}
	if _, err := s.BYO.Egress.CheckURL(r.Context(), in.Endpoint, plan.EgressAllowlist); err != nil {
		errs = append(errs, fieldErr("/endpoint", "egress_denied", err.Error()))
	}
	if in.SecretRef != "" {
		if _, err := s.BYO.Secrets.Get(r.Context(), string(pathTenant(r)), in.SecretRef); err != nil {
			errs = append(errs, fieldErr("/secret_ref", "not_found", "no such secret"))
		}
	}
	if len(errs) > 0 {
		invalid(w, errs...)
		return
	}
	id := tenant.NewID().String()
	err := s.Q.CreateTenantModel(r.Context(), tenantdb.CreateTenantModelParams{
		ID: id, TenantID: string(pathTenant(r)), Kind: tenantdb.TenantModelsKind(in.Kind), Type: tenantdb.TenantModelsType(in.Type),
		Name: in.Name, Version: in.Version, Endpoint: nullString(in.Endpoint), SecretRef: nullString(in.SecretRef),
		InputSchema: settingsJSON(in.InputSchema), OutputSchema: settingsJSON(in.OutputSchema),
		Settings: settingsJSON(tenant.ModelSettings{MaxBatchSize: in.MaxBatchSize, SupportsTextToImage: in.SupportsTextToImage}),
	})
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeModel(w, r, id, http.StatusCreated)
}

func (s *Server) writeModel(w http.ResponseWriter, r *http.Request, id string, status int) {
	row, err := s.Q.GetTenantModel(r.Context(), tenantdb.GetTenantModelParams{TenantID: string(pathTenant(r)), ID: id})
	if errors.Is(err, sql.ErrNoRows) {
		problem(w, http.StatusNotFound, "no such model")
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	v, err := modelView(row)
	if err != nil {
		s.internalError(w, err)
		return
	}
	platform.WriteJSON(w, status, v)
}

func (s *Server) handleGetModel(w http.ResponseWriter, r *http.Request) {
	s.writeModel(w, r, r.PathValue("model_id"), http.StatusOK)
}

func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Q.ListTenantModels(r.Context(), tenantdb.ListTenantModelsParams{TenantID: string(pathTenant(r)), Limit: listLimit(r)})
	if err != nil {
		s.internalError(w, err)
		return
	}
	items := make([]ModelView, 0, len(rows))
	for _, row := range rows {
		v, err := modelView(row)
		if err != nil {
			s.internalError(w, err)
			return
		}
		items = append(items, v)
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// handleValidateModel runs probe inferences and records the outcome as the model's status;
// for kserve_v2 it also records the transport that worked (gRPC, else REST).
func (s *Server) handleValidateModel(w http.ResponseWriter, r *http.Request) {
	id, tid := r.PathValue("model_id"), pathTenant(r)
	m, err := s.Tenants.Model(r.Context(), tid, id)
	if err != nil {
		problem(w, http.StatusNotFound, "no such model")
		return
	}
	plan, ok := s.byoPlan(w, r)
	if !ok {
		return
	}
	res, settings := s.validateModel(r.Context(), tid, plan, m)
	status, lastErr, validatedAt := tenant.StatusValidated, sql.NullString{}, sql.NullTime{Time: time.Now().UTC(), Valid: true}
	if !res.OK {
		status, lastErr, validatedAt = tenant.StatusFailed, sql.NullString{String: res.firstError(), Valid: true}, sql.NullTime{}
	}
	if err := s.Q.SetTenantModelValidation(r.Context(), tenantdb.SetTenantModelValidationParams{
		Status: status, Settings: settingsJSON(settings), ValidatedAt: validatedAt, LastError: lastErr, TenantID: string(tid), ID: id,
	}); err != nil {
		s.internalError(w, err)
		return
	}
	s.probes.forget(tid)
	platform.WriteJSON(w, http.StatusOK, res)
}

// PlatformModelView describes a platform model tenants can select.
type PlatformModelView struct {
	ID                  string `json:"id"`
	Kind                string `json:"kind"`
	Modality            string `json:"modality"`
	Dim                 int    `json:"dim"`
	SupportsTextToImage bool   `json:"supports_text_to_image"`
	Description         string `json:"description"`
}

func (s *Server) handlePlatformModels(w http.ResponseWriter, _ *http.Request) {
	items := []PlatformModelView{
		{ID: "bge-small-en-v1.5", Kind: "embedder", Modality: tenant.ModalityText, Dim: 384,
			Description: "BAAI/bge-small-en-v1.5 (MIT): English text embeddings for semantic retrieval."},
		{ID: "clip-vit-b32", Kind: "embedder", Modality: tenant.ModalityImage, Dim: 512, SupportsTextToImage: true,
			Description: "laion/CLIP-ViT-B-32-laion2B-s34B-b79K (MIT): image embeddings, plus text-to-image search through its text tower."},
	}
	for _, m := range items {
		if s.PlatformEmbedder != nil {
			if info, ok := s.PlatformEmbedder(m.ID); !ok || info.Dim != m.Dim {
				s.Log.Warn("platform model catalog out of step with the registry", "model", m.ID)
			}
		}
	}
	platform.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// endpointHost returns the host of an endpoint URL, for messages.
func endpointHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}
