package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrEgressDenied marks an outbound request or connection refused by the egress policy.
var ErrEgressDenied = errors.New("egress denied")

// blockedPrefixes are never dialed for tenant endpoints: private (RFC 1918, ULA), loopback,
// link-local (which holds the cloud metadata addresses), CGNAT, documentation, benchmarking,
// multicast and reserved ranges, plus the IPv6 translation prefixes that can embed any of them.
var blockedPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/32", "2001:db8::/32",
		"2002::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Blocked reports whether addr is in a range tenant endpoints may never reach.
func Blocked(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// EgressConfig is the platform-wide egress policy.
type EgressConfig struct {
	// Trusted lists the platform's own hosts ("host" or "host:port"), such as the S3 image
	// host. They skip the https and address checks and are not subject to plan allowlists.
	Trusted []string
	// DialTimeout bounds each TCP connect (default 5 s).
	DialTimeout time.Duration
}

// Egress builds HTTP clients and dialers for tenant-registered endpoints
// (docs/ARCHITECTURE.md §9): https only, an optional per-plan host allowlist, no proxies or
// redirects, and a dialer whose Control hook checks the address actually being connected
// after every DNS resolution, so a hostname that later resolves (or rebinds) to a private,
// loopback, link-local or metadata address is refused.
type Egress struct {
	trusted     map[string]bool
	dialTimeout time.Duration
	blocked     func(netip.Addr) bool
}

// NewEgress returns the egress policy.
func NewEgress(cfg EgressConfig) *Egress {
	e := &Egress{trusted: map[string]bool{}, dialTimeout: cfg.DialTimeout, blocked: Blocked}
	if e.dialTimeout <= 0 {
		e.dialTimeout = 5 * time.Second
	}
	for _, h := range cfg.Trusted {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			e.trusted[h] = true
		}
	}
	return e
}

// Trusted reports whether hostport ("host" or "host:port") is a platform host.
func (e *Egress) Trusted(hostport string) bool {
	hostport = strings.ToLower(hostport)
	if e.trusted[hostport] {
		return true
	}
	host, _, err := net.SplitHostPort(hostport)
	return err == nil && e.trusted[host]
}

// HostAllowed reports whether host matches the allowlist; an empty allowlist allows every
// host. Entries are exact host names or "*.suffix" for subdomains.
func HostAllowed(host string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for _, a := range allowlist {
		a = strings.ToLower(strings.TrimSpace(a))
		if suffix, ok := strings.CutPrefix(a, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == a {
			return true
		}
	}
	return false
}

func denied(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrEgressDenied, fmt.Sprintf(format, args...))
}

// CheckURL validates an endpoint at registration: scheme https (or one of schemes, when
// given, such as postgres), host allowed by the plan allowlist, and every address the host
// resolves to now is public. Platform hosts skip the checks. Dials are checked again later.
func (e *Egress) CheckURL(ctx context.Context, raw string, allowlist []string, schemes ...string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, denied("%q is not an absolute URL", raw)
	}
	if _, hasPassword := u.User.Password(); hasPassword || (u.User != nil && u.Scheme == "https") {
		return nil, denied("credentials belong in a secret, not in the URL")
	}
	if e.Trusted(u.Host) {
		return u, nil
	}
	if len(schemes) == 0 {
		schemes = []string{"https"}
	}
	ok := false
	for _, s := range schemes {
		ok = ok || u.Scheme == s
	}
	if !ok {
		return nil, denied("scheme %q is not allowed (want %s)", u.Scheme, strings.Join(schemes, " or "))
	}
	host := u.Hostname()
	if !HostAllowed(host, allowlist) {
		return nil, denied("host %s is not in the plan's egress allowlist", host)
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	for _, a := range addrs {
		if e.blocked(a) {
			return nil, denied("%s resolves to %s, a private or reserved address", host, a)
		}
	}
	return u, nil
}

// control rejects connections to blocked addresses; it runs after DNS resolution, for every
// address the dialer tries.
func (e *Egress) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return denied("unparseable address %q", address)
	}
	if e.blocked(ap.Addr()) {
		return denied("%s is a private or reserved address", ap.Addr())
	}
	return nil
}

// DialContext returns a dialer for tenant endpoints that enforces the allowlist on the host
// name and the address checks on every resolved address. addr must be host:port, not a
// pre-resolved IP (callers pass gRPC "passthrough" targets and an identity lookup to pgx).
func (e *Egress) DialContext(allowlist []string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if e.Trusted(addr) {
			d := net.Dialer{Timeout: e.dialTimeout}
			return d.DialContext(ctx, network, addr)
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, denied("address %q has no port", addr)
		}
		if !HostAllowed(host, allowlist) {
			return nil, denied("host %s is not in the plan's egress allowlist", host)
		}
		d := net.Dialer{Timeout: e.dialTimeout, Control: e.control}
		conn, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("dial %s: %w", addr, err)
		}
		return conn, nil
	}
}

// GRPCDialer adapts DialContext to grpc.WithContextDialer.
func (e *Egress) GRPCDialer(allowlist []string) func(ctx context.Context, addr string) (net.Conn, error) {
	dial := e.DialContext(allowlist)
	return func(ctx context.Context, addr string) (net.Conn, error) { return dial(ctx, "tcp", addr) }
}

// HTTPClient returns a client for tenant endpoints: https only (platform hosts excepted),
// allowlisted hosts, checked addresses, no proxy and no redirects. timeout bounds each request
// as a backstop; callers pass tighter deadlines through the request context.
func (e *Egress) HTTPClient(allowlist []string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           e.DialContext(allowlist),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{
		Transport: &schemeGuard{egress: e, next: tr},
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// schemeGuard refuses plain-http requests to anything but platform hosts.
type schemeGuard struct {
	egress *Egress
	next   *http.Transport
}

func (g *schemeGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme != "https" && !g.egress.Trusted(r.URL.Host) {
		return nil, denied("%s is not https", r.URL.Redacted())
	}
	resp, err := g.next.RoundTrip(r)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", r.Method, r.URL.Redacted(), err)
	}
	return resp, nil
}

// CloseIdleConnections lets http.Client.CloseIdleConnections reach the transport.
func (g *schemeGuard) CloseIdleConnections() { g.next.CloseIdleConnections() }
