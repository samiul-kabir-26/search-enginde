package indexer

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/zavisoft/search-engine/internal/security"
)

// MaxImageBytes matches the image upload limit (docs/API.md).
const MaxImageBytes = 10 << 20

// HTTPFetcher downloads images with a size cap. Product image URLs come from tenant catalogs,
// so the client is the egress client: https only and no private, loopback, link-local or
// metadata addresses, except the platform's own (trusted) image host.
type HTTPFetcher struct {
	Client *http.Client
}

// NewHTTPFetcher returns a fetcher over egress with a per-request timeout.
func NewHTTPFetcher(egress *security.Egress, timeout time.Duration) *HTTPFetcher {
	return &HTTPFetcher{Client: egress.HTTPClient(nil, timeout)}
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
