package indexer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxImageBytes matches the image upload limit (docs/API.md).
const MaxImageBytes = 10 << 20

// HTTPFetcher downloads images over HTTP with a size cap.
//
// Product image URLs come from tenant catalogs, so from M5 this must go through
// security.EgressClient (SSRF-safe dialer plus allowlist) with the platform's own image host
// allowed; until then it is only used with platform-hosted demo images.
type HTTPFetcher struct {
	Client *http.Client
}

// NewHTTPFetcher returns a fetcher with a per-request timeout.
func NewHTTPFetcher(timeout time.Duration) *HTTPFetcher {
	return &HTTPFetcher{Client: &http.Client{Timeout: timeout}}
}

// Fetch returns the body of url, failing on non-200 responses and bodies over MaxImageBytes.
func (f *HTTPFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("image request: %w", err)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if len(b) > MaxImageBytes {
		return nil, fmt.Errorf("GET %s: image exceeds %d bytes", url, MaxImageBytes)
	}
	return b, nil
}
