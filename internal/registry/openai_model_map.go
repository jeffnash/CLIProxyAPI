package registry

// ToOpenAIModelMap converts the canonical registry ModelInfo into an OpenAI-style model
// JSON object.
//
// This is the single, canonical serializer used for /v1/models responses.
//
// In addition to OpenAI's standard fields (id/object/created/owned_by), this function
// emits the OpenAI-style token limit metadata that downstream clients (notably Letta)
// rely on:
//   - context_length
//   - max_completion_tokens
//
// When provider-native limits are available instead (e.g., Gemini's inputTokenLimit /
// outputTokenLimit), this function falls back to those values.
func ToOpenAIModelMap(info *ModelInfo) map[string]any {
	if info == nil {
		return nil
	}

	result := map[string]any{
		"id":       info.ID,
		"object":   "model",
		"created":  info.Created,
		"owned_by": info.OwnedBy,
	}

	// OpenAI-style limits (preferred) with provider-native fallbacks.
	contextLength := info.ContextLength
	if contextLength <= 0 && info.InputTokenLimit > 0 {
		contextLength = info.InputTokenLimit
	}
	if contextLength > 0 {
		result["context_length"] = contextLength
		// Alias for letta-server compatibility.
		result["context_window"] = contextLength
	}

	maxCompletionTokens := info.MaxCompletionTokens
	if maxCompletionTokens <= 0 && info.OutputTokenLimit > 0 {
		maxCompletionTokens = info.OutputTokenLimit
	}
	if maxCompletionTokens > 0 {
		result["max_completion_tokens"] = maxCompletionTokens
		// Alias for letta-server compatibility.
		result["max_tokens"] = maxCompletionTokens
	}

	// Provider-native limit fields (optional, but useful for debugging / UI).
	if info.InputTokenLimit > 0 {
		result["inputTokenLimit"] = info.InputTokenLimit
	}
	if info.OutputTokenLimit > 0 {
		result["outputTokenLimit"] = info.OutputTokenLimit
	}

	return result
}
