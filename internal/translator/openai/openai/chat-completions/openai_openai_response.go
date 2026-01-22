// Package openai provides response translation functionality for Gemini CLI to OpenAI API compatibility.
// This package handles the conversion of Gemini CLI API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"encoding/json"
)

// ConvertOpenAIResponseToOpenAI translates a single chunk of a streaming response from the
// Gemini CLI API format to the OpenAI Chat Completions streaming format.
// It processes various Gemini CLI event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini CLI API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - []string: A slice of strings, each containing an OpenAI-compatible JSON response
func ConvertOpenAIResponseToOpenAI(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []string {
	trimmed := bytes.TrimSpace(rawJSON)
	if len(trimmed) == 0 {
		return []string{}
	}

	// Upstreams that already emit SSE lines will include `data:` prefixes.
	// Strip them if present.
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}

	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return []string{}
	}

	// Only forward JSON objects to OpenAI-compatible streaming clients.
	// This prevents empty lines / SSE metadata (": keep-alive", "event: ...") from being
	// wrapped into `data:` events, which can crash JSON parsers in strict clients.
	if trimmed[0] != '{' || !json.Valid(trimmed) {
		return []string{}
	}

	return []string{string(trimmed)}
}

// ConvertOpenAIResponseToOpenAINonStream converts a non-streaming Gemini CLI response to a non-streaming OpenAI response.
// This function processes the complete Gemini CLI response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response
//   - rawJSON: The raw JSON response from the Gemini CLI API
//   - param: A pointer to a parameter object for the conversion
//
// Returns:
//   - string: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertOpenAIResponseToOpenAINonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) string {
	return string(rawJSON)
}
