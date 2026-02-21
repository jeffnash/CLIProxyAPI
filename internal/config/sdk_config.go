// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"strings"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ProxyServices optionally restricts which outbound services will use ProxyURL.
	//
	// When empty, ProxyURL applies to all outbound services.
	//
	// Typical values: "copilot", "codex", "openai", "anthropic", "google".
	ProxyServices []string `yaml:"proxy-services,omitempty" json:"proxy-services,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// ProxyEnabledFor reports whether the global ProxyURL should be applied for the given service name.
//
// Behavior:
// - If ProxyURL is empty: always false.
// - If ProxyServices is empty: true for all services.
// - Otherwise: true only if the service is included in ProxyServices (case-insensitive).
func (c *SDKConfig) ProxyEnabledFor(service string) bool {
	if c == nil {
		return false
	}
	if strings.TrimSpace(c.ProxyURL) == "" {
		return false
	}
	if len(c.ProxyServices) == 0 {
		return true
	}
	svc := strings.ToLower(strings.TrimSpace(service))
	for _, v := range c.ProxyServices {
		if strings.ToLower(strings.TrimSpace(v)) == svc {
			return true
		}
	}
	return false
}

// StreamingConfig holds server streaming behavior configuration.
type StreamingConfig struct {
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n").
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`

	// DisableProxyBuffering when true adds "X-Accel-Buffering: no" header to SSE responses.
	// This tells reverse proxies (Nginx, Railway) to not buffer the response.
	// Useful when SSE streams get corrupted due to proxy chunking. Default is false.
	DisableProxyBuffering bool `yaml:"disable-proxy-buffering,omitempty" json:"disable-proxy-buffering,omitempty"`
}
