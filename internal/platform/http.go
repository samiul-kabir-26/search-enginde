package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/trace"
)

// Check reports whether a dependency is usable.
type Check func(ctx context.Context) error

// Healthz is the liveness handler: the process is up and serving HTTP.
func Healthz(w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Readyz returns a readiness handler that runs every check concurrently,
// each bounded by timeout, and answers 503 listing the failing checks.
func Readyz(checks map[string]Check, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		var (
			mu     sync.Mutex
			wg     sync.WaitGroup
			failed []FieldError
		)
		for name, check := range checks {
			wg.Go(func() {
				if err := check(ctx); err != nil {
					mu.Lock()
					failed = append(failed, FieldError{Path: "/" + name, Code: "unavailable", Message: err.Error()})
					mu.Unlock()
				}
			})
		}
		wg.Wait()

		if len(failed) > 0 {
			sort.Slice(failed, func(i, j int) bool { return failed[i].Path < failed[j].Path })
			WriteProblem(w, Problem{Status: http.StatusServiceUnavailable, Detail: "dependencies not ready", Errors: failed})
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
}

// Instrument wraps h with OpenTelemetry tracing and a structured access log.
func Instrument(service string, log *slog.Logger, h http.Handler) http.Handler {
	return otelhttp.NewHandler(accessLog(log, h), service)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err //nolint:wrapcheck // transparent ResponseWriter passthrough
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func accessLog(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		attrs := []any{
			"method", r.Method,
			"route", r.Pattern,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if sc := trace.SpanContextFromContext(r.Context()); sc.HasTraceID() {
			attrs = append(attrs, "trace_id", sc.TraceID().String())
		}
		log.InfoContext(r.Context(), "http_request", attrs...)
	})
}

// Serve runs srv until ctx is cancelled, then shuts it down gracefully.
func Serve(ctx context.Context, srv *http.Server, shutdownTimeout time.Duration, log *slog.Logger) error {
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}
