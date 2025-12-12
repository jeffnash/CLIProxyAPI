// Package chat_completions provides response translation functionality for Grok to OpenAI API compatibility.
// This package handles the conversion of Grok API responses (JSON-lines format) into OpenAI Chat Completions-compatible
// JSON format. Grok responses contain incremental tokens at result.response.token and final messages at
// result.response.modelResponse.message. The translator supports both streaming (SSE chunks) and non-streaming modes.
package chat_completions

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type convertGrokResponseToOpenAIParams struct {
	ResponseID           string
	CreatedAt            int64
	HasEmittedFirstChunk bool
	InThinking           bool
	// Tool call buffering (Option A: buffer everything when tools present)
	ContentBuffer   strings.Builder // Buffer ALL streaming content
	HasTools        bool            // Whether the original request had tools
	IsBuffering     bool            // Whether we're in buffering mode
	StreamCompleted bool            // Whether we've seen the final message
}

type grokConfigCtxKey struct{}
type toolsCtxKey struct{}

// WithGrokConfig attaches Grok configuration to the translation context.
func WithGrokConfig(ctx context.Context, cfg *config.Config) context.Context {
	return context.WithValue(ctx, grokConfigCtxKey{}, cfg)
}

// WithToolsFlag attaches a flag indicating whether the request has tools.
func WithToolsFlag(ctx context.Context, hasTools bool) context.Context {
	return context.WithValue(ctx, toolsCtxKey{}, hasTools)
}

func grokConfigFromContext(ctx context.Context) *config.Config {
	if ctx == nil {
		return nil
	}
	if cfg, ok := ctx.Value(grokConfigCtxKey{}).(*config.Config); ok {
		return cfg
	}
	return nil
}

func hasToolsFromContext(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if hasTools, ok := ctx.Value(toolsCtxKey{}).(bool); ok {
		return hasTools
	}
	return false
}

func ConvertGrokResponseToOpenAI(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []string {
	if strings.TrimSpace(string(rawJSON)) == "[DONE]" {
		return []string{}
	}

	state := ensureGrokOpenAIParams(param)
	cfg := grokConfigFromContext(ctx)
	showThinking := true
	var filtered []string
	if cfg != nil {
		showThinking = cfg.Grok.ShowThinkingValue()
		filtered = cfg.Grok.FilteredTags
	}

	// Check if original request has tools - if so, we need to buffer everything
	if !state.IsBuffering && originalRequestRawJSON != nil {
		tools := gjson.GetBytes(originalRequestRawJSON, "tools")
		if tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
			state.HasTools = true
			state.IsBuffering = true
		}
	}

	parsed := gjson.ParseBytes(rawJSON)
	resp := parsed.Get("result.response")
	if !resp.Exists() {
		return []string{}
	}

	contentParts := make([]string, 0)
	finishReason := ""

	if video := resp.Get("streamingVideoGenerationResponse"); video.Exists() {
		if videoURL := strings.TrimSpace(video.Get("videoUrl").String()); videoURL != "" {
			contentParts = append(contentParts, formatVideoContent(videoURL))
			finishReason = "stop"
		}
	}

	if images := resp.Get("modelResponse.generatedImageUrls"); images.Exists() {
		if formatted := formatImageContent(images.Array(), cfg); formatted != "" {
			contentParts = append(contentParts, formatted)
		}
	}

	// Keep the original token with its whitespace - spaces between words are important
	token := resp.Get("token").String()
	token = filterToken(token, filtered)
	currentThinking := resp.Get("isThinking").Bool()
	if token != "" || state.InThinking && !currentThinking {
		if wrapped := wrapThinking(token, currentThinking, state, showThinking); wrapped != "" {
			contentParts = append(contentParts, wrapped)
		}
	}

	if web := resp.Get("webSearchResults"); web.Exists() && showThinking {
		if formatted := formatWebSearch(web); formatted != "" {
			contentParts = append(contentParts, formatted)
		}
	}

	// Check for final message (stream completion signal)
	if finalMessage := strings.TrimSpace(resp.Get("modelResponse.message").String()); finalMessage != "" {
		finishReason = "stop"
		state.StreamCompleted = true
	}

	content := strings.Join(contentParts, "")

	// Set metadata
	setGrokResponseMetadata(parsed, state)

	// Option A: If we have tools, buffer everything and emit at the end
	if state.IsBuffering {
		// Accumulate content
		state.ContentBuffer.WriteString(content)

		// If stream completed, parse and emit
		if state.StreamCompleted {
			bufferedContent := state.ContentBuffer.String()
			return emitBufferedContent(modelName, state, bufferedContent)
		}

		// Still buffering, don't emit anything
		return []string{}
	}

	// Normal streaming (no tools)
	if len(contentParts) == 0 && finishReason == "" {
		return []string{}
	}

	chunk := buildOpenAIStreamChunk(modelName, state.ResponseID, state.CreatedAt, content, finishReason)
	if !state.HasEmittedFirstChunk {
		chunk, _ = sjson.Set(chunk, "choices.0.delta.role", "assistant")
	}

	state.HasEmittedFirstChunk = true

	return []string{chunk}
}

// emitBufferedContent parses buffered content for tool calls and emits appropriate chunks
func emitBufferedContent(modelName string, state *convertGrokResponseToOpenAIParams, content string) []string {
	toolCalls, cleanedContent := extractToolCalls(content)

	var results []string

	// If we have text content before/after tool calls, emit it
	if cleanedContent != "" {
		chunk := buildOpenAIStreamChunk(modelName, state.ResponseID, state.CreatedAt, cleanedContent, "")
		if !state.HasEmittedFirstChunk {
			chunk, _ = sjson.Set(chunk, "choices.0.delta.role", "assistant")
			state.HasEmittedFirstChunk = true
		}
		results = append(results, chunk)
	}

	// Emit tool calls if present
	if len(toolCalls) > 0 {
		chunk := buildOpenAIStreamChunkWithToolCalls(modelName, state.ResponseID, state.CreatedAt, toolCalls, "tool_calls")
		if !state.HasEmittedFirstChunk {
			chunk, _ = sjson.Set(chunk, "choices.0.delta.role", "assistant")
			state.HasEmittedFirstChunk = true
		}
		results = append(results, chunk)
	} else {
		// No tool calls, emit finish
		finishChunk := buildOpenAIStreamChunk(modelName, state.ResponseID, state.CreatedAt, "", "stop")
		results = append(results, finishChunk)
	}

	return results
}

func ConvertGrokResponseToOpenAINonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) string {
	lines := strings.Split(string(rawJSON), "\n")

	var (
		responseID string
		createdAt  int64
		content    []string
		finish     string
	)

	cfg := grokConfigFromContext(ctx)
	showThinking := true
	var filtered []string
	if cfg != nil {
		showThinking = cfg.Grok.ShowThinkingValue()
		filtered = cfg.Grok.FilteredTags
	}
	state := ensureGrokOpenAIParams(param)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		parsed := gjson.Parse(line)

		if responseID == "" {
			responseID = parsed.Get("result.response.id").String()
			if responseID == "" {
				responseID = parsed.Get("id").String()
			}
		}

		if createdAt == 0 {
			createdAt = parsed.Get("result.response.created").Int()
		}

		resp := parsed.Get("result.response")
		if !resp.Exists() {
			continue
		}

		if video := resp.Get("streamingVideoGenerationResponse"); video.Exists() {
			if videoURL := strings.TrimSpace(video.Get("videoUrl").String()); videoURL != "" {
				content = []string{formatVideoContent(videoURL)}
				finish = "stop"
				break
			}
		}

		if images := resp.Get("modelResponse.generatedImageUrls"); images.Exists() {
			if formatted := formatImageContent(images.Array(), cfg); formatted != "" {
				content = append(content, formatted)
			}
		}

		// Keep the original token with its whitespace - spaces between words are important
		token := resp.Get("token").String()
		token = filterToken(token, filtered)
		currentThinking := resp.Get("isThinking").Bool()
		if token != "" || state.InThinking && !currentThinking {
			if wrapped := wrapThinking(token, currentThinking, state, showThinking); wrapped != "" {
				content = append(content, wrapped)
			}
		}

		if web := resp.Get("webSearchResults"); web.Exists() && showThinking {
			if formatted := formatWebSearch(web); formatted != "" {
				content = append(content, formatted)
			}
		}

		if finalMessage := strings.TrimSpace(resp.Get("modelResponse.message").String()); finalMessage != "" {
			content = append(content, finalMessage)
			finish = "stop"
		}
	}

	if responseID == "" {
		responseID = uuid.New().String()
	}

	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}

	return buildOpenAINonStreamResponse(modelName, responseID, createdAt, strings.Join(content, ""), finish)
}

func buildOpenAIStreamChunk(modelName, responseID string, createdAt int64, content string, finishReason string) string {
	json := `{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{"content":""},"finish_reason":null}]}`

	json, _ = sjson.Set(json, "id", responseID)
	json, _ = sjson.Set(json, "model", modelName)
	json, _ = sjson.Set(json, "created", createdAt)
	json, _ = sjson.Set(json, "choices.0.delta.content", content)

	if finishReason != "" {
		json, _ = sjson.Set(json, "choices.0.finish_reason", finishReason)
	}

	return json
}

func buildOpenAIStreamChunkWithToolCalls(modelName, responseID string, createdAt int64, toolCalls []map[string]interface{}, finishReason string) string {
	json := `{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{"tool_calls":[]},"finish_reason":null}]}`

	json, _ = sjson.Set(json, "id", responseID)
	json, _ = sjson.Set(json, "model", modelName)
	json, _ = sjson.Set(json, "created", createdAt)

	// Convert tool calls to streaming format
	var streamToolCalls []map[string]interface{}
	for i, tc := range toolCalls {
		fn := tc["function"].(map[string]string)
		streamTC := map[string]interface{}{
			"index": i,
			"id":    tc["id"],
			"type":  "function",
			"function": map[string]string{
				"name":      fn["name"],
				"arguments": fn["arguments"],
			},
		}
		streamToolCalls = append(streamToolCalls, streamTC)
	}

	json, _ = sjson.Set(json, "choices.0.delta.tool_calls", streamToolCalls)

	if finishReason != "" {
		json, _ = sjson.Set(json, "choices.0.finish_reason", finishReason)
	}

	return json
}

func buildOpenAINonStreamResponse(modelName, responseID string, createdAt int64, content string, finishReason string) string {
	if finishReason == "" {
		finishReason = "stop"
	}

	// Check for tool calls in the content
	toolCalls, cleanedContent := extractToolCalls(content)

	json := `{"id":"","object":"chat.completion","created":0,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`

	json, _ = sjson.Set(json, "id", responseID)
	json, _ = sjson.Set(json, "model", modelName)
	json, _ = sjson.Set(json, "created", createdAt)
	json, _ = sjson.Set(json, "choices.0.message.content", cleanedContent)

	if len(toolCalls) > 0 {
		json, _ = sjson.Set(json, "choices.0.message.tool_calls", toolCalls)
		json, _ = sjson.Set(json, "choices.0.finish_reason", "tool_calls")
	} else {
		json, _ = sjson.Set(json, "choices.0.finish_reason", finishReason)
	}

	return json
}

// extractToolCalls parses <tool_call>...</tool_call> tags from content and returns
// OpenAI-format tool calls and the cleaned content (with tool_call tags removed)
func extractToolCalls(content string) ([]map[string]interface{}, string) {
	var toolCalls []map[string]interface{}
	cleanedContent := content
	toolCallIndex := 0

	startTag := "<tool_call>"
	endTag := "</tool_call>"

	// Find all tool_call tags - handle nested content by finding valid JSON
	for {
		startIdx := strings.Index(cleanedContent, startTag)
		if startIdx == -1 {
			break
		}

		jsonStart := startIdx + len(startTag)

		// Find the matching </tool_call> by looking for valid JSON object end
		// The JSON starts with { and we need to find its matching }
		jsonStr, endIdx := extractJSONObject(cleanedContent[jsonStart:])
		if jsonStr == "" || endIdx == -1 {
			// Couldn't find valid JSON, try simple approach as fallback
			simpleEnd := strings.Index(cleanedContent[startIdx+len(startTag):], endTag)
			if simpleEnd == -1 {
				// No closing tag found - remove the dangling start tag to prevent leaking
				cleanedContent = cleanedContent[:startIdx] + cleanedContent[startIdx+len(startTag):]
				continue
			}
			endIdx = simpleEnd
			jsonStr = strings.TrimSpace(cleanedContent[jsonStart : jsonStart+endIdx])
		}

		// Find the actual </tool_call> after the JSON
		remainingAfterJSON := cleanedContent[jsonStart+endIdx:]
		closeTagIdx := strings.Index(remainingAfterJSON, endTag)
		if closeTagIdx == -1 {
			// No closing tag - remove start tag and continue
			cleanedContent = cleanedContent[:startIdx] + cleanedContent[startIdx+len(startTag):]
			continue
		}

		// Calculate absolute end position
		absoluteEnd := jsonStart + endIdx + closeTagIdx + len(endTag)

		// Parse the tool call JSON
		toolCall := parseToolCallJSON(jsonStr, toolCallIndex)
		if toolCall != nil {
			toolCalls = append(toolCalls, toolCall)
			toolCallIndex++
		}

		// Remove this tool_call from content (even if parse failed, remove the tags)
		fullMatch := cleanedContent[startIdx:absoluteEnd]
		cleanedContent = strings.Replace(cleanedContent, fullMatch, "", 1)
	}

	// Clean up any orphaned end tags (can happen if content has nested tool_call text)
	for strings.Contains(cleanedContent, endTag) {
		cleanedContent = strings.Replace(cleanedContent, endTag, "", 1)
	}

	// Clean up any leftover whitespace
	cleanedContent = strings.TrimSpace(cleanedContent)

	return toolCalls, cleanedContent
}

// extractJSONObject finds a complete JSON object starting from the beginning of the string
// Returns the JSON string and the index after the closing brace
func extractJSONObject(s string) (string, int) {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s[0] != '{' {
		return "", -1
	}

	depth := 0
	inString := false
	escaped := false

	for i := 0; i < len(s); i++ {
		c := s[i]

		if escaped {
			escaped = false
			continue
		}

		if c == '\\' && inString {
			escaped = true
			continue
		}

		if c == '"' {
			inString = !inString
			continue
		}

		if inString {
			continue
		}

		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return s[:i+1], i + 1
			}
		}
	}

	return "", -1
}

// parseToolCallJSON parses a tool call JSON string into OpenAI format
func parseToolCallJSON(jsonStr string, index int) map[string]interface{} {
	parsed := gjson.Parse(jsonStr)
	if !parsed.IsObject() {
		return nil
	}

	// Get tool name (try multiple possible field names)
	toolName := parsed.Get("tool_name").String()
	if toolName == "" {
		toolName = parsed.Get("name").String()
	}
	if toolName == "" {
		return nil
	}

	// Get call ID
	callID := parsed.Get("call_id").String()
	if callID == "" {
		callID = parsed.Get("id").String()
	}
	if callID == "" {
		callID = fmt.Sprintf("call_%s_%d", uuid.New().String()[:8], index)
	}

	// Get arguments - handle both object and string formats
	args := parsed.Get("arguments")
	argsStr := "{}"
	if args.Exists() {
		if args.IsObject() || args.IsArray() {
			argsStr = args.Raw
		} else if args.Type == gjson.String {
			argsStr = args.String()
		}
	}

	return map[string]interface{}{
		"id":   callID,
		"type": "function",
		"function": map[string]string{
			"name":      toolName,
			"arguments": argsStr,
		},
	}
}

func ensureGrokOpenAIParams(param *any) *convertGrokResponseToOpenAIParams {
	state := &convertGrokResponseToOpenAIParams{
		ResponseID: uuid.New().String(),
		CreatedAt:  time.Now().Unix(),
	}

	if param == nil {
		return state
	}

	if existing, ok := (*param).(*convertGrokResponseToOpenAIParams); ok && existing != nil {
		return existing
	}

	*param = state
	return state
}

func setGrokResponseMetadata(parsed gjson.Result, state *convertGrokResponseToOpenAIParams) {
	if state.ResponseID == "" {
		state.ResponseID = parsed.Get("result.response.id").String()
		if state.ResponseID == "" {
			state.ResponseID = parsed.Get("id").String()
		}
		if state.ResponseID == "" {
			state.ResponseID = uuid.New().String()
		}
	}

	if state.CreatedAt == 0 {
		state.CreatedAt = parsed.Get("result.response.created").Int()
		if state.CreatedAt == 0 {
			state.CreatedAt = time.Now().Unix()
		}
	}
}

func filterToken(token string, filtered []string) string {
	if token == "" || len(filtered) == 0 {
		return token
	}
	for _, tag := range filtered {
		if tag != "" && strings.Contains(token, tag) {
			return ""
		}
	}
	return token
}

func wrapThinking(content string, currentThinking bool, state *convertGrokResponseToOpenAIParams, showThinking bool) string {
	prefix, suffix := "", ""
	if !showThinking && currentThinking {
		state.InThinking = currentThinking
		return ""
	}

	if showThinking {
		if !state.InThinking && currentThinking {
			prefix = "<think>\n"
		} else if state.InThinking && !currentThinking {
			suffix = "\n</think>"
		}
	}
	state.InThinking = currentThinking

	if content == "" && prefix == "" && suffix != "" {
		return suffix
	}
	return prefix + content + suffix
}

func formatVideoContent(videoURL string) string {
	url := strings.TrimSpace(videoURL)
	if url == "" {
		return ""
	}
	if !strings.HasPrefix(url, "http") {
		if !strings.HasPrefix(url, "/") {
			url = "/" + url
		}
		url = "https://assets.grok.com" + url
	}
	return `<video src="` + url + `" controls="controls"></video>`
}

func formatImageContent(images []gjson.Result, cfg *config.Config) string {
	if len(images) == 0 {
		return ""
	}

	mode := "url"
	if cfg != nil && strings.TrimSpace(cfg.Grok.ImageMode) != "" {
		mode = strings.ToLower(cfg.Grok.ImageMode)
	}

	builder := strings.Builder{}
	for _, img := range images {
		path := strings.TrimSpace(img.String())
		if path == "" {
			continue
		}
		if !strings.HasPrefix(path, "http") {
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			path = "https://assets.grok.com" + path
		}
		if mode == "html" {
			builder.WriteString(`<img src="`)
			builder.WriteString(path)
			builder.WriteString(`" alt="Generated Image"/>`)
		} else {
			builder.WriteString("![Generated Image](")
			builder.WriteString(path)
			builder.WriteString(")")
		}
		builder.WriteString("\n")
	}
	return strings.TrimSpace(builder.String())
}

func formatWebSearch(web gjson.Result) string {
	results := web.Get("results")
	if !results.Exists() || !results.IsArray() {
		return ""
	}

	var builder strings.Builder
	for _, res := range results.Array() {
		title := strings.TrimSpace(res.Get("title").String())
		url := strings.TrimSpace(res.Get("url").String())
		preview := strings.TrimSpace(res.Get("preview").String())
		if title == "" && url == "" {
			continue
		}

		builder.WriteString("- [")
		if title != "" {
			builder.WriteString(title)
		} else {
			builder.WriteString("result")
		}
		builder.WriteString("](")
		if url != "" {
			builder.WriteString(url)
		} else {
			builder.WriteString("#")
		}
		if preview != "" {
			builder.WriteString(` "`)
			builder.WriteString(strings.ReplaceAll(preview, "\n", " "))
			builder.WriteString(`"`)
		}
		builder.WriteString(")\n")
	}
	return builder.String()
}
