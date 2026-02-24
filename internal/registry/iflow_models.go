package registry

// GenerateIFlowAliases creates iflow- prefixed aliases for explicit routing.
// This allows users to force provider selection even when a model ID overlaps
// with other providers.
func GenerateIFlowAliases(models []*ModelInfo) []*ModelInfo {
	result := make([]*ModelInfo, 0, len(models)*2)
	result = append(result, models...)

	for _, m := range models {
		alias := *m
		alias.ID = IFlowModelPrefix + m.ID
		alias.DisplayName = m.DisplayName + " (iFlow)"
		alias.Description = m.Description + " - explicit routing alias"
		result = append(result, &alias)
	}

	return result
}
