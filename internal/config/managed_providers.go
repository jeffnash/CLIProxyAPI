package config

import (
	"strings"
	"time"
)

const (
	ManagedProviderSecretRedactionInherit  = "inherit"
	ManagedProviderSecretRedactionEnabled  = "enabled"
	ManagedProviderSecretRedactionDisabled = "disabled"
)

// MergeManagedProviders merges provider entries by name, with later entries replacing earlier ones.
func MergeManagedProviders(base, overrides []ManagedProviderConfig) []ManagedProviderConfig {
	if len(base) == 0 {
		return NormalizeManagedProviders(overrides)
	}
	out := NormalizeManagedProviders(base)
	index := make(map[string]int, len(out))
	for i := range out {
		if name := ManagedProviderName(out[i]); name != "" {
			index[name] = i
		}
	}
	for _, provider := range NormalizeManagedProviders(overrides) {
		name := ManagedProviderName(provider)
		if name == "" {
			continue
		}
		if existing, ok := index[name]; ok {
			out[existing] = provider
			continue
		}
		index[name] = len(out)
		out = append(out, provider)
	}
	return out
}

// NormalizeManagedProviders trims and normalizes provider names and policy fields.
func NormalizeManagedProviders(providers []ManagedProviderConfig) []ManagedProviderConfig {
	if len(providers) == 0 {
		return nil
	}
	out := make([]ManagedProviderConfig, 0, len(providers))
	for _, provider := range providers {
		provider.Name = ManagedProviderName(provider)
		if provider.Name == "" {
			continue
		}
		provider.Prefix = strings.TrimSpace(provider.Prefix)
		provider.APIKeyEnv = strings.TrimSpace(provider.APIKeyEnv)
		provider.BaseURL = strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
		provider.ClaudeBaseURL = strings.TrimRight(strings.TrimSpace(provider.ClaudeBaseURL), "/")
		provider.AnthropicBaseURL = strings.TrimRight(strings.TrimSpace(provider.AnthropicBaseURL), "/")
		provider.OpenAIBaseURL = strings.TrimRight(strings.TrimSpace(provider.OpenAIBaseURL), "/")
		provider.ClaudeMessagesPath = normalizeManagedProviderPath(provider.ClaudeMessagesPath)
		provider.AnthropicMessagesPath = normalizeManagedProviderPath(provider.AnthropicMessagesPath)
		provider.OpenAIChatPath = normalizeManagedProviderPath(provider.OpenAIChatPath)
		provider.OpenAIResponsesPath = normalizeManagedProviderPath(provider.OpenAIResponsesPath)
		provider.TransportMode = strings.ToLower(strings.TrimSpace(provider.TransportMode))
		provider.DefaultTransport = strings.ToLower(strings.TrimSpace(provider.DefaultTransport))
		provider.Priority = strings.ToLower(strings.TrimSpace(provider.Priority))
		provider.SecretRedaction = NormalizeManagedProviderSecretRedaction(provider.SecretRedaction)
		provider.ModelDiscovery.Path = normalizeManagedProviderPath(provider.ModelDiscovery.Path)
		provider.ModelDiscovery.Format = strings.ToLower(strings.TrimSpace(provider.ModelDiscovery.Format))
		provider.ModelDiscovery.TTL = strings.TrimSpace(provider.ModelDiscovery.TTL)
		out = append(out, provider)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ManagedProviderName returns the normalized provider name used for routing and auth IDs.
func ManagedProviderName(provider ManagedProviderConfig) string {
	return strings.ToLower(strings.TrimSpace(provider.Name))
}

// ManagedProviderPrefix returns the client-visible explicit routing prefix.
func ManagedProviderPrefix(provider ManagedProviderConfig) string {
	if prefix := strings.TrimSpace(provider.Prefix); prefix != "" {
		return prefix
	}
	name := ManagedProviderName(provider)
	if name == "" {
		return ""
	}
	return name + "-"
}

// FindManagedProvider returns a normalized provider by name.
func FindManagedProvider(cfg *SDKConfig, name string) (ManagedProviderConfig, bool) {
	if cfg == nil {
		return ManagedProviderConfig{}, false
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return ManagedProviderConfig{}, false
	}
	for _, provider := range cfg.ManagedProviders {
		if ManagedProviderName(provider) == name {
			return provider, true
		}
	}
	return ManagedProviderConfig{}, false
}

// FindManagedProviderByPrefix returns the provider whose explicit prefix matches model.
func FindManagedProviderByPrefix(cfg *SDKConfig, model string) (ManagedProviderConfig, string, bool) {
	if cfg == nil {
		return ManagedProviderConfig{}, model, false
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	for _, provider := range cfg.ManagedProviders {
		prefix := ManagedProviderPrefix(provider)
		if prefix == "" {
			continue
		}
		if strings.HasPrefix(lower, strings.ToLower(prefix)) {
			return provider, strings.TrimSpace(model[len(prefix):]), true
		}
	}
	return ManagedProviderConfig{}, model, false
}

// ManagedProviderDiscoveryEnabled reports whether /models discovery should be attempted.
func ManagedProviderDiscoveryEnabled(provider ManagedProviderConfig) bool {
	if provider.ModelDiscovery.Enabled == nil {
		return true
	}
	return *provider.ModelDiscovery.Enabled
}

// ManagedProviderModelCacheTTL returns the configured model discovery cache TTL.
func ManagedProviderModelCacheTTL(provider ManagedProviderConfig) time.Duration {
	raw := strings.TrimSpace(provider.ModelDiscovery.TTL)
	if raw == "" {
		return 30 * time.Minute
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 30 * time.Minute
	}
	return d
}

// NormalizeManagedProviderSecretRedaction normalizes inherit/enabled/disabled policy values.
func NormalizeManagedProviderSecretRedaction(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ManagedProviderSecretRedactionEnabled:
		return ManagedProviderSecretRedactionEnabled
	case ManagedProviderSecretRedactionDisabled:
		return ManagedProviderSecretRedactionDisabled
	default:
		return ManagedProviderSecretRedactionInherit
	}
}

func normalizeManagedProviderPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}
