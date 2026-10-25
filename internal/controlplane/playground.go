package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/zavisoft/search-engine/internal/auth"
)

// HeaderConfigVersion pins a playground search to a config version number or "draft".
const HeaderConfigVersion = "X-Config-Version"

func (s *Server) playgroundRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/tenants/{tid}/playground/search", s.tenantRoute(auth.ScopeTenantRead, s.handlePlayground))
	mux.Handle("POST /v1/tenants/{tid}/playground/search/image", s.tenantRoute(auth.ScopeTenantRead, s.handlePlaygroundImage))
}

// versionRef validates a playground version reference ("draft" or a positive integer).
func versionRef(ref string) (string, bool) {
	if ref == "" || ref == "draft" {
		return "draft", true
	}
	if v, err := strconv.Atoi(ref); err == nil && v > 0 {
		return ref, true
	}
	return "", false
}

// proxy forwards a playground request to search-api with a short-lived token that carries
// search:playground, which makes search-api honour the version header and force explain.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, path, contentType string, body io.Reader, ref string) {
	token, err := s.Keys.IssueClaims(auth.Claims{
		TenantID: r.PathValue("tid"), Scope: auth.ScopeSearchRead + " " + auth.ScopeSearchDebug + " " + auth.ScopeSearchPlayground,
		RegisteredClaims: jwt.RegisteredClaims{Subject: "playground:" + actor(r)},
	}, time.Minute)
	if err != nil {
		s.internalError(w, err)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.SearchURL+path, body)
	if err != nil {
		s.internalError(w, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set(HeaderConfigVersion, ref)
	resp, err := s.HTTPClient.Do(req)
	if err != nil {
		problem(w, http.StatusBadGateway, "search-api is unavailable")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
}

func (s *Server) handlePlayground(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if !decodeOrFail(w, r, &raw) {
		return
	}
	ref := "draft"
	if v, ok := raw["config_version"]; ok {
		var x any
		_ = json.Unmarshal(v, &x)
		switch t := x.(type) {
		case string:
			ref = t
		case float64:
			ref = strconv.Itoa(int(t))
		}
		delete(raw, "config_version")
	}
	ref, ok := versionRef(ref)
	if !ok {
		invalid(w, fieldErr("/config_version", "invalid", "a version number or draft"))
		return
	}
	body, err := json.Marshal(raw)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.proxy(w, r, "/v1/search", "application/json", bytes.NewReader(body), ref)
}

func (s *Server) handlePlaygroundImage(w http.ResponseWriter, r *http.Request) {
	ref, ok := versionRef(r.URL.Query().Get("config_version"))
	if !ok {
		invalid(w, fieldErr("/config_version", "invalid", "a version number or draft"))
		return
	}
	s.proxy(w, r, "/v1/search/image", r.Header.Get("Content-Type"), http.MaxBytesReader(w, r.Body, 11<<20), ref)
}
