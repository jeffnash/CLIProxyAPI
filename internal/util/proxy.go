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
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

// MaskProxyURL redacts user credentials from a proxy URL for safe logging.
func MaskProxyURL(raw string) string {
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

// NoProxyEnvRaw returns the raw NO_PROXY / no_proxy environment variable value.
func NoProxyEnvRaw() string {
	if v := strings.TrimSpace(os.Getenv("NO_PROXY")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("no_proxy"))
}

// ParseNoProxyList splits a comma-separated NO_PROXY value into a lowercase list.
func ParseNoProxyList(raw string) []string {
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

// ShouldBypassProxy returns true when host matches any NO_PROXY pattern.
func ShouldBypassProxy(host string, patterns []string) bool {
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

	setting, errParse := proxyutil.Parse(proxyURLRaw)
	if errParse != nil {
		log.Errorf("%v", errParse)
		return httpClient
	}

	switch setting.Mode {
	case proxyutil.ModeDirect:
		httpClient.Transport = proxyutil.NewDirectTransport()
		return httpClient
	case proxyutil.ModeProxy:
	case proxyutil.ModeInherit:
		return httpClient
	default:
		return httpClient
	}

	noProxyList := ParseNoProxyList(NoProxyEnvRaw())
	var transport *http.Transport

	if setting.URL.Scheme == "socks5" {
		var proxyAuth *proxy.Auth
		if setting.URL.User != nil {
			username := setting.URL.User.Username()
			password, _ := setting.URL.User.Password()
			proxyAuth = &proxy.Auth{User: username, Password: password}
		}
		dialer, errSOCKS5 := proxy.SOCKS5("tcp", setting.URL.Host, proxyAuth, proxy.Direct)
		if errSOCKS5 != nil {
			log.Errorf("create SOCKS5 dialer failed: %v", errSOCKS5)
			return httpClient
		}
		direct := &net.Dialer{}
		transport = &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if ShouldBypassProxy(addr, noProxyList) {
					return direct.DialContext(ctx, network, addr)
				}
				return dialer.Dial(network, addr)
			},
		}
	} else if setting.URL.Scheme == "http" || setting.URL.Scheme == "https" {
		transport = &http.Transport{
			Proxy: func(req *http.Request) (*url.URL, error) {
				if req != nil && req.URL != nil && ShouldBypassProxy(req.URL.Hostname(), noProxyList) {
					return nil, nil
				}
				return setting.URL, nil
			},
		}
	}

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
