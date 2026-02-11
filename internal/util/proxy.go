// Package util provides utility functions for the CLI Proxy API server.
// It includes helper functions for proxy configuration, HTTP client setup,
// log level management, and other common operations used across the application.
package util

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

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
	if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
		host = h
	}
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if host == p {
			return true
		}
		if strings.HasPrefix(p, ".") && strings.HasSuffix(host, p) {
			return true
		}
		if !strings.HasPrefix(p, ".") && strings.HasSuffix(host, "."+p) {
			return true
		}
	}
	return false
}

// SetProxyForService configures the provided HTTP client with proxy settings from the configuration,
// but only when the proxy is enabled for the given service via OUTBOUND_PROXY_SERVICES / proxy-services.
//
// It supports SOCKS5, HTTP, and HTTPS proxies. The function modifies the client's transport
// to route requests through the configured proxy server.
func SetProxyForService(cfg *config.SDKConfig, service string, httpClient *http.Client) *http.Client {
	if cfg == nil || httpClient == nil {
		return httpClient
	}
	proxyURLRaw := strings.TrimSpace(cfg.ProxyURL)
	if proxyURLRaw == "" {
		return httpClient
	}
	if !cfg.ProxyEnabledFor(service) {
		return httpClient
	}

	noProxyRaw := noProxyEnvRaw()
	noProxyList := parseNoProxyList(noProxyRaw)

	var transport *http.Transport
	// Attempt to parse the proxy URL from the configuration.
	proxyURL, errParse := url.Parse(proxyURLRaw)
	if errParse == nil {
		// Handle different proxy schemes.
		if proxyURL.Scheme == "socks5" {
			// Configure SOCKS5 proxy with optional authentication.
			var proxyAuth *proxy.Auth
			if proxyURL.User != nil {
				username := proxyURL.User.Username()
				password, _ := proxyURL.User.Password()
				proxyAuth = &proxy.Auth{User: username, Password: password}
			}
			dialer, errSOCKS5 := proxy.SOCKS5("tcp", proxyURL.Host, proxyAuth, proxy.Direct)
			if errSOCKS5 != nil {
				log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
				return httpClient
			}
			// Set up a custom transport using the SOCKS5 dialer.
			direct := &net.Dialer{}
			transport = &http.Transport{
				DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
					// addr is host:port
					if shouldBypassProxy(addr, noProxyList) {
						return direct.DialContext(ctx, network, addr)
					}
					return dialer.Dial(network, addr)
				},
			}
		} else if proxyURL.Scheme == "http" || proxyURL.Scheme == "https" {
			// Configure HTTP or HTTPS proxy with NO_PROXY bypass support.
			transport = &http.Transport{
				Proxy: func(req *http.Request) (*url.URL, error) {
					if req != nil && req.URL != nil {
						host := req.URL.Hostname()
						if shouldBypassProxy(host, noProxyList) {
							return nil, nil
						}
					}
					return proxyURL, nil
				},
			}
		}
	}
	// If a new transport was created, apply it to the HTTP client.
	if transport != nil {
		httpClient.Transport = transport
	}
	return httpClient
}

// SetProxy is a legacy helper that preserves prior behavior for callsites that haven't
// been updated to pass an explicit service name. When OUTBOUND_PROXY_SERVICES is set,
// these callsites will only use the proxy if the allowlist is empty (meaning "all").
func SetProxy(cfg *config.SDKConfig, httpClient *http.Client) *http.Client {
	return SetProxyForService(cfg, "", httpClient)
}
