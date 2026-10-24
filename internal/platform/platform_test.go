package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEnv(t *testing.T) {
	vars := map[string]string{
		"S":   "value",
		"I":   "42",
		"B":   "true",
		"D":   "250ms",
		"BAD": "nope",
	}
	env := NewEnvFrom(func(k string) (string, bool) { v, ok := vars[k]; return v, ok })

	if got := env.String("S", "x"); got != "value" {
		t.Errorf("String = %q", got)
	}
	if got := env.String("MISSING", "x"); got != "x" {
		t.Errorf("String default = %q", got)
	}
	if got := env.Int("I", 0); got != 42 {
		t.Errorf("Int = %d", got)
	}
	if got := env.Bool("B", false); !got {
		t.Errorf("Bool = %v", got)
	}
	if got := env.Duration("D", 0); got != 250*time.Millisecond {
		t.Errorf("Duration = %v", got)
	}
	if err := env.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	env.Required("MISSING")
	env.Int("BAD", 1)
	env.Duration("BAD", 0)
	err := env.Err()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"MISSING is required", "BAD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestNewLogger(t *testing.T) {
	var buf bytes.Buffer
	log, err := NewLogger(&buf, "svc", "warn")
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("want exactly one JSON record, got %q: %v", buf.String(), err)
	}
	if rec["service"] != "svc" || rec["msg"] != "shown" {
		t.Errorf("record = %v", rec)
	}

	if _, err := NewLogger(&buf, "svc", "loud"); err == nil {
		t.Error("expected error for invalid level")
	}
}

func TestWriteProblem(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteProblem(rec, Problem{Status: http.StatusBadRequest, Errors: []FieldError{{Path: "/q", Code: "required", Message: "q is required"}}})

	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var p Problem
	if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Status != 400 || p.Type != "about:blank" || p.Title != "Bad Request" || len(p.Errors) != 1 {
		t.Errorf("problem = %+v", p)
	}
}

func TestReadyz(t *testing.T) {
	ok := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("connection refused") }

	tests := []struct {
		name       string
		checks     map[string]Check
		wantStatus int
		wantPaths  []string
	}{
		{"all ready", map[string]Check{"mysql": ok, "redis": ok}, http.StatusOK, nil},
		{"one down", map[string]Check{"mysql": ok, "redis": down}, http.StatusServiceUnavailable, []string{"/redis"}},
		{"all down sorted", map[string]Check{"redis": down, "mysql": down}, http.StatusServiceUnavailable, []string{"/mysql", "/redis"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Readyz(tt.checks, time.Second).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/readyz", nil))
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantPaths == nil {
				return
			}
			var p Problem
			if err := json.NewDecoder(rec.Body).Decode(&p); err != nil {
				t.Fatal(err)
			}
			if len(p.Errors) != len(tt.wantPaths) {
				t.Fatalf("errors = %+v", p.Errors)
			}
			for i, want := range tt.wantPaths {
				if p.Errors[i].Path != want {
					t.Errorf("errors[%d].Path = %q, want %q", i, p.Errors[i].Path, want)
				}
			}
		})
	}
}

func TestInstrumentLogsRequest(t *testing.T) {
	var buf bytes.Buffer
	log, _ := NewLogger(&buf, "svc", "info")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })

	rec := httptest.NewRecorder()
	Instrument("svc", log, mux).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/items/7", nil))

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("access log not JSON: %q", buf.String())
	}
	if entry["route"] != "GET /items/{id}" || entry["status"] != float64(http.StatusTeapot) {
		t.Errorf("access log = %v", entry)
	}
}
