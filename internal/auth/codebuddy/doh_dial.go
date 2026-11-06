package codebuddy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Some networks' resolvers null-route the CodeBuddy global endpoint: they
// answer 0.0.0.1 for www.codebuddy.ai even though the service is reachable at
// its real addresses (observed with datacenter egress resolvers, while the
// CN and WorkBuddy hosts resolve normally). To stay reachable from such
// networks, connections to *.codebuddy.ai are resolved through
// DNS-over-HTTPS and dialed by address. Every other host, and every failure
// of the DoH lookup, keeps the caller's normal dial path, so this is never
// worse than the unpatched behavior.

// dohResolveURL is the DNS-over-HTTPS endpoint used for *.codebuddy.ai
// lookups. Tests override it.
var dohResolveURL = "https://dns.google/resolve"

// dohHTTPClient performs the DoH lookups. The lookup itself follows the
// environment's proxy configuration, like any other outbound request: only
// the codebuddy.ai resolution is special, the lookup's own path is not.
var dohHTTPClient = &http.Client{
	Timeout:   5 * time.Second,
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
}

// dohCacheTTL bounds how long a DoH answer is reused. The upstream TTL for
// www.codebuddy.ai is 60s, so a slightly shorter cache stays fresh without
// a lookup per connection.
const dohCacheTTL = 50 * time.Second

type dohCacheEntry struct {
	ips []string
	at  time.Time
}

var (
	dohCacheMu sync.Mutex
	dohCache   = map[string]dohCacheEntry{}
)

// hostNeedsDoH reports whether host is a CodeBuddy global endpoint whose
// system resolution may be null-routed.
func hostNeedsDoH(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	return h == "codebuddy.ai" || strings.HasSuffix(h, ".codebuddy.ai")
}

// resolveViaDoH returns the A records for host from the DoH endpoint,
// consulting the cache first. A stale cache entry is returned when a fresh
// lookup fails, so a DoH outage does not break a host that resolved before.
func resolveViaDoH(ctx context.Context, host string) ([]string, error) {
	dohCacheMu.Lock()
	cached, hasCached := dohCache[host]
	dohCacheMu.Unlock()
	if hasCached && time.Since(cached.at) < dohCacheTTL {
		return cached.ips, nil
	}
	ips, err := fetchDoH(ctx, host)
	if err != nil {
		if hasCached {
			return cached.ips, nil
		}
		return nil, err
	}
	dohCacheMu.Lock()
	dohCache[host] = dohCacheEntry{ips: ips, at: time.Now()}
	dohCacheMu.Unlock()
	return ips, nil
}

func fetchDoH(ctx context.Context, host string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		dohResolveURL+"?name="+host+"&type=A", nil)
	if err != nil {
		return nil, err
	}
	resp, err := dohHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("codebuddy: DoH lookup for %s: status %d", host, resp.StatusCode)
	}
	var body struct {
		Answer []struct {
			Type int    `json:"type"`
			Data string `json:"data"`
		} `json:"Answer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, AuthEnvelopeLimit)).Decode(&body); err != nil {
		return nil, err
	}
	var ips []string
	for _, answer := range body.Answer {
		if answer.Type == 1 && net.ParseIP(answer.Data) != nil {
			ips = append(ips, answer.Data)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("codebuddy: DoH lookup for %s: no A records", host)
	}
	return ips, nil
}

type dialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// dohDialContext wraps a base dial function: addresses whose host needs DoH
// are resolved through resolveViaDoH and dialed by IP (the TLS ServerName
// still comes from the request URL, so verification is unaffected); all
// other addresses, IP literals, and failed lookups use the base dialer
// unchanged.
func dohDialContext(base dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil || !hostNeedsDoH(host) || net.ParseIP(host) != nil {
			return base(ctx, network, addr)
		}
		ips, err := resolveViaDoH(ctx, host)
		if err != nil || len(ips) == 0 {
			return base(ctx, network, addr)
		}
		var lastErr error
		for _, ip := range ips {
			conn, errDial := base(ctx, network, net.JoinHostPort(ip, port))
			if errDial == nil {
				return conn, nil
			}
			lastErr = errDial
		}
		return nil, lastErr
	}
}

// wrappedTransports memoizes dohWrapTransport results per source transport
// so connection pooling survives: NewClient runs per operation, and every
// caller sharing a transport keeps sharing one wrapped pool.
var wrappedTransports sync.Map // *http.Transport -> *http.Transport

// dohWrapTransport returns a transport clone whose dialer resolves
// *.codebuddy.ai through DoH and dials directly. A proxied connection would
// hand the proxy the hostname to resolve — exactly the resolution that is
// broken — so the clone drops the inherited environment proxy. An operator
// configured proxy (one that differs from the environment's) is a deliberate
// choice and is respected: the transport is returned unchanged.
// Non-*http.Transport round trippers (test fakes, request decorators) are
// also returned unchanged.
func dohWrapTransport(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		rt = http.DefaultTransport
	}
	transport, ok := rt.(*http.Transport)
	if !ok || transport == nil {
		return rt
	}
	if transport.Proxy != nil {
		probe := &http.Request{URL: &url.URL{Scheme: "https", Host: "www.codebuddy.ai"}}
		viaTransport, errTransport := transport.Proxy(probe)
		viaEnv, errEnv := http.ProxyFromEnvironment(probe)
		if errTransport == nil && errEnv == nil && viaTransport != nil &&
			(viaEnv == nil || viaTransport.String() != viaEnv.String()) {
			return rt
		}
	}
	if wrapped, ok := wrappedTransports.Load(transport); ok {
		return wrapped.(*http.Transport)
	}
	clone := transport.Clone()
	clone.Proxy = nil
	base := clone.DialContext
	if base == nil {
		// Mirror http.DefaultTransport's dialer.
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		base = dialer.DialContext
	}
	clone.DialContext = dohDialContext(base)
	wrapped, _ := wrappedTransports.LoadOrStore(transport, clone)
	return wrapped.(*http.Transport)
}
