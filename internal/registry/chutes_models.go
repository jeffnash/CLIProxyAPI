package registry

import (
	"strings"
	"time"
)

// GenerateChutesAliases creates chutes- prefixed aliases for explicit routing.
// Input models should already be deduplicated by root.
func GenerateChutesAliases(models []*ModelInfo) []*ModelInfo {
	result := make([]*ModelInfo, 0, len(models)*2)
	result = append(result, models...)

	for _, m := range models {
		alias := *m
		alias.ID = ChutesModelPrefix + m.ID
		alias.DisplayName = m.DisplayName + " (Chutes)"
		alias.Description = m.Description + " - explicit routing alias"
		result = append(result, &alias)
	}
	return result
}

// NormalizeChutesModelKey creates a simplified lookup key from a model root.
// "deepseek-ai/DeepSeek-V3" → "deepseek-v3"
// "Qwen/Qwen3-32B" → "qwen3-32b"
// "MiniMaxAI/MiniMax-M2.1" → "minimax-m2.1"
func NormalizeChutesModelKey(root string) string {
	s := root
	// Take part after last "/" if present
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		s = s[idx+1:]
	}
	// Lowercase
	s = strings.ToLower(s)
	// Remove common suffixes (keep -tee for variant distinction)
	for _, suffix := range []string{"-instruct", "-2507", "-0324", "-2503", "-2506"} {
		s = strings.TrimSuffix(s, suffix)
	}
	return s
}

// GetChutesModels returns static fallback models for when /v1/models is unavailable.
func GetChutesModels() []*ModelInfo {
	now := time.Now().Unix()
	baseParams := []string{"temperature", "top_p", "max_tokens", "stream", "tools"}

	baseModels := []*ModelInfo{
		{
			ID:                  "deepseek-ai/DeepSeek-V3",
			Object:              "model",
			Created:             now,
			OwnedBy:             "chutes",
			Type:                "chutes",
			DisplayName:         "DeepSeek V3",
			Description:         "DeepSeek V3 via Chutes API",
			ContextLength:       163840,
			MaxCompletionTokens: 65536,
			SupportedParameters: baseParams,
		},
		{
			ID:                  "Qwen/Qwen3-32B",
			Object:              "model",
			Created:             now,
			OwnedBy:             "chutes",
			Type:                "chutes",
			DisplayName:         "Qwen3 32B",
			Description:         "Qwen3 32B via Chutes API",
			ContextLength:       40960,
			MaxCompletionTokens: 40960,
			SupportedParameters: baseParams,
		},
		// Add more fallbacks as needed
	}
	return GenerateChutesAliases(baseModels)
}
