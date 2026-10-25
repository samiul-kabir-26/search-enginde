package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// Idempotency replays responses of create requests that carry an Idempotency-Key header
// (docs/API.md; PLAN.md R8): keys live in Redis as idem:{scope}:{key} for 24 h.
type Idempotency struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewIdempotency returns the middleware's store.
func NewIdempotency(rdb *redis.Client) *Idempotency {
	return &Idempotency{rdb: rdb, ttl: 24 * time.Hour}
}

// MaxIdempotencyKey bounds header length.
const MaxIdempotencyKey = 255

type storedResponse struct {
	Pending     bool   `json:"pending,omitempty"`
	RequestHash string `json:"hash"`
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Body        []byte `json:"body,omitempty"`
}

type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.buf.Write(b)
	n, err := r.ResponseWriter.Write(b)
	if err != nil {
		return n, errors.Join(err)
	}
	return n, nil
}

// Wrap applies idempotency to h. scope names the key space (the tenant, or "signup"). A key
// reused with a different body is a 422; a key whose first request is still running is a 409;
// 5xx responses are not stored, so the client may retry.
func (i *Idempotency) Wrap(scope func(*http.Request) string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			h.ServeHTTP(w, r)
			return
		}
		if len(key) > MaxIdempotencyKey {
			invalid(w, fieldErr("/headers/Idempotency-Key", "too_long", "at most 255 characters"))
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			invalid(w, fieldErr("", "invalid_body", err.Error()))
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), body...))
		hash := hex.EncodeToString(sum[:])
		rk := "idem:{" + scope(r) + "}:" + key
		ctx := r.Context()

		pending, _ := json.Marshal(storedResponse{Pending: true, RequestHash: hash})
		ok, err := i.rdb.SetNX(ctx, rk, pending, time.Minute).Result()
		if err != nil {
			h.ServeHTTP(w, r)
			return
		}
		if !ok {
			raw, err := i.rdb.Get(ctx, rk).Bytes()
			var prev storedResponse
			if err != nil || json.Unmarshal(raw, &prev) != nil {
				problem(w, http.StatusConflict, "the first request with this Idempotency-Key is still running")
				return
			}
			switch {
			case prev.RequestHash != hash:
				problem(w, http.StatusUnprocessableEntity, "this Idempotency-Key was used with a different request")
			case prev.Pending:
				problem(w, http.StatusConflict, "the first request with this Idempotency-Key is still running")
			default:
				if prev.ContentType != "" {
					w.Header().Set("Content-Type", prev.ContentType)
				}
				w.Header().Set("Idempotent-Replayed", "true")
				w.WriteHeader(prev.Status)
				_, _ = w.Write(prev.Body)
			}
			return
		}

		rec := &recorder{ResponseWriter: w}
		h.ServeHTTP(rec, r)
		if rec.status >= 500 || rec.status == 0 {
			_ = i.rdb.Del(context.WithoutCancel(ctx), rk).Err()
			return
		}
		done, _ := json.Marshal(storedResponse{RequestHash: hash, Status: rec.status, ContentType: rec.Header().Get("Content-Type"), Body: rec.buf.Bytes()})
		_ = i.rdb.Set(context.WithoutCancel(ctx), rk, done, i.ttl).Err()
	})
}
