package platform

import (
	"fmt"
	"io"
	"log/slog"
)

// NewLogger returns a JSON slog logger tagged with the service name.
// level is one of debug, info, warn, error.
func NewLogger(w io.Writer, service, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(h).With("service", service), nil
}
