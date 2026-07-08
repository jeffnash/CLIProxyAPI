package registry

// GenerateCursorAliases creates cursor- prefixed aliases for explicit routing.
// Same pattern as GenerateCopilotAliases / GenerateCodexAliases: when a bare model
// id is shared with another provider (e.g. xAI also has "grok-4.5"), clients can
// request "cursor-grok-4.5" to force the Cursor provider. handlers.go recognizes
// CursorModelPrefix, strips it, and sets forced_provider=true.
//
// Grok 4.3 has no Cursor variant — bare "grok-4.3" only exists under xAI, so no
// disambiguation is needed for that model.
func GenerateCursorAliases(models []*ModelInfo) []*ModelInfo {
	result := make([]*ModelInfo, 0, len(models)*2)
	result = append(result, models...)

	for _, m := range models {
		if m == nil {
			continue
		}
		// Avoid double-prefixing if an entry is already a cursor- alias.
		if len(m.ID) >= len(CursorModelPrefix) && m.ID[:len(CursorModelPrefix)] == CursorModelPrefix {
			continue
		}
		alias := *m
		alias.ID = CursorModelPrefix + m.ID
		alias.DisplayName = m.DisplayName + " (Cursor)"
		alias.Description = m.Description + " - explicit routing alias (forces Cursor provider)"
		result = append(result, &alias)
	}

	return result
}
