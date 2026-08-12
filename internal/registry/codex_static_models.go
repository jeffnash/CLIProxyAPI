package registry

import "strings"

const codex52Created = 1765440000
const codex53Created = 1770307200
const codex54Created = 1772668800
const codex55Created = 1776902400

func enrichCodexModels(models []*ModelInfo) []*ModelInfo {
	base := cloneModelInfos(models)
	base = appendMissingModelInfos(base, forkAdditionalCodexModels()...)
	return expandReasoningAliases(base)
}

// GetOpenAIModels preserves the fork's legacy helper used by Codex alias tests.
func GetOpenAIModels() []*ModelInfo {
	return GetCodexProModels()
}

func forkAdditionalCodexModels() []*ModelInfo {
	return []*ModelInfo{
		{
			ID:                  "gpt-5.2",
			Object:              "model",
			Created:             codex52Created,
			OwnedBy:             "openai",
			Type:                "openai",
			DisplayName:         "GPT 5.2",
			Version:             "gpt-5.2",
			Description:         "Stable version of GPT 5.2",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking: &ThinkingSupport{
				Levels: []string{"none", "low", "medium", "high", "xhigh"},
			},
		},
		{
			ID:                  "gpt-5.3-codex",
			Object:              "model",
			Created:             codex53Created,
			OwnedBy:             "openai",
			Type:                "openai",
			DisplayName:         "GPT 5.3 Codex",
			Version:             "gpt-5.3",
			Description:         "Stable version of GPT 5.3 Codex, The best model for coding and agentic tasks across domains.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking: &ThinkingSupport{
				Levels: []string{"low", "medium", "high", "xhigh"},
			},
		},
		{
			ID:                  "gpt-5.4-mini",
			Object:              "model",
			Created:             codex54Created,
			OwnedBy:             "openai",
			Type:                "openai",
			DisplayName:         "GPT 5.4 Mini",
			Version:             "gpt-5.4-mini",
			Description:         "Smaller GPT 5.4 model for faster coding and agentic tasks.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking: &ThinkingSupport{
				Levels: []string{"low", "medium", "high", "xhigh"},
			},
		},
		{
			ID:                  "gpt-5.4-nano",
			Object:              "model",
			Created:             codex54Created,
			OwnedBy:             "openai",
			Type:                "openai",
			DisplayName:         "GPT 5.4 Nano",
			Version:             "gpt-5.4-nano",
			Description:         "Lightweight GPT 5.4 model for low-latency coding and agentic tasks.",
			ContextLength:       400000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking: &ThinkingSupport{
				Levels: []string{"low", "medium", "high", "xhigh"},
			},
		},
		{
			ID:                  "gpt-5.5",
			Object:              "model",
			Created:             codex55Created,
			OwnedBy:             "openai",
			Type:                "openai",
			DisplayName:         "GPT 5.5",
			Version:             "gpt-5.5",
			Description:         "Frontier model for complex coding, research, and real-world work.",
			ContextLength:       272000,
			MaxCompletionTokens: 128000,
			SupportedParameters: []string{"tools"},
			Thinking: &ThinkingSupport{
				Levels: []string{"low", "medium", "high", "xhigh"},
			},
		},
	}
}

func appendMissingModelInfos(dst []*ModelInfo, extras ...*ModelInfo) []*ModelInfo {
	seen := make(map[string]struct{}, len(dst))
	for _, model := range dst {
		if model == nil {
			continue
		}
		seen[strings.ToLower(strings.TrimSpace(model.ID))] = struct{}{}
	}
	for _, model := range extras {
		if model == nil {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(model.ID))
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		dst = append(dst, cloneModelInfo(model))
	}
	return dst
}
