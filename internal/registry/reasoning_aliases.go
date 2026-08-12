package registry

import "strings"

// expandReasoningAliases derives public effort aliases from a model's thinking
// metadata and records the provider-native selection used during execution.
func expandReasoningAliases(models []*ModelInfo, onlyLevels ...string) []*ModelInfo {
	if len(models) == 0 {
		return nil
	}

	allowed := make(map[string]struct{}, len(onlyLevels))
	for _, level := range onlyLevels {
		level = strings.ToLower(strings.TrimSpace(level))
		if level != "" {
			allowed[level] = struct{}{}
		}
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
			if len(allowed) > 0 {
				if _, ok := allowed[level]; !ok {
					continue
				}
			}

			alias := cloneModelInfo(model)
			alias.ID = model.ID + "-" + level
			alias.DisplayName = model.DisplayName + " " + strings.ToUpper(level[:1]) + level[1:]
			alias.Description = model.Description + " (" + level + " reasoning effort)"
			alias.UpstreamID = model.ID
			if strings.TrimSpace(model.UpstreamID) != "" {
				alias.UpstreamID = model.UpstreamID
			}
			alias.ReasoningEffort = level
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
