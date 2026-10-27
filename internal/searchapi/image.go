package searchapi

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"

	"github.com/zavisoft/search-engine/internal/auth"
	"github.com/zavisoft/search-engine/internal/catalog"
	"github.com/zavisoft/search-engine/internal/imaging"
	"github.com/zavisoft/search-engine/internal/pipeline"
	"github.com/zavisoft/search-engine/internal/pipeline/filter"
	"github.com/zavisoft/search-engine/internal/platform"
	"github.com/zavisoft/search-engine/internal/stages/queryunderstanding"
)

// MaxImageRequestBytes bounds a whole image-search request: the image plus the JSON parts.
const MaxImageRequestBytes = imaging.MaxBytes + 1<<20

// ImageSearchResponse adds the predicted categories to a search response.
type ImageSearchResponse struct {
	*SearchResponse
	PredictedCategories []pipeline.CategoryGuess `json:"predicted_categories"`
}

// imageRequest is the parsed multipart form of POST /v1/search/image.
type imageRequest struct {
	image   []byte
	crop    *imaging.Crop
	q       string
	filters *filter.Expr
	user    pipeline.UserContext
	page    PageRequest
	explain bool
}

func (s *Service) handleImageSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _ := auth.FromContext(ctx)
	req, status, errs := parseImageRequest(w, r)
	if status != 0 {
		platform.WriteProblem(w, platform.Problem{Status: status, Detail: "the image upload is invalid", Errors: errs})
		return
	}
	prepared, _, err := imaging.Prepare(req.image, req.crop)
	if err != nil {
		status, path := http.StatusBadRequest, "/image"
		switch {
		case errors.Is(err, imaging.ErrTooLarge):
			status = http.StatusRequestEntityTooLarge
		case errors.Is(err, imaging.ErrUnsupported):
			status = http.StatusUnsupportedMediaType
		case errors.Is(err, imaging.ErrCrop):
			path = "/crop"
		}
		platform.WriteProblem(w, platform.Problem{Status: status, Detail: err.Error(),
			Errors: []platform.FieldError{{Path: path, Code: "invalid_image", Message: err.Error()}}})
		return
	}

	res, ok := s.resolve(ctx, w, r, p)
	if !ok {
		return
	}
	reqFilter, ok := compileFilter(w, req.filters, res)
	if !ok {
		return
	}
	q := &pipeline.Query{
		Tenant: p.Tenant, ConfigVersion: res.ConfigVersion, Type: pipeline.QueryImage, Raw: req.q, ImageBytes: prepared,
		Filter: reqFilter, BaseFilter: res.AlwaysFilters, User: req.user,
	}
	if strings.TrimSpace(req.q) != "" {
		q.Facets = facetRequests(res.Config)
	}
	sum := sha256.Sum256(prepared)
	resp, ok := s.servePaged(ctx, w, p, res, q, req.page, req.explain,
		pipeline.QueryImage, hex.EncodeToString(sum[:]), req.crop, queryunderstanding.Normalize(req.q), reqFilter, req.user.ID)
	if !ok {
		return
	}
	cats := resp.Query.Categories
	if cats == nil {
		cats = []pipeline.CategoryGuess{}
	}
	platform.WriteJSON(w, http.StatusOK, ImageSearchResponse{SearchResponse: resp, PredictedCategories: cats})
}

// parseImageRequest reads the multipart form. JSON parts (crop, filters, user, page) may be sent
// as plain fields or as file parts. A non-zero status means the request was rejected.
func parseImageRequest(w http.ResponseWriter, r *http.Request) (imageRequest, int, []platform.FieldError) {
	var req imageRequest
	fail := func(status int, path, code, msg string) (imageRequest, int, []platform.FieldError) {
		return req, status, []platform.FieldError{{Path: path, Code: code, Message: msg}}
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxImageRequestBytes)
	if err := r.ParseMultipartForm(MaxImageRequestBytes); err != nil { //nolint:gosec // body bounded by MaxBytesReader above
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return fail(http.StatusRequestEntityTooLarge, "/image", "too_large", "the request exceeds 11 MB")
		}
		return fail(http.StatusBadRequest, "", "invalid_multipart", "expected multipart/form-data: "+err.Error())
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()

	part := func(name string) ([]byte, bool, error) {
		if fhs := r.MultipartForm.File[name]; len(fhs) > 0 {
			f, err := fhs[0].Open()
			if err != nil {
				return nil, true, fmt.Errorf("open %s: %w", name, err)
			}
			defer f.Close()
			b, err := io.ReadAll(io.LimitReader(f, imaging.MaxBytes+1))
			if err != nil {
				return nil, true, fmt.Errorf("read %s: %w", name, err)
			}
			return b, true, nil
		}
		if vs := r.MultipartForm.Value[name]; len(vs) > 0 {
			return []byte(vs[0]), true, nil
		}
		return nil, false, nil
	}
	jsonPart := func(name string, dst any) error {
		b, ok, err := part(name)
		if err != nil || !ok || len(bytes.TrimSpace(b)) == 0 {
			return err
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		return dec.Decode(dst)
	}

	img, ok, err := part("image")
	if err != nil || !ok || len(img) == 0 {
		return fail(http.StatusBadRequest, "/image", "required", "an image part is required")
	}
	req.image = img
	var crop imaging.Crop
	if b, ok, _ := part("crop"); ok && len(bytes.TrimSpace(b)) > 0 {
		if err := jsonPart("crop", &crop); err != nil {
			return fail(http.StatusBadRequest, "/crop", "invalid_json", err.Error())
		}
		req.crop = &crop
	}
	var f filter.Expr
	if b, ok, _ := part("filters"); ok && len(bytes.TrimSpace(b)) > 0 {
		if err := jsonPart("filters", &f); err != nil {
			return fail(http.StatusBadRequest, "/filters", "invalid_json", err.Error())
		}
		req.filters = &f
	}
	if err := jsonPart("user", &req.user); err != nil {
		return fail(http.StatusBadRequest, "/user", "invalid_json", err.Error())
	}
	if err := jsonPart("page", &req.page); err != nil {
		return fail(http.StatusBadRequest, "/page", "invalid_json", err.Error())
	}
	if b, ok, _ := part("q"); ok {
		req.q = string(b)
	}
	if utf8.RuneCountInString(req.q) > MaxQueryLength {
		return fail(http.StatusBadRequest, "/q", "too_long", fmt.Sprintf("q is at most %d characters", MaxQueryLength))
	}
	if b, ok, _ := part("explain"); ok {
		v, err := strconv.ParseBool(strings.TrimSpace(string(b)))
		if err != nil {
			return fail(http.StatusBadRequest, "/explain", "invalid", "explain must be true or false")
		}
		req.explain = v
	}
	if errs := validatePage(&req.page); len(errs) > 0 {
		return req, http.StatusBadRequest, errs
	}
	return req, 0, nil
}

// handleSimilar serves GET /v1/products/{product_id}/similar: vector_image and vector_text on
// the product's stored vectors, excluding the product itself.
func (s *Service) handleSimilar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, _ := auth.FromContext(ctx)
	id := r.PathValue("product_id")
	size := DefaultPageSize
	if v := r.URL.Query().Get("size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > MaxPageSize {
			badRequest(w, platform.FieldError{Path: "/size", Code: "out_of_range", Message: fmt.Sprintf("size must be within [1, %d]", MaxPageSize)})
			return
		}
		size = n
	}
	res, ok := s.resolve(ctx, w, r, p)
	if !ok {
		return
	}
	providers, ok := s.providers(ctx, w, res)
	if !ok {
		return
	}
	anchor, doc, err := providers.Vectors.Get(ctx, id)
	if errors.Is(err, pipeline.ErrNotFound) {
		platform.WriteProblem(w, platform.Problem{Status: http.StatusNotFound, Detail: "product " + id + " is not indexed"})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	reqID := ulid.Make().String()
	q := &pipeline.Query{
		RequestID: reqID, Tenant: p.Tenant, ConfigVersion: res.ConfigVersion, Type: pipeline.QuerySimilar, AnchorID: id,
		ImageVector: anchor.Vectors[pipeline.VectorImage], TextVector: anchor.Vectors[pipeline.VectorText],
		Filter:     filter.Expr{Cond: &filter.Condition{Field: "product_id", Op: filter.Ne, Value: id, Type: catalog.TypeKeyword}},
		BaseFilter: res.AlwaysFilters, Depth: size,
	}
	if doc != nil && len(doc.CategoryIDs) > 0 {
		q.Categories = []pipeline.CategoryGuess{{ID: doc.CategoryIDs[len(doc.CategoryIDs)-1], Confidence: 1}}
	}
	st, _ := s.Engine.Run(ctx, res, providers, q)
	resp, err := s.page(ctx, res, snapshotOf(res.ConfigVersion, st), "", 0, size)
	if err != nil {
		s.fail(w, err)
		return
	}
	resp.RequestID = reqID
	s.emitServed(p.Tenant, resp, q)
	platform.WriteJSON(w, http.StatusOK, resp)
}
