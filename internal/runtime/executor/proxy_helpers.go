package executor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// httpClientCache caches HTTP clients by proxy URL to enable connection reuse
var (
	httpClientCache      = make(map[string]*http.Client)
	httpClientCacheMutex sync.RWMutex
)

var proxyInfoOnce sync.Map

func maskProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u == nil {
		return "<invalid-proxy-url>"
	}
	if u.User != nil {
		u.User = url.UserPassword("****", "****")
	}
	return u.String()
}

func noProxyEnvRaw() string {
	if v := strings.TrimSpace(os.Getenv("NO_PROXY")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("no_proxy"))
}

func parseNoProxyList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func shouldBypassProxy(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || len(patterns) == 0 {
		return false
	}
	// Strip any port if present.
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	}
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		// Exact match.
		if host == p {
			return true
		}
		// Domain suffix match (".example.com" matches "a.example.com").
		if strings.HasPrefix(p, ".") && strings.HasSuffix(host, p) {
			return true
		}
		// Allow "example.com" to match subdomains too.
		if !strings.HasPrefix(p, ".") && strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

func logProxyOnce(key, msg string, args ...any) {
	if _, loaded := proxyInfoOnce.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	log.Infof(msg, args...)
}

// newProxyAwareHTTPClient creates an HTTP client with proper proxy configuration priority:
// 1. Use auth.ProxyURL if configured (highest priority)
// 2. Use cfg.ProxyURL if auth proxy is not configured AND the proxy is enabled for the given service
// 3. Use RoundTripper from context if neither are configured
//
// This function caches HTTP clients by proxy URL to enable TCP/TLS connection reuse.
//
// NOTE: Avoid caching non-zero http.Client.Timeout values. http.Client.Timeout applies to the
// entire request including reading the response body; caching a timed client can accidentally
// impose that timeout on long-lived streaming requests.
//
// Parameters:
//   - ctx: The context containing optional RoundTripper
//   - cfg: The application configuration
//   - auth: The authentication information
//   - timeout: The client timeout (0 means no timeout)
//   - service: the logical outbound service name (e.g. "copilot", "codex") for OUTBOUND_PROXY_SERVICES allowlisting
//
// Returns:
//   - *http.Client: An HTTP client with configured proxy or transport
func newProxyAwareHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration, service string) *http.Client {
	// Priority 1: Use auth.ProxyURL if configured
	var proxyURL string
	proxySource := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
		if proxyURL != "" {
			proxySource = "auth"
		}
	}

	// Priority 2: Use cfg.ProxyURL if auth proxy is not configured
	if proxyURL == "" && cfg != nil {
		if cfg.SDKConfig.ProxyEnabledFor(service) {
			proxyURL = strings.TrimSpace(cfg.ProxyURL)
			if proxyURL != "" {
				proxySource = "global"
			}
		} else if strings.TrimSpace(cfg.ProxyURL) != "" {
			// Explicitly log that the proxy is configured but disabled for this service.
			logProxyOnce(
				fmt.Sprintf("proxy.disabled.%s", strings.ToLower(strings.TrimSpace(service))),
				"proxy: service=%s disabled by OUTBOUND_PROXY_SERVICES (proxy configured but not enabled for service)",
				service,
			)
		}
	}

	noProxyRaw := ""
	noProxyList := []string(nil)
	if proxyURL != "" {
		noProxyRaw = noProxyEnvRaw()
		noProxyList = parseNoProxyList(noProxyRaw)
	}

	// Build cache key from proxy URL (empty string for no proxy)
	cacheKey := proxyURL
	if proxyURL != "" && noProxyRaw != "" {
		cacheKey = proxyURL + "|no_proxy=" + strings.ToLower(noProxyRaw)
	}

	// Check cache first
	httpClientCacheMutex.RLock()
	if cachedClient, ok := httpClientCache[cacheKey]; ok {
		httpClientCacheMutex.RUnlock()
		// Return a wrapper with the requested timeout but shared transport.
		// Cached clients are stored with Timeout=0 to avoid leaking timeouts across requests.
		if timeout > 0 {
			return &http.Client{
				Transport: cachedClient.Transport,
				Timeout:   timeout,
			}
		}
		return cachedClient
	}
	httpClientCacheMutex.RUnlock()

	// Create new base client (Timeout=0). If a timeout is requested, return a per-call wrapper.
	httpClient := &http.Client{}

	// If we have a proxy URL configured, set up the transport
	if proxyURL != "" {
		transport := buildProxyTransport(proxyURL, noProxyList, service)
		if transport != nil {
			httpClient.Transport = transport
			// Cache the base client (Timeout=0) for connection reuse.
			httpClientCacheMutex.Lock()
			httpClientCache[cacheKey] = httpClient
			httpClientCacheMutex.Unlock()
			logProxyOnce(
				fmt.Sprintf("proxy.enabled.%s.%s", strings.ToLower(strings.TrimSpace(service)), maskProxyURL(proxyURL)),
				"proxy: service=%s enabled proxy=%s source=%s no_proxy=%q",
				service,
				maskProxyURL(proxyURL),
				proxySource,
				noProxyRaw,
			)
			if timeout > 0 {
				return &http.Client{Transport: transport, Timeout: timeout}
			}
			return httpClient
		}
		// If proxy setup failed, log and fall through to context RoundTripper
		log.Debugf("failed to setup proxy from URL: %s, falling back to context transport", proxyURL)
	}

	// Priority 3: Use RoundTripper from context (typically from RoundTripperFor)
	if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
		httpClient.Transport = rt
	}

	// Cache the client for the true no-proxy/default-transport case only.
	// If Transport came from context, it may be request/auth-specific and should not be shared.
	if proxyURL == "" && httpClient.Transport == nil {
		httpClientCacheMutex.Lock()
		httpClientCache[cacheKey] = httpClient
		httpClientCacheMutex.Unlock()
	}

	if timeout > 0 {
		return &http.Client{Transport: httpClient.Transport, Timeout: timeout}
	}
	return httpClient
}

// buildProxyTransport creates an HTTP transport configured for the given proxy URL.
// It supports SOCKS5, HTTP, and HTTPS proxy protocols.
//
// Parameters:
//   - proxyURL: The proxy URL string (e.g., "socks5://user:pass@host:port", "http://host:port")
//   - noProxyList: parsed NO_PROXY list used to bypass proxy for matching hosts
//   - service: logical service name (only used for low-noise bypass logging)
//
// Returns:
//   - *http.Transport: A configured transport, or nil if the proxy URL is invalid
func buildProxyTransport(proxyURL string, noProxyList []string, service string) *http.Transport {
	if proxyURL == "" {
		return nil
	}

	parsedURL, errParse := url.Parse(proxyURL)
	if errParse != nil {
		log.Errorf("parse proxy URL failed: %v", errParse)
		return nil
	}

	var transport *http.Transport

	// Handle different proxy schemes
	if parsedURL.Scheme == "socks5" {
		// Configure SOCKS5 proxy with optional authentication
		var proxyAuth *proxy.Auth
		if parsedURL.User != nil {
			username := parsedURL.User.Username()
			password, _ := parsedURL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", parsedURL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
			return nil
		}
		// Set up a custom transport using the SOCKS5 dialer
		direct := &net.Dialer{}
		transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				// addr is host:port; apply NO_PROXY at dial time.
				if shouldBypassProxy(addr, noProxyList) {
					logProxyOnce(
						fmt.Sprintf("proxy.bypass.%s.%s", strings.ToLower(strings.TrimSpace(service)), strings.ToLower(strings.TrimSpace(addr))),
						"proxy: service=%s bypass host=%s reason=NO_PROXY",
						service,
						addr,
					)
					return direct.DialContext(ctx, network, addr)
				}
				return dialer.Dial(network, addr)
			},
		}
	} else if parsedURL.Scheme == "http" || parsedURL.Scheme == "https" {
		// Configure HTTP or HTTPS proxy with NO_PROXY bypass support.
		transport = &http.Transport{
			Proxy: func(req *http.Request) (*url.URL, error) {
				if req != nil && req.URL != nil {
					host := req.URL.Hostname()
					if shouldBypassProxy(host, noProxyList) {
						logProxyOnce(
							fmt.Sprintf("proxy.bypass.%s.%s", strings.ToLower(strings.TrimSpace(service)), strings.ToLower(strings.TrimSpace(host))),
							"proxy: service=%s bypass host=%s reason=NO_PROXY",
							service,
							host,
						)
						return nil, nil
					}
				}
				return parsedURL, nil
			},
		}
	} else {
		log.Errorf("unsupported proxy scheme: %s", parsedURL.Scheme)
		return nil
	}

	return transport
}
