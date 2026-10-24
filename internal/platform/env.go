// Package platform holds process-level plumbing shared by every binary:
// env config loading, structured logging, telemetry, HTTP serving and
// RFC 9457 problem+json errors.
package platform

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Env reads typed configuration from environment variables. It accumulates
// errors so a binary can report every misconfiguration at once and fail fast.
type Env struct {
	lookup func(string) (string, bool)
	errs   []error
}

// NewEnv returns an Env backed by the process environment.
func NewEnv() *Env {
	return NewEnvFrom(os.LookupEnv)
}

// NewEnvFrom returns an Env backed by an arbitrary lookup function.
func NewEnvFrom(lookup func(string) (string, bool)) *Env {
	return &Env{lookup: lookup}
}

func (e *Env) get(key string) (string, bool) {
	v, ok := e.lookup(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// String returns the value of key, or def when unset or empty.
func (e *Env) String(key, def string) string {
	if v, ok := e.get(key); ok {
		return v
	}
	return def
}

// Required returns the value of key and records an error when unset or empty.
func (e *Env) Required(key string) string {
	v, ok := e.get(key)
	if !ok {
		e.errs = append(e.errs, fmt.Errorf("%s is required", key))
	}
	return v
}

// Int returns key parsed as an int, or def when unset.
func (e *Env) Int(key string, def int) int {
	v, ok := e.get(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return n
}

// Bool returns key parsed with strconv.ParseBool, or def when unset.
func (e *Env) Bool(key string, def bool) bool {
	v, ok := e.get(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return b
}

// Duration returns key parsed with time.ParseDuration, or def when unset.
func (e *Env) Duration(key string, def time.Duration) time.Duration {
	v, ok := e.get(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", key, err))
		return def
	}
	return d
}

// Err returns all accumulated errors joined, or nil.
func (e *Env) Err() error {
	return errors.Join(e.errs...)
}
