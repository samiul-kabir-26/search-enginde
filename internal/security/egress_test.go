package security

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	for addr, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "172.31.255.255": true, "192.168.1.1": true,
		"127.0.0.1": true, "127.8.8.8": true, "169.254.169.254": true, "100.100.100.200": true, "0.0.0.0": true,
		"224.0.0.1": true, "255.255.255.255": true, "198.18.0.1": true,
		"::1": true, "::": true, "fc00::1": true, "fd00:ec2::254": true, "fe80::1": true, "ff02::1": true,
		"::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true, "64:ff9b::a00:1": true, "2002:a00:1::": true,
		"8.8.8.8": false, "1.1.1.1": false, "172.32.0.1": false, "2606:4700:4700::1111": false, "::ffff:8.8.8.8": false,
	} {
		if got := Blocked(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Blocked(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	allow := []string{"api.example.com", "*.vendor.io"}
	cases := map[string]bool{
		"api.example.com": true, "API.example.com.": true, "x.vendor.io": true, "a.b.vendor.io": true,
		"vendor.io": false, "example.com": false, "evil-vendor.io": false, "api.example.com.evil.io": false,
	}
	for host, want := range cases {
		if got := HostAllowed(host, allow); got != want {
			t.Errorf("HostAllowed(%s) = %v, want %v", host, got, want)
		}
	}
	if !HostAllowed("anything.net", nil) {
		t.Error("an empty allowlist allows every host")
	}
}

func TestCheckURL(t *testing.T) {
	e := NewEgress(EgressConfig{Trusted: []string{"s3:9100"}})
	ctx := context.Background()
	for _, tc := range []struct {
		url     string
		allow   []string
		schemes []string
		ok      bool
	}{
		{url: "https://127.0.0.1/v1", ok: false},
		{url: "https://localhost:8443", ok: false},
		{url: "https://169.254.169.254/latest/meta-data", ok: false},
		{url: "https://[::1]:443", ok: false},
		{url: "http://8.8.8.8/", ok: false},
		{url: "https://8.8.8.8/", ok: true},
		{url: "https://8.8.8.8/", allow: []string{"api.example.com"}, ok: false},
		{url: "https://user:pw@8.8.8.8/", ok: false},
		{url: "postgres://search@8.8.8.8:5432/db", schemes: []string{"postgres", "postgresql"}, ok: true},
		{url: "postgres://search:pw@8.8.8.8:5432/db", schemes: []string{"postgres"}, ok: false},
		{url: "postgres://10.0.0.5:5432/db", schemes: []string{"postgres"}, ok: false},
		{url: "http://s3:9100/bucket/a.jpg", ok: true},
		{url: "not a url", ok: false},
	} {
		_, err := e.CheckURL(ctx, tc.url, tc.allow, tc.schemes...)
		if (err == nil) != tc.ok {
			t.Errorf("CheckURL(%s, %v) = %v, want ok=%v", tc.url, tc.allow, err, tc.ok)
		}
		if err != nil && !tc.ok && !errors.Is(err, ErrEgressDenied) && !strings.Contains(err.Error(), "resolve") {
			t.Errorf("CheckURL(%s): error %v should wrap ErrEgressDenied", tc.url, err)
		}
	}
}

func TestHTTPClientRefusesPrivateAddressesAfterResolution(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())

	c := NewEgress(EgressConfig{}).HTTPClient(nil, 2*time.Second)
	// "localhost" passes every name-based check; only the dial-time address check stops it.
	for _, u := range []string{srv.URL, "https://localhost:" + port + "/"} {
		if err := get(c, u); !errors.Is(err, ErrEgressDenied) {
			t.Errorf("GET %s: %v, want ErrEgressDenied", u, err)
		}
	}
	if err := get(c, "http://8.8.8.8/"); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("plain http must be refused: %v", err)
	}
	allowOnly := NewEgress(EgressConfig{}).HTTPClient([]string{"api.example.com"}, time.Second)
	if err := get(allowOnly, "https://8.8.8.8/"); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("hosts outside the allowlist must be refused: %v", err)
	}
}

// get issues a GET and closes the body, returning only the error.
func get(c *http.Client, u string) error {
	resp, err := do(c, u)
	if err == nil {
		_ = resp.Body.Close()
	}
	return err
}

func do(c *http.Client, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

func TestTrustedHostsAndRedirects(t *testing.T) {
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	target = "http://169.254.169.254/latest/meta-data"
	host := strings.TrimPrefix(srv.URL, "http://")

	c := NewEgress(EgressConfig{Trusted: []string{host}}).HTTPClient(nil, 2*time.Second)
	resp, err := do(c, srv.URL+"/")
	if err != nil {
		t.Fatalf("trusted platform host: %v", err)
	}
	_ = resp.Body.Close()
	resp, err = do(c, srv.URL+"/redirect")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Errorf("redirects must not be followed: status %d", resp.StatusCode)
	}
}

func TestDialContext(t *testing.T) {
	var lc net.ListenConfig
	lis, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	e := NewEgress(EgressConfig{Trusted: []string{lis.Addr().String()}})
	ctx := context.Background()
	if c, err := e.DialContext(nil)(ctx, "tcp", lis.Addr().String()); err != nil {
		t.Errorf("trusted dial: %v", err)
	} else {
		_ = c.Close()
	}
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	if _, err := e.GRPCDialer(nil)(ctx, "localhost:"+port); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("loopback via name: %v", err)
	}
	if _, err := e.DialContext([]string{"db.example.com"})(ctx, "tcp", "other.example.com:5432"); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("allowlist: %v", err)
	}
	if _, err := e.DialContext(nil)(ctx, "tcp", "noport"); !errors.Is(err, ErrEgressDenied) {
		t.Errorf("missing port: %v", err)
	}
	if !e.Trusted(strings.ToUpper(lis.Addr().String())) || e.Trusted("example.com:80") {
		t.Error("Trusted matching")
	}
}
