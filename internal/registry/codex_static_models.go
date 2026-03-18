package registry

import "strings"

const codex54Created = 1772668800

func enrichCodexModels(models []*ModelInfo) []*ModelInfo {
	base := cloneModelInfos(models)
	base = appendMissingModelInfos(base, forkAdditionalCodexModels()...)
	return expandCodexReasoningAliases(base)
}

// GetOpenAIModels preserves the fork's legacy helper used by Codex alias tests.
func GetOpenAIModels() []*ModelInfo {
	return GetCodexProModels()
}

func forkAdditionalCodexModels() []*ModelInfo {
	return []*ModelInfo{
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

func expandCodexReasoningAliases(models []*ModelInfo) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}

	result := make([]*ModelInfo, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		result = appendUniqueModelInfo(result, seen, model)
		if model.Thinking == nil || len(model.Thinking.Levels) == 0 {
			continue
		}
		for _, level := range model.Thinking.Levels {
			level = strings.ToLower(strings.TrimSpace(level))
			if level == "" {
				continue
			}
			alias := cloneModelInfo(model)
			alias.ID = model.ID + "-" + level
			alias.DisplayName = model.DisplayName + " " + strings.ToUpper(level[:1]) + level[1:]
			alias.Description = model.Description + " (" + level + " reasoning effort)"
			result = appendUniqueModelInfo(result, seen, alias)
		}
	}
	return result
}

func appendUniqueModelInfo(dst []*ModelInfo, seen map[string]struct{}, model *ModelInfo) []*ModelInfo {
	if model == nil {
		return dst
	}
	key := strings.ToLower(strings.TrimSpace(model.ID))
	if key == "" {
		return dst
	}
	if _, exists := seen[key]; exists {
		return dst
	}
	seen[key] = struct{}{}
	return append(dst, cloneModelInfo(model))
}
