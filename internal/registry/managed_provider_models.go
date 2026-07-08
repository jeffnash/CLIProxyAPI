package registry

import (
	"strings"
	"time"
)

const (
	ManagedProviderAnthropicProtocolPrefix = "anthropic-"
	ManagedProviderOpenAIProtocolPrefix    = "openai-"
)

// GenerateManagedProviderAliases creates provider-prefixed aliases for explicit routing.
func GenerateManagedProviderAliases(models []*ModelInfo, prefix, label string) []*ModelInfo {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return models
	}
	result := make([]*ModelInfo, 0, len(models)*2)
	result = append(result, models...)

	for _, m := range models {
		if m == nil || strings.HasPrefix(m.ID, prefix) {
			continue
		}
		alias := *m
		alias.ID = prefix + m.ID
		if alias.DisplayName != "" && label != "" {
			alias.DisplayName = alias.DisplayName + " (" + label + ")"
		}
		if alias.Description != "" {
			alias.Description = alias.Description + " - explicit routing alias"
		}
		result = append(result, &alias)
	}
	return result
}

// GenerateManagedProviderProtocolAliases creates protocol-prefixed aliases for
// provider-prefixed managed-provider models.
func GenerateManagedProviderProtocolAliases(models []*ModelInfo, providerPrefix, label string, protocolPrefixes ...string) []*ModelInfo {
	providerPrefix = strings.TrimSpace(providerPrefix)
	if providerPrefix == "" || len(models) == 0 || len(protocolPrefixes) == 0 {
		return models
	}

	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		if id := strings.TrimSpace(model.ID); id != "" {
			seen[id] = struct{}{}
		}
	}

	result := make([]*ModelInfo, 0, len(models)*(len(protocolPrefixes)+1))
	result = append(result, models...)
	for _, protocolPrefix := range protocolPrefixes {
		protocolPrefix = strings.TrimSpace(protocolPrefix)
		if protocolPrefix == "" {
			continue
		}
		if !strings.HasSuffix(protocolPrefix, "-") {
			protocolPrefix += "-"
		}
		for _, model := range models {
			if model == nil || !strings.HasPrefix(model.ID, providerPrefix) {
				continue
			}
			aliasID := protocolPrefix + model.ID
			if _, ok := seen[aliasID]; ok {
				continue
			}
			alias := *model
			alias.ID = aliasID
			if alias.DisplayName != "" && label != "" {
				alias.DisplayName = alias.DisplayName + " (" + strings.TrimSuffix(protocolPrefix, "-") + " transport)"
			}
			if alias.Description != "" {
				alias.Description = alias.Description + " - explicit transport routing alias"
			}
			seen[aliasID] = struct{}{}
			result = append(result, &alias)
		}
	}
	return result
}

// GetManagedProviderFallbackModels returns static fallback models for a configured provider.
func GetManagedProviderFallbackModels(providerName, prefix, label string, modelIDs []string) []*ModelInfo {
	if len(modelIDs) == 0 {
		return nil
	}
	now := time.Now().Unix()
	baseParams := []string{"temperature", "top_p", "max_tokens", "stream", "tools"}
	baseModels := make([]*ModelInfo, 0, len(modelIDs))
	for _, id := range modelIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		displayName := id
		if label != "" {
			displayName = label + " " + id
		}
		baseModels = append(baseModels, &ModelInfo{
			ID:                  id,
			Object:              "model",
			Created:             now,
			OwnedBy:             providerName,
			Type:                providerName,
			DisplayName:         displayName,
			Description:         displayName + " via " + providerName + " API",
			ContextLength:       DefaultClaudeMaxInputTokens,
			MaxCompletionTokens: DefaultClaudeMaxOutputTokens,
			SupportedParameters: baseParams,
			UpstreamID:          id,
			UserDefined:         true,
		})
	}
	return GenerateManagedProviderAliases(baseModels, prefix, label)
}
