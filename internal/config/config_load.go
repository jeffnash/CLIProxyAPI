package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// LoadConfig reads a YAML configuration file from the given path,
// unmarshals it into a Config struct, applies environment variable overrides,
// and returns it.
//
// Parameters:
//   - configFile: The path to the YAML configuration file
//
// Returns:
//   - *Config: The loaded configuration
//   - error: An error if the configuration could not be loaded
func LoadConfig(configFile string) (*Config, error) {
	return LoadConfigOptional(configFile, false)
}

// LoadConfigOptional reads YAML from configFile.
// If optional is true and the file is missing, it returns an empty Config.
// If optional is true and the file is empty or invalid, it returns an empty Config.
func LoadConfigOptional(configFile string, optional bool) (*Config, error) {
	// Read the entire configuration file into memory.
	data, err := os.ReadFile(configFile)
	if err != nil {
		if optional {
			if os.IsNotExist(err) || errors.Is(err, syscall.EISDIR) {
				// Missing and optional: return empty config (cloud deploy standby).
				cfg := &Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
				cfg.NormalizePluginsConfig()
				return cfg, nil
			}
		}
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// In cloud deploy mode (optional=true), if file is empty or contains only whitespace, return empty config.
	if optional && len(bytes.TrimSpace(data)) == 0 {
		cfg := &Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
		cfg.NormalizePluginsConfig()
		return cfg, nil
	}

	if errValidate := validateCredentialWeightYAML(data); errValidate != nil {
		if optional {
			cfgOptional := &Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
			cfgOptional.NormalizePluginsConfig()
			return cfgOptional, nil
		}
		return nil, errValidate
	}

	// Unmarshal the YAML data into the Config struct.
	var cfg Config
	// Set defaults before unmarshal so that absent keys keep defaults.
	cfg.Host = "" // Default empty: binds to all interfaces (IPv4 + IPv6)
	cfg.LoggingToFile = false
	cfg.LogsMaxTotalSizeMB = 0
	cfg.ErrorLogsMaxFiles = 10
	cfg.UsageStatisticsEnabled = false
	cfg.RedisUsageQueueRetentionSeconds = 60
	cfg.DisableCooling = false
	cfg.SaveCooldownStatus = false
	cfg.TransientErrorCooldownSeconds = 0
	cfg.DisableImageGeneration = DisableImageGenerationOff
	cfg.WebsocketAuth = true
	cfg.Pprof.Enable = false
	cfg.Pprof.Addr = DefaultPprofAddr
	cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	cfg.CredentialInFlight = DefaultCredentialInFlightConfig()
	cfg.IncognitoBrowser = false // Default to normal browser (AWS uses incognito by force)
	if err = yaml.Unmarshal(data, &cfg); err != nil {
		if optional {
			// In cloud deploy mode, if YAML parsing fails, return empty config instead of error.
			cfgOptional := &Config{CredentialInFlight: DefaultCredentialInFlightConfig()}
			cfgOptional.NormalizePluginsConfig()
			return cfgOptional, nil
		}
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Outbound proxy (Railway-friendly env override).
	//
	// Priority:
	// 1) OUTBOUND_PROXY_URL (explicit project env var)
	// 2) HTTPS_PROXY / HTTP_PROXY (standard env vars, only if OUTBOUND_PROXY_URL is unset)
	// 3) YAML proxy-url (already loaded into cfg.ProxyURL)
	if env := strings.TrimSpace(os.Getenv("OUTBOUND_PROXY_URL")); env != "" {
		cfg.ProxyURL = env
	} else {
		// Honor standard proxy env vars as a fallback, but don't override YAML when it's set.
		if strings.TrimSpace(cfg.ProxyURL) == "" {
			if env := strings.TrimSpace(os.Getenv("HTTPS_PROXY")); env != "" {
				cfg.ProxyURL = env
			} else if env := strings.TrimSpace(os.Getenv("HTTP_PROXY")); env != "" {
				cfg.ProxyURL = env
			}
		}
	}

	// Optional proxy service scoping.
	//
	// OUTBOUND_PROXY_SERVICES is a comma-separated allowlist of service names that should use
	// the global outbound proxy. When empty/unset, the proxy applies to all services.
	//
	// Example: "copilot,codex"
	if env := strings.TrimSpace(os.Getenv("OUTBOUND_PROXY_SERVICES")); env != "" {
		parts := strings.Split(env, ",")
		cfg.ProxyServices = cfg.ProxyServices[:0]
		for _, p := range parts {
			if v := strings.TrimSpace(p); v != "" {
				cfg.ProxyServices = append(cfg.ProxyServices, strings.ToLower(v))
			}
		}
	}

	cfg.CredentialConcurrency = cfg.CredentialConcurrency.WithDefaults()
	if errValidate := cfg.CredentialInFlight.Validate(); errValidate != nil {
		return nil, errValidate
	}
	if errValidate := cfg.Codex.LiveMediaRelay.Validate(); errValidate != nil {
		return nil, errValidate
	}
	if errValidate := cfg.ValidateCredentialWeights(); errValidate != nil {
		return nil, errValidate
	}

	// Hash remote management key if plaintext is detected (nested)
	// We consider a value to be already hashed if it looks like a bcrypt hash ($2a$, $2b$, or $2y$ prefix).
	if cfg.RemoteManagement.SecretKey != "" && !looksLikeBcrypt(cfg.RemoteManagement.SecretKey) {
		hashed, errHash := hashSecret(cfg.RemoteManagement.SecretKey)
		if errHash != nil {
			return nil, fmt.Errorf("failed to hash remote management key: %w", errHash)
		}
		cfg.RemoteManagement.SecretKey = hashed

		// Persist the hashed value back to the config file to avoid re-hashing on next startup.
		// Preserve YAML comments and ordering; update only the nested key.
		_ = SaveConfigPreserveCommentsUpdateNestedScalar(configFile, []string{"remote-management", "secret-key"}, hashed)
	}

	cfg.RemoteManagement.PanelGitHubRepository = strings.TrimSpace(cfg.RemoteManagement.PanelGitHubRepository)
	if cfg.RemoteManagement.PanelGitHubRepository == "" {
		cfg.RemoteManagement.PanelGitHubRepository = DefaultPanelGitHubRepository
	}

	cfg.Pprof.Addr = strings.TrimSpace(cfg.Pprof.Addr)
	if cfg.Pprof.Addr == "" {
		cfg.Pprof.Addr = DefaultPprofAddr
	}

	if cfg.LogsMaxTotalSizeMB < 0 {
		cfg.LogsMaxTotalSizeMB = 0
	}

	if cfg.ErrorLogsMaxFiles < 0 {
		cfg.ErrorLogsMaxFiles = 10
	}

	if cfg.RedisUsageQueueRetentionSeconds <= 0 {
		cfg.RedisUsageQueueRetentionSeconds = 60
	} else if cfg.RedisUsageQueueRetentionSeconds > 3600 {
		log.WithField("value", cfg.RedisUsageQueueRetentionSeconds).Warn("redis-usage-queue-retention-seconds too large; clamping to 3600")
		cfg.RedisUsageQueueRetentionSeconds = 3600
	}

	if cfg.MaxRetryCredentials < 0 {
		cfg.MaxRetryCredentials = 0
	}

	cfg.NormalizePluginsConfig()
	if errResolvePluginsDir := cfg.ResolvePluginsDir(); errResolvePluginsDir != nil && cfg.Plugins.Enabled {
		return nil, errResolvePluginsDir
	}

	// Sanitize Gemini API key configuration and migrate legacy entries.
	cfg.SanitizeGeminiKeys()

	// Sanitize native Interactions API key configuration.
	cfg.SanitizeInteractionsKeys()

	// Sanitize Vertex-compatible API keys.
	cfg.SanitizeVertexCompatKeys()

	// Sanitize Codex keys: drop entries without base-url
	cfg.SanitizeCodexKeys()

	// Sanitize Copilot keys: normalize account type
	cfg.SanitizeCopilotKeys()

	// Sanitize Grok keys: normalize token types and trim whitespace
	cfg.SanitizeGrokKeys()
	cfg.SanitizeGrokConfig()

	// Sanitize xAI keys: drop entries without base-url
	cfg.SanitizeXAIKeys()

	// Sanitize Codex header defaults.
	cfg.SanitizeCodexHeaderDefaults()

	// Sanitize Claude header defaults.
	cfg.SanitizeClaudeHeaderDefaults()

	// Sanitize Claude key headers
	cfg.SanitizeClaudeKeys()

	// Sanitize Kiro keys: trim whitespace from credential fields
	cfg.SanitizeKiroKeys()

	// Sanitize OpenAI compatibility providers: drop entries without base-url
	cfg.SanitizeOpenAICompatibility()

	// Normalize passthru routes (trim headers, etc.).
	if len(cfg.Passthru) > 0 {
		for i := range cfg.Passthru {
			cfg.Passthru[i].Headers = NormalizeHeaders(cfg.Passthru[i].Headers)
		}
	}

	// Load streaming keep-alive from env (useful for Railway deployments with 60s proxy timeout).
	// STREAMING_KEEPALIVE_SECONDS sets how often the server emits SSE heartbeats.
	if env := strings.TrimSpace(os.Getenv("STREAMING_KEEPALIVE_SECONDS")); env != "" {
		if seconds, errParse := strconv.Atoi(env); errParse == nil && seconds > 0 {
			cfg.Streaming.KeepAliveSeconds = seconds
		}
	}

	// VERBOSE_LOGGING enables debug-level logging and request/response snippet capture.
	// This is useful for Railway deployments where you need more visibility without editing YAML.
	if env := strings.TrimSpace(os.Getenv("VERBOSE_LOGGING")); env != "" {
		switch strings.ToLower(env) {
		case "true", "1", "yes", "y", "on":
			cfg.Debug = true
			cfg.RequestLog = true
		case "false", "0", "no", "n", "off":
			// no-op: keep config values
		}
	}

	// STREAMING_DISABLE_PROXY_BUFFERING adds X-Accel-Buffering: no header to SSE responses.
	// Useful for Railway/Nginx deployments where proxy buffering corrupts SSE streams.
	if env := strings.TrimSpace(os.Getenv("STREAMING_DISABLE_PROXY_BUFFERING")); env != "" {
		switch strings.ToLower(env) {
		case "true", "1", "yes", "y", "on":
			cfg.Streaming.DisableProxyBuffering = true
		default:
			cfg.Streaming.DisableProxyBuffering = false
		}
	}

	// Load passthru routes from env (useful for Railway deployments).
	// Env format: JSON array matching []PassthruRoute.
	if env := strings.TrimSpace(os.Getenv("PASSTHRU_MODELS_JSON")); env != "" {
		var routes []PassthruRoute
		if err := json.Unmarshal([]byte(env), &routes); err != nil {
			if !optional {
				return nil, fmt.Errorf("failed to parse PASSTHRU_MODELS_JSON: %w", err)
			}
		} else {
			cfg.Passthru = mergePassthruRoutes(cfg.Passthru, routes)
			cfg.legacyMigrationPending = true
		}
	}

	// Load Chutes configuration from env.
	if env := strings.TrimSpace(os.Getenv("CHUTES_API_KEY")); env != "" {
		cfg.Chutes.APIKey = env
	}
	if env := strings.TrimSpace(os.Getenv("CHUTES_BASE_URL")); env != "" {
		cfg.Chutes.BaseURL = env
	}
	if env := strings.TrimSpace(os.Getenv("CHUTES_MODELS")); env != "" {
		cfg.Chutes.Models = splitAndTrim(env)
	}
	if env := strings.TrimSpace(os.Getenv("CHUTES_MODELS_EXCLUDE")); env != "" {
		cfg.Chutes.ModelsExclude = splitAndTrim(env)
	}
	if env := strings.TrimSpace(os.Getenv("CHUTES_PRIORITY")); env != "" {
		cfg.Chutes.Priority = strings.ToLower(env)
	}
	if env := strings.TrimSpace(os.Getenv("CHUTES_TEE_PREFERENCE")); env != "" {
		cfg.Chutes.TEEPreference = strings.ToLower(env)
	}
	if env := strings.TrimSpace(os.Getenv("CHUTES_PROXY_URL")); env != "" {
		cfg.Chutes.ProxyURL = env
	}
	if env := strings.TrimSpace(os.Getenv("CHUTES_MAX_RETRIES")); env != "" {
		if parsed, errParse := strconv.Atoi(env); errParse == nil {
			if parsed < 0 {
				parsed = 0
			}
			cfg.Chutes.MaxRetries = parsed
		}
	}
	if env := strings.TrimSpace(os.Getenv("CHUTES_RETRY_BACKOFF")); env != "" {
		cfg.Chutes.RetryBackoff = env
	}

	// Load generic managed providers from env. Env wins by provider name.
	if env := strings.TrimSpace(os.Getenv("MANAGED_PROVIDERS_JSON")); env != "" {
		var providers []ManagedProviderConfig
		if err := json.Unmarshal([]byte(env), &providers); err != nil {
			if !optional {
				return nil, fmt.Errorf("failed to parse MANAGED_PROVIDERS_JSON: %w", err)
			}
		} else {
			cfg.ManagedProviders = MergeManagedProviders(cfg.ManagedProviders, providers)
		}
	}

	if env := strings.TrimSpace(os.Getenv("SECRET_DLP_DEFAULT_PROVIDER_POLICY")); env != "" {
		cfg.SecretDLP.DefaultProviderPolicy = strings.ToLower(env)
	}
	if env := strings.TrimSpace(os.Getenv("SECRET_DLP_PROVIDER_OVERRIDES")); env != "" {
		cfg.SecretDLP.ProviderOverrides = mergeStringMap(cfg.SecretDLP.ProviderOverrides, parseKeyValueList(env))
	}

	// Cursor API key from environment (useful for Railway deployments).
	// Must run BEFORE SanitizeCursorKeys so the env-sourced key gets sanitized.
	if env := strings.TrimSpace(os.Getenv("CURSOR_API_KEY")); env != "" {
		found := false
		for i := range cfg.CursorKey {
			if strings.TrimSpace(cfg.CursorKey[i].APIKey) != "" {
				found = true
				break
			}
		}
		if !found {
			cfg.CursorKey = append(cfg.CursorKey, CursorKey{APIKey: env})
		}
	}
	cfg.SanitizeCursorKeys()

	// Normalize OAuth provider model exclusion map.
	cfg.OAuthExcludedModels = NormalizeOAuthExcludedModels(cfg.OAuthExcludedModels)

	// Load OAuth proxy pools from env (useful for Railway deployments).
	//
	// OAUTH_PROXY_POOL is a semicolon-separated list of provider=urls pairs.
	// Each pair maps a provider name to a CSV list of proxy URLs.
	//
	// Format: "provider1=url1,url2;provider2=url3,url4"
	// Example: "copilot=http://p1:8080,http://p2:8080;codex=http://p3:8080"
	//
	// Env entries are merged into any existing YAML oauth-proxy-pool values;
	// env wins on provider key collision.
	if env := strings.TrimSpace(os.Getenv("OAUTH_PROXY_POOL")); env != "" {
		if cfg.OAuthProxyPool == nil {
			cfg.OAuthProxyPool = make(map[string]string)
		}
		pairs := strings.Split(env, ";")
		for _, pair := range pairs {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			eqIdx := strings.Index(pair, "=")
			if eqIdx < 0 {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(pair[:eqIdx]))
			val := strings.TrimSpace(pair[eqIdx+1:])
			if key != "" && val != "" {
				cfg.OAuthProxyPool[key] = val
			}
		}
	}

	// Normalize OAuth proxy pools.
	cfg.OAuthProxyPool = NormalizeOAuthProxyPool(cfg.OAuthProxyPool)

	// Normalize managed providers and DLP provider policy.
	cfg.ManagedProviders = NormalizeManagedProviders(cfg.ManagedProviders)
	cfg.SecretDLP.DefaultProviderPolicy = strings.ToLower(strings.TrimSpace(cfg.SecretDLP.DefaultProviderPolicy))
	cfg.SecretDLP.ProviderOverrides = normalizeStringMap(cfg.SecretDLP.ProviderOverrides)

	// Normalize global OAuth model name aliases.
	cfg.SanitizeOAuthModelAlias()

	// Normalize global OAuth request-scoped error rules.
	cfg.SanitizeOAuthRequestScopedErrors()

	// Expand route-scoped passthru payload rules into the shared payload rule set.
	cfg.AddPassthruPayloadRules()

	// Validate raw payload rules and drop invalid entries.
	cfg.SanitizePayloadRules()

	if err := cfg.loadRetryPolicies(); err != nil {
		return nil, err
	}

	// Return the populated configuration struct.
	return &cfg, nil
}
