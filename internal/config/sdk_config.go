// Package config provides configuration management for the CLI Proxy API server.
// It handles loading and parsing YAML configuration files, and provides structured
// access to application settings including server port, authentication directory,
// debug settings, proxy configuration, and API keys.
package config

import (
	"math"
	"strings"
	"time"
)

// SDKConfig represents the application's configuration, loaded from a YAML file.
type SDKConfig struct {
	// MaxRequestBodyBytes limits the size (in bytes) of incoming HTTP request bodies.
	// Values <= 0 disable the limit (default).
	MaxRequestBodyBytes int64 `yaml:"max-request-body-bytes" json:"max-request-body-bytes"`

	// Client configures client-facing compatibility behavior.
	Client ClientConfig `yaml:"client" json:"client"`

	// OAuthOnlyFields records v8 provider settings that must wait for credential
	// selection and must not affect API-key credentials. Config YAML snapshots
	// preserve the corresponding v8 paths instead of serializing this metadata.
	OAuthOnlyFields map[string]bool `yaml:"-" json:"-"`

	// CodexResponseSteering mirrors the provider-wide runtime setting for API handlers.
	CodexResponseSteering bool `yaml:"-" json:"-"`

	// ProxyURL is the URL of an optional proxy server to use for outbound requests.
	ProxyURL string `yaml:"proxy-url" json:"proxy-url"`

	// ProxyServices optionally restricts which outbound services will use ProxyURL.
	//
	// When empty, ProxyURL applies to all outbound services.
	//
	// Typical values: "copilot", "codex", "openai", "anthropic", "google".
	ProxyServices []string `yaml:"proxy-services,omitempty" json:"proxy-services,omitempty"`
	// TLSHandshakeTimeoutSeconds bounds upstream TLS handshakes made by the
	// shared uTLS client, in seconds. It applies to both the Claude Code
	// Anthropic transport and the Chrome ChatGPT/Codex transport; the
	// standard fallback transport is unaffected. It starts after TCP or
	// proxy connection establishment returns and does not bound dialing or
	// proxy CONNECT. Those uTLS paths use custom TLS dialing, for which Go
	// ignores http.Transport.TLSHandshakeTimeout, so a peer that accepts the
	// connection but never answers the ClientHello would otherwise block the
	// request indefinitely and request-retry could never engage. The bound
	// applies only during the TLS handshake and is removed once it succeeds,
	// so inference and long-running streams are unaffected. The default 0
	// disables the bound (previous behavior); negative values are treated as
	// 0, and values beyond the representable duration range saturate.
	TLSHandshakeTimeoutSeconds int `yaml:"tls-handshake-timeout-seconds" json:"tls-handshake-timeout-seconds"`

	// DisableImageGeneration controls whether the built-in image_generation tool is injected/allowed.
	//
	// Supported values:
	//   - false (default): image_generation is enabled everywhere (normal behavior).
	//   - true: image_generation is disabled everywhere. The server stops injecting it, removes it from request payloads,
	//     and returns 404 for /v1/images/generations and /v1/images/edits.
	//   - "chat": disable image_generation injection for all non-images endpoints (e.g. /v1/responses, /v1/chat/completions),
	//     while keeping /v1/images/generations and /v1/images/edits enabled and preserving image_generation there.
	//   - "passthrough": do not modify the tool list on non-images endpoints — keep image_generation if the client
	//     sent it and do not inject it otherwise; on /v1/images/generations and /v1/images/edits behave like "chat".
	DisableImageGeneration DisableImageGenerationMode `yaml:"disable-image-generation" json:"disable-image-generation"`

	// GPTImage2BaseModel sets the base (mainline) model used by the legacy hosted
	// image_generation tool path when a Codex image request is not proxied directly
	// through the Image API.
	//
	// The value must start with "gpt-" (case-insensitive). If empty or invalid, the
	// default base model ("gpt-5.4-mini") is used.
	GPTImage2BaseModel string `yaml:"gpt-image-2-base-model,omitempty" json:"gpt-image-2-base-model,omitempty"`

	// VideoResultAuthCacheTTL controls how long video IDs stay pinned to the credential
	// that created them. Accepts duration strings like "30m" or "3h".
	// Empty or invalid values use the default 3h.
	VideoResultAuthCacheTTL string `yaml:"video-result-auth-cache-ttl,omitempty" json:"video-result-auth-cache-ttl,omitempty"`

	// ForceModelPrefix requires explicit model prefixes (e.g., "teamA/gemini-3-pro-preview")
	// to target prefixed credentials. When false, unprefixed model requests may use prefixed
	// credentials as well.
	ForceModelPrefix bool `yaml:"force-model-prefix" json:"force-model-prefix"`

	// RequestLog enables or disables detailed request logging functionality.
	RequestLog bool `yaml:"request-log" json:"request-log"`

	// CodexOrphanDelegationCompatibility mirrors the provider-wide runtime setting for API handlers.
	CodexOrphanDelegationCompatibility bool `yaml:"-" json:"-"`

	// ClaudeCode configures Claude Code compatibility behavior.
	ClaudeCode ClaudeCodeConfig `yaml:"claude-code" json:"claude-code"`

	// APIKeys is a list of keys for authenticating clients to this proxy server.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`

	// EnableGeminiCLIEndpoint enables the localhost-only Gemini CLI compatibility endpoint.
	EnableGeminiCLIEndpoint bool `yaml:"enable-gemini-cli-endpoint" json:"enable-gemini-cli-endpoint"`

	// PassthroughHeaders controls whether upstream response headers are forwarded to downstream clients.
	// Default is false (disabled).
	PassthroughHeaders bool `yaml:"passthrough-headers" json:"passthrough-headers"`

	// Streaming configures server-side streaming behavior (keep-alives and safe bootstrap retries).
	Streaming StreamingConfig `yaml:"streaming" json:"streaming"`

	// ManagedProviders defines first-class externally hosted model providers.
	ManagedProviders []ManagedProviderConfig `yaml:"managed-providers,omitempty" json:"managed-providers,omitempty"`

	// NonStreamKeepAliveInterval controls how often blank lines are emitted for non-streaming responses.
	// <= 0 disables keep-alives. Value is in seconds.
	NonStreamKeepAliveInterval int `yaml:"nonstream-keepalive-interval,omitempty" json:"nonstream-keepalive-interval,omitempty"`
}

// TLSHandshakeTimeout returns the configured upstream TLS handshake bound
// for requests using the shared uTLS client. A non-positive configured
// value disables the bound and yields 0.
func (c *SDKConfig) TLSHandshakeTimeout() time.Duration {
	if c == nil || c.TLSHandshakeTimeoutSeconds <= 0 {
		return 0
	}
	const maxTLSHandshakeTimeoutSeconds = math.MaxInt64 / int64(time.Second)
	if int64(c.TLSHandshakeTimeoutSeconds) > maxTLSHandshakeTimeoutSeconds {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(c.TLSHandshakeTimeoutSeconds) * time.Second
}

// ClientConfig configures client-facing compatibility behavior.
type ClientConfig struct {
	Codex CodexClientConfig `yaml:"codex" json:"codex"`
}

// CodexClientConfig configures Codex client compatibility and the model catalog.
type CodexClientConfig struct {
	// OptimizeMultiAgentV2 optimizes official Codex multi-agent requests across providers.
	// Default false leaves the client's multi-agent behavior unchanged.
	OptimizeMultiAgentV2 bool `yaml:"optimize-multi-agent-v2" json:"optimize-multi-agent-v2"`

	// EnableApplyPatch advertises freeform apply_patch only for supported models.
	// Default false clears the capability regardless of template metadata.
	EnableApplyPatch bool `yaml:"enable-apply-patch" json:"enable-apply-patch"`
}

// ClaudeCodeConfig configures Claude Code compatibility behavior.
type ClaudeCodeConfig struct {
	// DisableCloakingModelList disables model ID cloaking in Anthropic model list responses.
	DisableCloakingModelList bool `yaml:"disable-cloaking-model-list" json:"disable-cloaking-model-list"`
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
	// KeepAliveSeconds controls how often the server emits SSE heartbeats (": keep-alive\n\n")
	// or WebSocket Ping control frames.
	// <= 0 disables keep-alives. Default is 0.
	KeepAliveSeconds int `yaml:"keepalive-seconds,omitempty" json:"keepalive-seconds,omitempty"`

	// BootstrapRetries controls how many times the server may retry a streaming request before any bytes are sent,
	// to allow auth rotation / transient recovery.
	// <= 0 disables bootstrap retries. Default is 0.
	BootstrapRetries int `yaml:"bootstrap-retries,omitempty" json:"bootstrap-retries,omitempty"`

	// SendKeepAliveBeforeFT controls whether SSE heartbeats are sent before the first token arrives
	// when KeepAliveSeconds > 0. Default is false.
	SendKeepAliveBeforeFT bool `yaml:"send-keepalive-before-ft,omitempty" json:"send-keepalive-before-ft,omitempty"`
	// DisableProxyBuffering when true adds "X-Accel-Buffering: no" header to SSE responses.
	// This tells reverse proxies (Nginx, Railway) to not buffer the response.
	// Useful when SSE streams get corrupted due to proxy chunking. Default is false.
	DisableProxyBuffering bool `yaml:"disable-proxy-buffering,omitempty" json:"disable-proxy-buffering,omitempty"`
}

// ManagedProviderConfig describes an external provider with Claude/OpenAI-compatible endpoints.
type ManagedProviderConfig struct {
	Name                  string                              `yaml:"name" json:"name"`
	Prefix                string                              `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	APIKey                string                              `yaml:"api-key,omitempty" json:"api-key,omitempty"`
	APIKeyEnv             string                              `yaml:"api-key-env,omitempty" json:"api-key-env,omitempty"`
	BaseURL               string                              `yaml:"base-url,omitempty" json:"base-url,omitempty"`
	ClaudeBaseURL         string                              `yaml:"claude-base-url,omitempty" json:"claude-base-url,omitempty"`
	AnthropicBaseURL      string                              `yaml:"anthropic-base-url,omitempty" json:"anthropic-base-url,omitempty"`
	OpenAIBaseURL         string                              `yaml:"openai-base-url,omitempty" json:"openai-base-url,omitempty"`
	ClaudeMessagesPath    string                              `yaml:"claude-messages-path,omitempty" json:"claude-messages-path,omitempty"`
	AnthropicMessagesPath string                              `yaml:"anthropic-messages-path,omitempty" json:"anthropic-messages-path,omitempty"`
	OpenAIChatPath        string                              `yaml:"openai-chat-path,omitempty" json:"openai-chat-path,omitempty"`
	OpenAIResponsesPath   string                              `yaml:"openai-responses-path,omitempty" json:"openai-responses-path,omitempty"`
	TransportMode         string                              `yaml:"transport-mode,omitempty" json:"transport-mode,omitempty"`
	DefaultTransport      string                              `yaml:"default-transport,omitempty" json:"default-transport,omitempty"`
	RouteHealth           ManagedProviderRouteHealthConfig    `yaml:"route-health,omitempty" json:"route-health,omitempty"`
	ModelDiscovery        ManagedProviderModelDiscoveryConfig `yaml:"model-discovery,omitempty" json:"model-discovery,omitempty"`
	Models                []string                            `yaml:"models,omitempty" json:"models,omitempty"`
	ModelsExclude         []string                            `yaml:"models-exclude,omitempty" json:"models-exclude,omitempty"`
	FallbackModels        []string                            `yaml:"fallback-models,omitempty" json:"fallback-models,omitempty"`
	Headers               map[string]string                   `yaml:"headers,omitempty" json:"headers,omitempty"`
	Priority              string                              `yaml:"priority,omitempty" json:"priority,omitempty"`
	SecretRedaction       string                              `yaml:"secret-redaction,omitempty" json:"secret-redaction,omitempty"`
	ProxyURL              string                              `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
	MaxRetries            *int                                `yaml:"max-retries,omitempty" json:"max-retries,omitempty"`
	RetryBackoff          string                              `yaml:"retry-backoff,omitempty" json:"retry-backoff,omitempty"`
	// SupportsDeveloperRole controls whether the OpenAI Chat Completions transport
	// may include the OpenAI "developer" role. Set false for upstreams that only
	// accept system/user/assistant/tool roles.
	SupportsDeveloperRole *bool `yaml:"supports-developer-role,omitempty" json:"supports-developer-role,omitempty"`
	// QuotaCooldownMaxSeconds caps how long a 429/quota error cools this provider
	// down. The global cooldown escalates up to 30m; set this (e.g. 60) to hard-cap
	// the lockout for this provider so it recovers quickly. 0/unset uses the default.
	QuotaCooldownMaxSeconds *int `yaml:"quota-cooldown-max-seconds,omitempty" json:"quota-cooldown-max-seconds,omitempty"`
}

// ManagedProviderModelDiscoveryConfig controls provider model discovery.
type ManagedProviderModelDiscoveryConfig struct {
	Enabled *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	URL     string `yaml:"url,omitempty" json:"url,omitempty"`
	Path    string `yaml:"path,omitempty" json:"path,omitempty"`
	Format  string `yaml:"format,omitempty" json:"format,omitempty"`
	TTL     string `yaml:"ttl,omitempty" json:"ttl,omitempty"`
}

// ManagedProviderRouteHealthConfig controls backend transport health tracking and probing.
type ManagedProviderRouteHealthConfig struct {
	Enabled             *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	ProbeEnabled        *bool  `yaml:"probe-enabled,omitempty" json:"probe-enabled,omitempty"`
	StatePath           string `yaml:"state-path,omitempty" json:"state-path,omitempty"`
	ProbeInterval       string `yaml:"probe-interval,omitempty" json:"probe-interval,omitempty"`
	ProbeTimeout        string `yaml:"probe-timeout,omitempty" json:"probe-timeout,omitempty"`
	FirstEventTimeout   string `yaml:"first-event-timeout,omitempty" json:"first-event-timeout,omitempty"`
	Cooldown            string `yaml:"cooldown,omitempty" json:"cooldown,omitempty"`
	UnsupportedTTL      string `yaml:"unsupported-ttl,omitempty" json:"unsupported-ttl,omitempty"`
	MaxConcurrentProbes int    `yaml:"max-concurrent-probes,omitempty" json:"max-concurrent-probes,omitempty"`
}
