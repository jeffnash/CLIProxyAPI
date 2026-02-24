package registry

// GenerateKimiAliases creates kimi- prefixed aliases for explicit routing.
// This allows users to force provider selection even when a model ID is already
// namespaced or overlaps with other providers.
func GenerateKimiAliases(models []*ModelInfo) []*ModelInfo {
	result := make([]*ModelInfo, 0, len(models)*2)
	result = append(result, models...)

	for _, m := range models {
		alias := *m
		alias.ID = KimiModelPrefix + m.ID
		alias.DisplayName = m.DisplayName + " (Kimi)"
		alias.Description = m.Description + " - explicit routing alias"
		result = append(result, &alias)
	}

	return result
}
