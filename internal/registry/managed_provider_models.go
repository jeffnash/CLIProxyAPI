package registry

import (
	"strings"
	"time"
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
		})
	}
	return GenerateManagedProviderAliases(baseModels, prefix, label)
}
