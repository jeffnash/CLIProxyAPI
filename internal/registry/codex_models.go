package registry

const CodexModelPrefix = "codex-"

// GenerateCodexAliases creates codex- prefixed aliases for explicit routing.
// This allows users to explicitly route to Codex when model names might conflict
// with other providers (e.g., "codex-gpt-5.2-xhigh" vs "gpt-5.2-xhigh").
func GenerateCodexAliases(models []*ModelInfo) []*ModelInfo {
	result := make([]*ModelInfo, 0, len(models)*2)
	result = append(result, models...)

	for _, m := range models {
		alias := *m
		alias.ID = CodexModelPrefix + m.ID
		alias.DisplayName = m.DisplayName + " (Codex)"
		alias.Description = m.Description + " - explicit routing alias"
		result = append(result, &alias)
	}

	return result
}
