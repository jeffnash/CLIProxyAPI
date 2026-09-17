// Package config provides configuration management for the CLI Proxy API server.
//
// This file holds branch-maintained provider integrations (Copilot, Kiro, Grok,
// Cursor, Chutes, Amp, passthru routes, secret DLP, OAuth proxy pools) and their
// load-time normalization, kept alongside the upstream configuration surface.
package config

import (
	"strings"

	copilotshared "github.com/router-for-me/CLIProxyAPI/v7/internal/copilot"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// PassthruRoute maps a local model name to an upstream provider endpoint.
// These routes synthesize runtime Auth entries, so they participate in normal
// selection, retries, proxies, and logging.
type PassthruRoute struct {
	Model            string              `yaml:"model" json:"model"`
	ModelRoutingName string              `yaml:"model-routing-name,omitempty" json:"model-routing-name,omitempty"`
	Protocol         string              `yaml:"protocol" json:"protocol"`
	UpstreamModel    string              `yaml:"upstream-model,omitempty" json:"upstream-model,omitempty"`
	BaseURL          string              `yaml:"base-url" json:"base-url"`
	APIKey           string              `yaml:"api-key,omitempty" json:"api-key,omitempty"`
	APIKeys          []string            `yaml:"api-keys,omitempty" json:"api-keys,omitempty"`
	ProxyURL         string              `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
	Headers          map[string]string   `yaml:"headers,omitempty" json:"headers,omitempty"`
	ContextWindow    int                 `yaml:"context-window,omitempty" json:"context-window,omitempty"`
	MaxTokens        int                 `yaml:"max-tokens,omitempty" json:"max-tokens,omitempty"`
	ModelOverride    *registry.ModelInfo `yaml:"model-override,omitempty" json:"model-override,omitempty"`
	// PreserveReasoningContent repairs clients that drop assistant reasoning_content
	// between tool-call turns for upstreams that require it.
	PreserveReasoningContent bool `yaml:"preserve-reasoning-content,omitempty" json:"preserve-reasoning-content,omitempty"`
	// SupportsDeveloperRole controls whether OpenAI-compatible passthru upstreams
	// accept the OpenAI "developer" role. Set false for DeepSeek-style APIs that
	// only accept system/user/assistant/tool roles.
	SupportsDeveloperRole *bool              `yaml:"supports-developer-role,omitempty" json:"supports-developer-role,omitempty"`
	Payload               *PassthruPayload   `yaml:"payload,omitempty" json:"payload,omitempty"`
	RateLimit             *PassthruRateLimit `yaml:"rate-limit,omitempty" json:"rate-limit,omitempty"`
	Lenient               bool               `yaml:"lenient,omitempty" json:"lenient,omitempty"`
	// DropTools removes tools by name from the request before forwarding to this
	// route's upstream. Shorthand for a payload drop-tools rule scoped to this
	// route; useful when a strict upstream rejects specific tool schemas.
	DropTools []string `yaml:"drop-tools,omitempty" json:"drop-tools,omitempty"`
	// TruncateTools enables automatic truncation and mapping of tool names longer than 64 characters.
	// When enabled, tool names exceeding 64 chars are deterministically shortened to 64 with a hash suffix
	// before forwarding to the upstream, and restored on the response. Required for strict upstreams
	// like api.meta.ai that enforce a 64-char limit (e.g., chrome-devtools-mcp tools).
	TruncateTools bool `yaml:"truncate-tools,omitempty" json:"truncate-tools,omitempty"`
	// StableCacheBreakpoints places prompt-cache breakpoints only on
	// conversation-stable sections (system prompt, tool definitions) and skips
	// the rolling latest-message breakpoint. Required for upstreams such as
	// api.meta.ai whose cache only reuses a breakpointed prefix that is
	// byte-stable across turns; the Anthropic-style rolling breakpoint would
	// bust the cache on every turn.
	StableCacheBreakpoints bool `yaml:"stable-cache-breakpoints,omitempty" json:"stable-cache-breakpoints,omitempty"`
}

// PassthruPayload defines route-scoped payload parameter rules.
type PassthruPayload struct {
	// Default sets parameters only when they are missing in the request payload.
	Default map[string]any `yaml:"default,omitempty" json:"default,omitempty"`
	// DefaultRaw sets raw JSON values only when they are missing.
	DefaultRaw map[string]any `yaml:"default-raw,omitempty" json:"default-raw,omitempty"`
	// Override always sets parameters, overwriting any existing values.
	Override map[string]any `yaml:"override,omitempty" json:"override,omitempty"`
	// OverrideRaw always sets raw JSON values, overwriting any existing values.
	OverrideRaw map[string]any `yaml:"override-raw,omitempty" json:"override-raw,omitempty"`
	// Filter removes parameters from the request payload by JSON path.
	Filter []string `yaml:"filter,omitempty" json:"filter,omitempty"`
	// DropTools removes tools by name from the request payload before forwarding.
	DropTools []string `yaml:"drop-tools,omitempty" json:"drop-tools,omitempty"`
}

// PassthruRateLimit configures per-route retry and cooldown behavior.
// Pointer fields allow distinguishing "not set" from zero values.
type PassthruRateLimit struct {
	RequestRetry           *int  `yaml:"request-retry,omitempty" json:"request-retry,omitempty"`
	DisableCooling         *bool `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
	DisableModelSuspend    *bool `yaml:"disable-model-suspend,omitempty" json:"disable-model-suspend,omitempty"`
	DisableProviderSuspend *bool `yaml:"disable-provider-suspend,omitempty" json:"disable-provider-suspend,omitempty"`
}

// ChutesConfig holds Chutes API configuration.
type ChutesConfig struct {
	APIKey        string   `yaml:"api-key" json:"api-key"`
	Models        []string `yaml:"models,omitempty" json:"models,omitempty"`
	ModelsExclude []string `yaml:"models-exclude,omitempty" json:"models-exclude,omitempty"`
	BaseURL       string   `yaml:"base-url,omitempty" json:"base-url,omitempty"`
	Priority      string   `yaml:"priority,omitempty" json:"priority,omitempty"`
	TEEPreference string   `yaml:"tee-preference,omitempty" json:"tee-preference,omitempty"`
	ProxyURL      string   `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// Retry configuration for handling Chutes' intermittent 429 errors.
	// MaxRetries is the maximum number of retry attempts for 429 errors (default: 4).
	// Set to 0 to disable retries.
	MaxRetries int `yaml:"max-retries,omitempty" json:"max-retries,omitempty"`

	// RetryBackoff is a comma-separated list of backoff durations in seconds for each retry attempt.
	// Example: "5,15,30,60" means 5s wait before 1st retry, 15s before 2nd, etc.
	// If fewer values than max-retries, the last value is repeated.
	// Default: "5,15,30,60"
	RetryBackoff string `yaml:"retry-backoff,omitempty" json:"retry-backoff,omitempty"`
}

// SecretDLPConfig contains provider-level secret redaction policy.
type SecretDLPConfig struct {
	DefaultProviderPolicy string            `yaml:"default-provider-policy,omitempty" json:"default-provider-policy,omitempty"`
	ProviderOverrides     map[string]string `yaml:"provider-overrides,omitempty" json:"provider-overrides,omitempty"`
}

// AmpModelMapping defines a model name mapping for Amp CLI requests.
// When Amp requests a model that isn't available locally, this mapping
// allows routing to an alternative model that IS available.
type AmpModelMapping struct {
	// From is the model name that Amp CLI requests (e.g., "claude-opus-4.5").
	From string `yaml:"from" json:"from"`

	// To is the target model name to route to (e.g., "claude-sonnet-4").
	// The target model must have available providers in the registry.
	To string `yaml:"to" json:"to"`

	// Regex indicates whether the 'from' field should be interpreted as a regular
	// expression for matching model names. When true, this mapping is evaluated
	// after exact matches and in the order provided. Defaults to false (exact match).
	Regex bool `yaml:"regex,omitempty" json:"regex,omitempty"`
}

// AmpCode groups Amp CLI integration settings including upstream routing,
// optional overrides, management route restrictions, and model fallback mappings.
type AmpCode struct {
	// UpstreamURL defines the upstream Amp control plane used for non-provider calls.
	UpstreamURL string `yaml:"upstream-url" json:"upstream-url"`

	// UpstreamAPIKey optionally overrides the Authorization header when proxying Amp upstream calls.
	UpstreamAPIKey string `yaml:"upstream-api-key" json:"upstream-api-key"`

	// UpstreamAPIKeys maps client API keys (from top-level api-keys) to upstream API keys.
	// When a request is authenticated with one of the APIKeys, the corresponding UpstreamAPIKey
	// is used for the upstream Amp request.
	UpstreamAPIKeys []AmpUpstreamAPIKeyEntry `yaml:"upstream-api-keys,omitempty" json:"upstream-api-keys,omitempty"`

	// RestrictManagementToLocalhost restricts Amp management routes (/api/user, /api/threads, etc.)
	// to only accept connections from localhost (127.0.0.1, ::1). When true, prevents drive-by
	// browser attacks and remote access to management endpoints. Default: false (API key auth is sufficient).
	RestrictManagementToLocalhost bool `yaml:"restrict-management-to-localhost" json:"restrict-management-to-localhost"`

	// ModelMappings defines model name mappings for Amp CLI requests.
	// When Amp requests a model that isn't available locally, these mappings
	// allow routing to an alternative model that IS available.
	ModelMappings []AmpModelMapping `yaml:"model-mappings" json:"model-mappings"`

	// ForceModelMappings when true, model mappings take precedence over local API keys.
	// When false (default), local API keys are used first if available.
	ForceModelMappings bool `yaml:"force-model-mappings" json:"force-model-mappings"`
}

// AmpUpstreamAPIKeyEntry maps a set of client API keys to a specific upstream API key.
// When a request is authenticated with one of the APIKeys, the corresponding UpstreamAPIKey
// is used for the upstream Amp request.
type AmpUpstreamAPIKeyEntry struct {
	// UpstreamAPIKey is the API key to use when proxying to the Amp upstream.
	UpstreamAPIKey string `yaml:"upstream-api-key" json:"upstream-api-key"`

	// APIKeys are the client API keys (from top-level api-keys) that map to this upstream key.
	APIKeys []string `yaml:"api-keys" json:"api-keys"`
}

// CopilotKey represents the configuration for GitHub Copilot API access.
// Authentication is handled via device code OAuth flow, not API keys.
type CopilotKey struct {
	// AccountType is the Copilot subscription type (individual, business, enterprise).
	// Defaults to "individual" if not specified.
	AccountType string `yaml:"account-type" json:"account-type"`

	// ProxyURL overrides the global proxy setting for Copilot requests if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// HeaderProfile selects which Copilot client header profile to emulate.
	// Supported values: "cli" (default), "vscode-chat".
	HeaderProfile string `yaml:"header-profile,omitempty" json:"header-profile,omitempty"`

	// CLIHeaderModels lists model IDs that should always use the "cli" header profile.
	CLIHeaderModels []string `yaml:"cli-header-models,omitempty" json:"cli-header-models,omitempty"`

	// VSCodeChatHeaderModels lists model IDs that should always use the "vscode-chat" header profile.
	VSCodeChatHeaderModels []string `yaml:"vscode-chat-header-models,omitempty" json:"vscode-chat-header-models,omitempty"`

	// AgentInitiatorPersist, when true, forces subsequent Copilot requests sharing the
	// same prompt_cache_key to send X-Initiator=agent after the first call. Default false.
	AgentInitiatorPersist bool `yaml:"agent-initiator-persist" json:"agent-initiator-persist"`

	// ForceAgentCall, when true, forces every Copilot request to be treated as an agent call
	// regardless of request payload (X-Initiator: agent). Default false.
	ForceAgentCall bool `yaml:"force-agent-call" json:"force-agent-call"`
}

// GrokKey represents the configuration for Grok (X.AI) API access.
// Authentication uses SSO cookies from grok.com rather than traditional API keys.
type GrokKey struct {
	// SSOToken is the raw JWT value from the grok.com sso cookie (without "sso=" or "sso-rw=" prefixes).
	SSOToken string `yaml:"sso-token" json:"sso-token"`

	// CFClearance is the optional Cloudflare clearance cookie for bypassing protection.
	CFClearance string `yaml:"cf-clearance,omitempty" json:"cf-clearance,omitempty"`

	// TokenType indicates the token tier: "normal" or "super" (for grok-4-heavy access).
	// Defaults to "normal" if not specified.
	TokenType string `yaml:"token-type" json:"token-type"`

	// Label is an optional user-friendly identifier for this Grok configuration.
	Label string `yaml:"label,omitempty" json:"label,omitempty"`

	// ProxyURL overrides the global proxy setting for Grok requests if provided.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
}

// GrokConfig exposes behavioral toggles for Grok integration.
// Values here mirror the reference grok_config options for Statsig headers, cookies, stream handling, and media rendering.
type GrokConfig struct {
	// Temporary controls whether Grok creates temporary conversations (defaults to true when unset).
	Temporary *bool `yaml:"temporary,omitempty" json:"temporary,omitempty"`

	// DynamicStatsig toggles random x-statsig-id generation; when false, FixedStatsigID is used if provided.
	DynamicStatsig *bool `yaml:"dynamic-statsig,omitempty" json:"dynamic-statsig,omitempty"`

	// FixedStatsigID overrides x-statsig-id when DynamicStatsig is false.
	FixedStatsigID string `yaml:"x-statsig-id,omitempty" json:"x-statsig-id,omitempty"`

	// CFClearance provides a global Cloudflare clearance cookie appended to Grok requests.
	CFClearance string `yaml:"cf-clearance,omitempty" json:"cf-clearance,omitempty"`

	// ProxyURL overrides the global proxy for Grok requests when set.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// AcceptLanguage customizes the Accept-Language header sent to Grok.
	AcceptLanguage string `yaml:"accept-language,omitempty" json:"accept-language,omitempty"`

	// ShowThinking controls whether <think> sections are surfaced in translated responses (default true).
	ShowThinking *bool `yaml:"show-thinking,omitempty" json:"show-thinking,omitempty"`

	// FilteredTags drops Grok streaming tokens containing any of these substrings.
	FilteredTags []string `yaml:"filtered-tags,omitempty" json:"filtered-tags,omitempty"`

	// ImageMode chooses how generated images are returned ("url" or "base64"); defaults to "url".
	ImageMode string `yaml:"image-mode,omitempty" json:"image-mode,omitempty"`

	// StreamChunkTimeoutSeconds caps idle time between Grok stream chunks (0 disables).
	StreamChunkTimeoutSeconds int `yaml:"stream-chunk-timeout,omitempty" json:"stream-chunk-timeout,omitempty"`

	// StreamFirstChunkTimeoutSeconds caps time to the first Grok stream chunk (0 disables).
	StreamFirstChunkTimeoutSeconds int `yaml:"stream-first-chunk-timeout,omitempty" json:"stream-first-chunk-timeout,omitempty"`

	// StreamTotalTimeoutSeconds caps total stream duration (0 disables).
	StreamTotalTimeoutSeconds int `yaml:"stream-total-timeout,omitempty" json:"stream-total-timeout,omitempty"`

	// RequestTimeoutSeconds sets the HTTP client timeout for Grok requests (defaults to 120s when unset/zero).
	RequestTimeoutSeconds int `yaml:"request-timeout,omitempty" json:"request-timeout,omitempty"`
}

// CursorKey represents the configuration for a Cursor Composer API key.
// Authentication uses a Cursor user API key (crsr_*) from cursor.com Integrations settings.
type CursorKey struct {
	// APIKey is the Cursor user API key (crsr_*).
	APIKey string `yaml:"api-key" json:"api-key"`

	// Prefix optionally namespaces models for this credential.
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`

	// ProxyURL optionally overrides the global proxy for this API key.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// ChatEndpoint optionally overrides the Cursor chat endpoint URL.
	ChatEndpoint string `yaml:"chat-endpoint,omitempty" json:"chat-endpoint,omitempty"`

	// BackendBaseURL optionally overrides the Cursor backend base URL.
	BackendBaseURL string `yaml:"backend-base-url,omitempty" json:"backend-base-url,omitempty"`

	// ComposerBridgeURL optionally overrides the Cursor Composer Client-Tools agent bridge URL
	// (cursor-agent-bridge.mjs, default http://127.0.0.1:9798). The Cursor Composer Client-Tools path
	// is the default, safe routing: the @cursor/sdk sidecar owns all Cursor I/O
	// and every tool executes on the client. Set CURSOR_DIRECT=1 to opt into the
	// gated, ToS-exposed direct path instead.
	ComposerBridgeURL string `yaml:"composer-client-tools-bridge-url,omitempty" json:"composer-client-tools-bridge-url,omitempty"`

	// ComposerBridgeToken optionally sets the multi-tenant bridge auth token (sent as X-Bridge-Auth).
	// When set, the bridge (CURSOR_AGENT_BRIDGE_TOKEN) gates on this token and uses THIS key's Cursor
	// credential under an isolated SDK platform/stateRoot, so multiple Cursor keys can share one bridge.
	// Leave empty for the default single-tenant setup (one CURSOR_API_KEY per bridge).
	ComposerBridgeToken string `yaml:"composer-client-tools-bridge-token,omitempty" json:"composer-client-tools-bridge-token,omitempty"`

	// ToolAliases optionally overrides how Cursor-emitted/generic tool names map to THIS client's tool
	// names (e.g. {"shell": "RunCommand"}), taking precedence over the built-in alias table when the
	// target tool is advertised. Also settable globally via env CURSOR_TOOL_ALIASES (JSON object or a
	// "from=to,from=to" list); the per-key value wins on conflict.
	ToolAliases map[string]string `yaml:"tool-aliases,omitempty" json:"tool-aliases,omitempty"`

	// Models defines upstream model names and aliases for request routing.
	Models []CursorModel `yaml:"models,omitempty" json:"models,omitempty"`

	// Headers optionally adds extra HTTP headers for requests sent with this key.
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`

	// ExcludedModels lists model IDs that should be excluded for this provider.
	ExcludedModels []string `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
}

// CursorModel describes a mapping between an alias and the actual upstream model name.
type CursorModel struct {
	// Name is the upstream model identifier used when issuing requests.
	Name string `yaml:"name" json:"name"`

	// Alias is the client-facing model name that maps to Name.
	Alias string `yaml:"alias" json:"alias"`
}

// KiroKey represents the configuration for Kiro (AWS CodeWhisperer) authentication.
type KiroKey struct {
	// TokenFile is the path to the Kiro token file (default: ~/.aws/sso/cache/kiro-auth-token.json)
	TokenFile string `yaml:"token-file,omitempty" json:"token-file,omitempty"`

	// AccessToken is the OAuth access token for direct configuration.
	AccessToken string `yaml:"access-token,omitempty" json:"access-token,omitempty"`

	// RefreshToken is the OAuth refresh token for token renewal.
	RefreshToken string `yaml:"refresh-token,omitempty" json:"refresh-token,omitempty"`

	// ProfileArn is the AWS CodeWhisperer profile ARN.
	ProfileArn string `yaml:"profile-arn,omitempty" json:"profile-arn,omitempty"`

	// Region is the AWS region (default: us-east-1).
	Region string `yaml:"region,omitempty" json:"region,omitempty"`

	// ProxyURL optionally overrides the global proxy for this configuration.
	ProxyURL string `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`

	// AgentTaskType sets the Kiro API task type. Known values: "vibe", "dev", "chat".
	// Leave empty to let API use defaults. Different values may inject different system prompts.
	AgentTaskType string `yaml:"agent-task-type,omitempty" json:"agent-task-type,omitempty"`

	// PreferredEndpoint sets the preferred Kiro API endpoint/quota.
	// Values: "codewhisperer" (default, IDE quota) or "amazonq" (CLI quota).
	PreferredEndpoint string `yaml:"preferred-endpoint,omitempty" json:"preferred-endpoint,omitempty"`
}

func (k CursorKey) GetAPIKey() string  { return k.APIKey }
func (k CursorKey) GetBaseURL() string { return k.BackendBaseURL }

// CursorModel describes a mapping between an alias and the actual upstream model name.
func (m CursorModel) GetName() string                        { return m.Name }
func (m CursorModel) GetAlias() string                       { return m.Alias }
func (m CursorModel) GetDisplayName() string                 { return "" }
func (m CursorModel) GetThinking() *registry.ThinkingSupport { return nil }

func mergePassthruRoutes(existing, incoming []PassthruRoute) []PassthruRoute {
	if len(existing) == 0 {
		return append([]PassthruRoute(nil), incoming...)
	}
	if len(incoming) == 0 {
		return existing
	}
	byModel := make(map[string]int, len(existing))
	out := append([]PassthruRoute(nil), existing...)
	for i := range out {
		m := strings.ToLower(strings.TrimSpace(out[i].Model))
		if m == "" {
			continue
		}
		byModel[m] = i
	}
	for _, r := range incoming {
		r.Headers = NormalizeHeaders(r.Headers)
		m := strings.ToLower(strings.TrimSpace(r.Model))
		if m == "" {
			continue
		}
		if idx, ok := byModel[m]; ok {
			out[idx] = r
			continue
		}
		byModel[m] = len(out)
		out = append(out, r)
	}
	return out
}

// AddPassthruPayloadRules expands per-route passthru payload settings into the
// shared payload rule engine used by provider executors.
func (cfg *Config) AddPassthruPayloadRules() {
	if cfg == nil || len(cfg.Passthru) == 0 {
		return
	}
	defaultRules := make([]PayloadRule, 0)
	defaultRawRules := make([]PayloadRule, 0)
	overrideRules := make([]PayloadRule, 0)
	overrideRawRules := make([]PayloadRule, 0)
	filterRules := make([]PayloadFilterRule, 0)
	dropToolsRules := make([]PayloadDropToolsRule, 0)
	for i := range cfg.Passthru {
		route := &cfg.Passthru[i]
		// Collect drop-tools from the route shorthand and the payload block.
		dropTools := cloneStringSlice(route.DropTools)
		if route.Payload != nil {
			dropTools = append(dropTools, route.Payload.DropTools...)
		}
		payloadEmpty := route.Payload == nil || passthruPayloadEmpty(*route.Payload)
		if payloadEmpty && len(dropTools) == 0 {
			continue
		}
		models := passthruPayloadModelRules(route)
		if len(models) == 0 {
			continue
		}
		if len(dropTools) > 0 {
			dropToolsRules = append(dropToolsRules, PayloadDropToolsRule{
				Models: models,
				Tools:  cloneStringSlice(dropTools),
			})
		}
		if route.Payload == nil {
			continue
		}
		payload := route.Payload
		if len(payload.Default) > 0 {
			defaultRules = append(defaultRules, PayloadRule{
				Models: models,
				Params: clonePayloadParams(payload.Default),
			})
		}
		if len(payload.DefaultRaw) > 0 {
			defaultRawRules = append(defaultRawRules, PayloadRule{
				Models: models,
				Params: clonePayloadParams(payload.DefaultRaw),
			})
		}
		if len(payload.Override) > 0 {
			overrideRules = append(overrideRules, PayloadRule{
				Models: models,
				Params: clonePayloadParams(payload.Override),
			})
		}
		if len(payload.OverrideRaw) > 0 {
			overrideRawRules = append(overrideRawRules, PayloadRule{
				Models: models,
				Params: clonePayloadParams(payload.OverrideRaw),
			})
		}
		if len(payload.Filter) > 0 {
			filterRules = append(filterRules, PayloadFilterRule{
				Models: models,
				Params: cloneStringSlice(payload.Filter),
			})
		}
	}
	// Route-scoped defaults should beat broad global defaults; route-scoped
	// overrides should run after broad global overrides.
	cfg.Payload.Default = append(defaultRules, cfg.Payload.Default...)
	cfg.Payload.DefaultRaw = append(defaultRawRules, cfg.Payload.DefaultRaw...)
	cfg.Payload.Override = append(cfg.Payload.Override, overrideRules...)
	cfg.Payload.OverrideRaw = append(cfg.Payload.OverrideRaw, overrideRawRules...)
	cfg.Payload.Filter = append(cfg.Payload.Filter, filterRules...)
	cfg.Payload.DropTools = append(cfg.Payload.DropTools, dropToolsRules...)
}
func passthruPayloadEmpty(payload PassthruPayload) bool {
	return len(payload.Default) == 0 &&
		len(payload.DefaultRaw) == 0 &&
		len(payload.Override) == 0 &&
		len(payload.OverrideRaw) == 0 &&
		len(payload.Filter) == 0
}
func passthruPayloadModelRules(route *PassthruRoute) []PayloadModelRule {
	if route == nil {
		return nil
	}
	protocol := normalizePassthruPayloadProtocol(route.Protocol)
	seen := make(map[string]struct{}, 2)
	models := make([]PayloadModelRule, 0, 2)
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		models = append(models, PayloadModelRule{Name: name, Protocol: protocol})
	}
	add(route.Model)
	add(route.ModelRoutingName)
	return models
}
func normalizePassthruPayloadProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "", "openai", "openai-chat", "openai_compat", "openai-compat", "openai-compatibility":
		return "openai"
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "responses":
		return "codex"
	default:
		return "openai"
	}
}
func clonePayloadParams(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func cloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// SanitizeCopilotKeys normalizes Copilot configurations.
// It sets default account type and trims whitespace.
func (cfg *Config) SanitizeCopilotKeys() {
	if cfg == nil || len(cfg.CopilotKey) == 0 {
		return
	}
	for i := range cfg.CopilotKey {
		entry := &cfg.CopilotKey[i]
		entry.AccountType = strings.TrimSpace(strings.ToLower(entry.AccountType))
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		validation := copilotshared.ValidateAccountType(entry.AccountType)
		if validation.Valid {
			entry.AccountType = string(validation.AccountType)
		} else {
			entry.AccountType = string(copilotshared.DefaultAccountType)
		}

		// Normalize header profile (empty string means use per-model detection)
		entry.HeaderProfile = strings.TrimSpace(strings.ToLower(entry.HeaderProfile))
		if entry.HeaderProfile != "" && entry.HeaderProfile != "cli" && entry.HeaderProfile != "vscode-chat" {
			entry.HeaderProfile = "" // Invalid value, reset to default behavior
		}

		// Trim whitespace from model lists
		for j := range entry.CLIHeaderModels {
			entry.CLIHeaderModels[j] = strings.TrimSpace(entry.CLIHeaderModels[j])
		}
		for j := range entry.VSCodeChatHeaderModels {
			entry.VSCodeChatHeaderModels[j] = strings.TrimSpace(entry.VSCodeChatHeaderModels[j])
		}
	}
}

// SanitizeGrokKeys normalizes Grok configurations.
// It validates token types, trims whitespace, and sets defaults.
func (cfg *Config) SanitizeGrokKeys() {
	if cfg == nil || len(cfg.GrokKey) == 0 {
		return
	}
	out := make([]GrokKey, 0, len(cfg.GrokKey))
	for i := range cfg.GrokKey {
		entry := cfg.GrokKey[i]
		entry.SSOToken = extractBareSSOToken(strings.TrimSpace(entry.SSOToken))
		if entry.SSOToken == "" {
			continue
		}
		entry.CFClearance = strings.TrimSpace(entry.CFClearance)
		entry.TokenType = strings.TrimSpace(strings.ToLower(entry.TokenType))
		entry.Label = strings.TrimSpace(entry.Label)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)

		// Validate and normalize token type
		if entry.TokenType != "normal" && entry.TokenType != "super" {
			entry.TokenType = "normal" // Default to normal
		}
		out = append(out, entry)
	}
	cfg.GrokKey = out
}

// SanitizeCursorKeys removes entries without an API key and trims whitespace.
func (cfg *Config) SanitizeCursorKeys() {
	if cfg == nil || len(cfg.CursorKey) == 0 {
		return
	}
	out := make([]CursorKey, 0, len(cfg.CursorKey))
	for i := range cfg.CursorKey {
		entry := cfg.CursorKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Prefix = strings.TrimSpace(entry.Prefix)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ChatEndpoint = strings.TrimSpace(entry.ChatEndpoint)
		entry.BackendBaseURL = strings.TrimSpace(entry.BackendBaseURL)
		out = append(out, entry)
	}
	cfg.CursorKey = out
}

// SanitizeGrokConfig applies defaults and normalization for Grok-specific settings.
func (cfg *Config) SanitizeGrokConfig() {
	if cfg == nil {
		return
	}

	// Defaults based on reference client behavior.
	if cfg.Grok.AcceptLanguage = strings.TrimSpace(cfg.Grok.AcceptLanguage); cfg.Grok.AcceptLanguage == "" {
		cfg.Grok.AcceptLanguage = "zh-CN,zh;q=0.9"
	}

	cfg.Grok.CFClearance = strings.TrimSpace(strings.TrimPrefix(cfg.Grok.CFClearance, "cf_clearance="))
	cfg.Grok.ProxyURL = strings.TrimSpace(cfg.Grok.ProxyURL)
	cfg.Grok.FixedStatsigID = strings.TrimSpace(cfg.Grok.FixedStatsigID)

	if cfg.Grok.ImageMode = strings.TrimSpace(strings.ToLower(cfg.Grok.ImageMode)); cfg.Grok.ImageMode == "" {
		cfg.Grok.ImageMode = "url"
	}

	cfg.Grok.FilteredTags = normalizeList(cfg.Grok.FilteredTags)

	if cfg.Grok.RequestTimeoutSeconds <= 0 {
		cfg.Grok.RequestTimeoutSeconds = 120
	}
	if cfg.Grok.StreamChunkTimeoutSeconds <= 0 {
		cfg.Grok.StreamChunkTimeoutSeconds = 120
	}
	if cfg.Grok.StreamFirstChunkTimeoutSeconds <= 0 {
		cfg.Grok.StreamFirstChunkTimeoutSeconds = 30
	}
	if cfg.Grok.StreamTotalTimeoutSeconds == 0 {
		cfg.Grok.StreamTotalTimeoutSeconds = 600
	} else if cfg.Grok.StreamTotalTimeoutSeconds < 0 {
		cfg.Grok.StreamTotalTimeoutSeconds = 0
	}

	// Enable dynamic statsig by default to avoid requiring a fixed ID.
	if cfg.Grok.DynamicStatsig == nil {
		def := true
		cfg.Grok.DynamicStatsig = &def
	}
	if cfg.Grok.ShowThinking == nil {
		def := true
		cfg.Grok.ShowThinking = &def
	}
	if cfg.Grok.Temporary == nil {
		def := true
		cfg.Grok.Temporary = &def
	}
}

// SanitizeKiroKeys trims whitespace from Kiro credential fields.
func (cfg *Config) SanitizeKiroKeys() {
	if cfg == nil || len(cfg.KiroKey) == 0 {
		return
	}
	for i := range cfg.KiroKey {
		entry := &cfg.KiroKey[i]
		entry.TokenFile = strings.TrimSpace(entry.TokenFile)
		entry.AccessToken = strings.TrimSpace(entry.AccessToken)
		entry.RefreshToken = strings.TrimSpace(entry.RefreshToken)
		entry.ProfileArn = strings.TrimSpace(entry.ProfileArn)
		entry.Region = strings.TrimSpace(entry.Region)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.PreferredEndpoint = strings.TrimSpace(entry.PreferredEndpoint)
	}
}
func splitAndTrim(csv string) []string {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return nil
	}
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func parseKeyValueList(raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		key, value, ok := strings.Cut(entry, "=")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.TrimSpace(value))
		if !ok || key == "" || value == "" {
			continue
		}
		out[key] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func mergeStringMap(existing, incoming map[string]string) map[string]string {
	if len(incoming) == 0 {
		return normalizeStringMap(existing)
	}
	out := normalizeStringMap(existing)
	if out == nil {
		out = make(map[string]string, len(incoming))
	}
	for key, value := range incoming {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.TrimSpace(value))
		if key != "" && value != "" {
			out[key] = value
		}
	}
	return out
}
func normalizeStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.ToLower(strings.TrimSpace(value))
		if key != "" && value != "" {
			out[key] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func normalizeList(entries []string) []string {
	if len(entries) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(entries))
	out := make([]string, 0, len(entries))
	for _, raw := range entries {
		for _, part := range splitAndTrim(raw) {
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
func extractBareSSOToken(token string) string {
	if token == "" {
		return ""
	}
	token = strings.TrimSpace(token)
	if strings.Contains(token, "sso=") {
		parts := strings.Split(token, "sso=")
		last := parts[len(parts)-1]
		if idx := strings.Index(last, ";"); idx >= 0 {
			return strings.TrimSpace(last[:idx])
		}
		return strings.TrimSpace(last)
	}
	return token
}

// Helper accessors avoid leaking pointer handling throughout the codebase.
func (g GrokConfig) TemporaryValue() bool {
	return boolOrDefault(g.Temporary, true)
}
func (g GrokConfig) DynamicStatsigValue() bool {
	return boolOrDefault(g.DynamicStatsig, true)
}
func (g GrokConfig) ShowThinkingValue() bool {
	return boolOrDefault(g.ShowThinking, true)
}
func boolOrDefault(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

// NormalizeOAuthProxyPool cleans provider -> proxy CSV mappings.
// It normalizes provider keys, trims entries, removes empties, and deduplicates URLs while preserving order.
func NormalizeOAuthProxyPool(entries map[string]string) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]string, len(entries))
	for provider, csv := range entries {
		key := strings.ToLower(strings.TrimSpace(provider))
		if key == "" {
			continue
		}
		parts := splitAndTrim(csv)
		if len(parts) == 0 {
			continue
		}
		seen := make(map[string]struct{}, len(parts))
		normalized := make([]string, 0, len(parts))
		for _, part := range parts {
			if _, ok := seen[part]; ok {
				continue
			}
			seen[part] = struct{}{}
			normalized = append(normalized, part)
		}
		if len(normalized) > 0 {
			out[key] = strings.Join(normalized, ",")
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// hashSecret hashes the given secret using bcrypt.
