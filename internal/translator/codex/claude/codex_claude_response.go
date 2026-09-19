// Package claude provides response translation functionality for Codex to Claude Code API compatibility.
// This package handles the conversion of Codex API responses into Claude Code-compatible
// Server-Sent Events (SSE) format, implementing a sophisticated state machine that manages
// different response types including text content, thinking processes, and function calls.
// The translation ensures proper sequencing of SSE events and maintains state across
// multiple response chunks to provide a seamless streaming experience.
package claude

import (
	"bytes"
	"context"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var (
	dataTag = []byte("data:")
)

// codexThinkingSummaryPartSeparator joins consecutive reasoning summary parts inside
// the single thinking block that represents one Codex reasoning item.
const codexThinkingSummaryPartSeparator = "\n\n"

// ConvertCodexResponseToClaudeParams holds parameters for response conversion.
type ConvertCodexResponseToClaudeParams struct {
	HasToolCall                bool
	HasEmittedToolUse          bool
	BlockIndex                 int
	HasReceivedArgumentsDelta  bool
	FunctionCallBlockOpen      bool
	FunctionCallBlockCallID    string
	FunctionCallBlockIndex     int
	HasTextDelta               bool
	TextBlockOpen              bool
	ThinkingBlockOpen          bool
	ThinkingStopPending        bool
	ThinkingSignature          string
	ThinkingSummarySeen        bool
	WebSearchToolUseIDs        map[string]struct{}
	WebSearchToolResultIDs     map[string]struct{}
	LastWebSearchToolUseID     string
	PendingFunctionCalls       map[string]*pendingCodexFunctionCall
	LastPendingFunctionCallKey string
	ToolBlockIndexes           map[string]int
	ToolBlockOpen              map[int]bool
	ToolBlockOrder             []int
	ToolArgumentDeltaSeen      map[int]bool
	CurrentToolBlockIndex      int
	HasCurrentToolBlock        bool
	MessageStarted             bool
	MessageStopped             bool
	FunctionCalls              map[string]*codexFunctionCallStream
	FunctionCallQueue          []*codexFunctionCallStream
	ActiveFunctionCall         *codexFunctionCallStream
	LastFunctionCall           *codexFunctionCallStream
	DeferredStreamEvents       [][]byte
}

type pendingCodexFunctionCall struct {
	BlockIndex                int
	CallID                    string
	Arguments                 string
	HasReceivedArgumentsDelta bool
	StartEmitted              bool
}

type codexFunctionCallStream struct {
	CallID                    string
	Name                      string
	BlockIndex                int
	Arguments                 string
	EmittedArgumentsLength    int
	HasReceivedArgumentsDelta bool
	EmitInitialEmptyDelta     bool
	Started                   bool
	Done                      bool
	Closed                    bool
}

// ConvertCodexResponseToClaude performs sophisticated streaming response format conversion.
// This function implements a complex state machine that translates Codex API responses
// into Claude Code-compatible Server-Sent Events (SSE) format. It manages different response types
// and handles state transitions between content blocks, thinking processes, and function calls.
//
// Response type states: 0=none, 1=content, 2=thinking, 3=function
// The function maintains state across multiple calls to ensure proper SSE event sequencing.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Codex API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of Claude Code-compatible JSON responses
func ConvertCodexResponseToClaude(_ context.Context, modelName string, originalRequestRawJSON, _ []byte, rawJSON []byte, param *any) [][]byte {
	if *param == nil {
		*param = &ConvertCodexResponseToClaudeParams{
			BlockIndex: 0,
		}
	}

	if !bytes.HasPrefix(rawJSON, dataTag) {
		return [][]byte{}
	}
	streamEventRawJSON := bytes.Clone(rawJSON)
	rawJSON = bytes.TrimSpace(rawJSON[5:])

	output := make([]byte, 0, 512)
	rootResult := gjson.ParseBytes(rawJSON)
	params := (*param).(*ConvertCodexResponseToClaudeParams)
	cacheScope := codexClaudeToolCallScope(originalRequestRawJSON)
	// Once the Claude message has been stopped, nothing further is valid: a late
	// content block, a duplicate message_stop, or a second response.completed
	// would all reference a closed message. Drop any trailing events. This is a
	// no-op for well-behaved streams, which emit nothing after completed.
	if params.MessageStopped {
		return [][]byte{}
	}

	typeResult := rootResult.Get("type")
	typeStr := typeResult.String()
	if params.ActiveFunctionCall != nil && shouldDeferCodexStreamEvent(typeStr, rootResult) {
		params.DeferredStreamEvents = append(params.DeferredStreamEvents, streamEventRawJSON)
		return [][]byte{}
	}
	var template []byte

	switch typeStr {
	case "error":
		output = append(output, codexStreamErrorToClaudeError(rootResult)...)
	case "response.created":
		// Emit message_start once per stream: a second response.created would
		// reset the client's content-block tracking while our block index keeps
		// climbing. One HTTP stream maps to one Claude message.
		if !params.MessageStarted {
			template = []byte(`{"type":"message_start","message":{"id":"","type":"message","role":"assistant","model":"claude-opus-4-1-20250805","stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0},"content":[],"stop_reason":null}}`)
			template, _ = sjson.SetBytes(template, "message.model", rootResult.Get("response.model").String())
			template, _ = sjson.SetBytes(template, "message.id", rootResult.Get("response.id").String())
			params.MessageStarted = true

			output = translatorcommon.AppendSSEEventBytes(output, "message_start", template, 2)
		}
	case "response.reasoning_summary_part.added":
		output = append(output, stopCodexTextBlock(params)...)
		// Codex splits a single reasoning item into several summary parts, but only
		// output_item.done carries that item's final encrypted_content. Keep one
		// thinking block open for the whole item and separate the parts with a blank
		// line, so the only signature ever emitted is the final one.
		if params.ThinkingBlockOpen {
			output = append(output, appendCodexThinkingDelta(params, codexThinkingSummaryPartSeparator)...)
		} else {
			output = append(output, startCodexThinkingBlock(params)...)
		}
		params.ThinkingSummarySeen = true
	case "response.reasoning_summary_text.delta":
		output = append(output, stopCodexTextBlock(params)...)
		output = append(output, startCodexThinkingBlock(params)...)
		output = append(output, appendCodexThinkingDelta(params, rootResult.Get("delta").String())...)
	case "response.reasoning_summary_part.done":
		// Intentionally does not close the thinking block: it stays open until
		// output_item.done delivers the reasoning item's final encrypted_content.
	case "response.content_part.added":
		output = append(output, finalizeCodexThinkingBlock(params)...)
		if rootResult.Get("part.type").String() == "output_text" {
			output = append(output, startCodexTextBlock(params)...)
		}
	case "response.output_text.delta":
		params.HasTextDelta = true
		output = append(output, finalizeCodexThinkingBlock(params)...)
		output = append(output, startCodexTextBlock(params)...)
		template = []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
		template, _ = sjson.SetBytes(template, "index", params.BlockIndex)
		template, _ = sjson.SetBytes(template, "delta.text", rootResult.Get("delta").String())

		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
	case "response.content_part.done":
		if rootResult.Get("part.type").String() == "output_text" {
			output = append(output, stopCodexTextBlock(params)...)
		}
	case "response.web_search_call.searching", "response.web_search_call.completed", "response.web_search_call.in_progress":
		// Wait for populated web_search_call items on output_item.done.
	case "response.completed", "response.incomplete":
		template = []byte(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":0,"output_tokens":0}}`)
		responseData := rootResult.Get("response")
		output = append(output, finalizeCodexThinkingBlock(params)...)
		output = append(output, stopCodexTextBlock(params)...)
		// Only grok-composer streams force-close tool blocks left open at
		// completion (they can skip the matching *.done events). Other models
		// must not invent a stop for an incomplete block.
		output = appendCodexFunctionCallsFromTerminal(output, params, originalRequestRawJSON, responseData, isGrokComposerClaudeStreamRepairModel(modelName))
		output = appendDeferredCodexStreamEvents(output, originalRequestRawJSON, param)
		output = append(output, finalizeCodexThinkingBlock(params)...)
		output = append(output, stopCodexTextBlock(params)...)
		template, _ = sjson.SetBytes(template, "delta.stop_reason", mapCodexStopReasonToClaude(codexStopReason(responseData), params.HasEmittedToolUse))
		template = setClaudeStopSequence(template, "delta.stop_sequence", responseData)
		inputTokens, outputTokens, cachedTokens, cacheWriteTokens := extractResponsesUsage(responseData.Get("usage"))
		template, _ = sjson.SetBytes(template, "usage.input_tokens", inputTokens)
		template, _ = sjson.SetBytes(template, "usage.output_tokens", outputTokens)
		if cachedTokens > 0 {
			template, _ = sjson.SetBytes(template, "usage.cache_read_input_tokens", cachedTokens)
		}
		if cacheWriteTokens > 0 {
			template, _ = sjson.SetBytes(template, "usage.cache_creation_input_tokens", cacheWriteTokens)
		}
		template = setClaudeReasoningUsage(template, responseData.Get("usage"))

		output = translatorcommon.AppendSSEEventBytes(output, "message_delta", template, 2)
		output = translatorcommon.AppendSSEEventBytes(output, "message_stop", []byte(`{"type":"message_stop"}`), 2)
		params.MessageStopped = true
	case "response.output_item.added":
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		switch itemType {
		case "function_call":
			output = append(output, finalizeCodexThinkingBlock(params)...)
			output = append(output, stopCodexTextBlock(params)...)

			// Remember the call under its Claude-visible (shortened) ID so a
			// later request can repair an orphan tool result referencing it.
			rememberCodexClaudeToolCall(cacheScope, shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(itemResult.Get("call_id").String())), itemResult)

			call := recordCodexFunctionCall(params, rootResult, itemResult)
			updateCodexFunctionCallIdentity(params, call, rootResult, itemResult)
			if call.Name != "" {
				call.EmitInitialEmptyDelta = true
			}
			output = appendCodexFunctionCallQueue(output, params, originalRequestRawJSON)
		case "reasoning":
			output = append(output, stopCodexTextBlock(params)...)
			// A previous reasoning item that never reported output_item.done must not
			// leak its still-open block into this one.
			output = append(output, finalizeCodexThinkingBlock(params)...)
			params.ThinkingSummarySeen = false
			// Kept only as a fallback for streams whose output_item.done omits
			// encrypted_content; it is a pre-content snapshot, never the final value.
			params.ThinkingSignature = itemResult.Get("encrypted_content").String()
		case "web_search_call":
			// Defer server_tool_use until output_item.done carries action/query.
		}
	case "response.output_item.done":
		itemResult := rootResult.Get("item")
		itemType := itemResult.Get("type").String()
		switch itemType {
		case "message":
			if params.HasTextDelta {
				return [][]byte{output}
			}
			contentResult := itemResult.Get("content")
			if !contentResult.Exists() || !contentResult.IsArray() {
				return [][]byte{output}
			}
			var textBuilder strings.Builder
			contentResult.ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() != "output_text" {
					return true
				}
				if txt := part.Get("text").String(); txt != "" {
					textBuilder.WriteString(txt)
				}
				return true
			})
			text := textBuilder.String()
			if text == "" {
				return [][]byte{output}
			}

			output = append(output, finalizeCodexThinkingBlock(params)...)
			output = append(output, startCodexTextBlock(params)...)

			template = []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":""}}`)
			template, _ = sjson.SetBytes(template, "index", params.BlockIndex)
			template, _ = sjson.SetBytes(template, "delta.text", text)
			output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)

			output = append(output, stopCodexTextBlock(params)...)
			params.HasTextDelta = true
		case "function_call":
			output = append(output, finalizeCodexThinkingBlock(params)...)
			output = append(output, stopCodexTextBlock(params)...)
			call := codexFunctionCallForEvent(params, rootResult, itemResult)
			if call == nil {
				call = recordCodexFunctionCall(params, rootResult, itemResult)
			}
			updateCodexFunctionCallIdentity(params, call, rootResult, itemResult)
			updateCodexFunctionCallArguments(call, itemResult.Get("arguments").String(), false)
			call.Done = true
			// The done item carries the final arguments: refresh the cached
			// call so orphan repair restores the complete version.
			rememberCodexClaudeToolCall(cacheScope, shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(itemResult.Get("call_id").String())), itemResult)
			output = appendCodexFunctionCallQueue(output, params, originalRequestRawJSON)
		case "reasoning":
			output = append(output, stopCodexTextBlock(params)...)
			if signature := itemResult.Get("encrypted_content").String(); signature != "" {
				params.ThinkingSignature = signature
			}
			if params.ThinkingSummarySeen {
				output = append(output, finalizeCodexThinkingBlock(params)...)
			} else {
				output = append(output, finalizeCodexSignatureOnlyThinkingBlock(params)...)
			}
			params.ThinkingSignature = ""
			params.ThinkingSummarySeen = false
		case "web_search_call":
			output = appendCodexWebSearchToolResult(output, params, rootResult, itemResult)
		}
	case "response.function_call_arguments.delta":
		call := codexFunctionCallForEvent(params, rootResult, gjson.Result{})
		if call == nil {
			call = recordCodexFunctionCall(params, rootResult, gjson.Result{})
		}
		updateCodexFunctionCallArguments(call, rootResult.Get("delta").String(), true)
		output = appendCodexFunctionCallBufferedArguments(output, params, call)
	case "response.function_call_arguments.done":
		call := codexFunctionCallForEvent(params, rootResult, gjson.Result{})
		if call == nil {
			call = recordCodexFunctionCall(params, rootResult, gjson.Result{})
		}
		updateCodexFunctionCallArguments(call, rootResult.Get("arguments").String(), false)
		output = appendCodexFunctionCallBufferedArguments(output, params, call)
	}

	if len(params.FunctionCallQueue) == 0 {
		output = appendDeferredCodexStreamEvents(output, originalRequestRawJSON, param)
	}
	return [][]byte{output}
}

func shouldDeferCodexStreamEvent(typeStr string, rootResult gjson.Result) bool {
	switch typeStr {
	case "error", "response.completed", "response.incomplete", "response.function_call_arguments.delta", "response.function_call_arguments.done":
		return false
	case "response.output_item.added", "response.output_item.done":
		return rootResult.Get("item.type").String() != "function_call"
	default:
		return true
	}
}

func appendDeferredCodexStreamEvents(output []byte, originalRequestRawJSON []byte, param *any) []byte {
	if param == nil || *param == nil {
		return output
	}
	params := (*param).(*ConvertCodexResponseToClaudeParams)
	if len(params.DeferredStreamEvents) == 0 {
		return output
	}

	events := params.DeferredStreamEvents
	params.DeferredStreamEvents = nil
	for _, event := range events {
		translated := ConvertCodexResponseToClaude(context.Background(), "", originalRequestRawJSON, nil, event, param)
		for _, chunk := range translated {
			output = append(output, chunk...)
		}
	}
	return output
}

func codexStreamErrorToClaudeError(rootResult gjson.Result) []byte {
	errorResult := rootResult.Get("error")
	errType := strings.TrimSpace(errorResult.Get("type").String())
	if errType == "" {
		errType = strings.TrimSpace(rootResult.Get("error_type").String())
	}
	if errType == "" {
		errType = "api_error"
	}

	code := strings.TrimSpace(errorResult.Get("code").String())
	message := strings.TrimSpace(errorResult.Get("message").String())
	if message == "" {
		message = strings.TrimSpace(rootResult.Get("message").String())
	}
	if message == "" {
		message = code
	}
	if message == "" {
		message = errType
	}

	if code == "cyber_policy" || errType == "invalid_request" {
		errType = "invalid_request_error"
	}

	out := []byte(`{"type":"error","error":{"type":"api_error","message":""}}`)
	out, _ = sjson.SetBytes(out, "error.type", errType)
	out, _ = sjson.SetBytes(out, "error.message", message)
	return translatorcommon.AppendSSEEventBytes(nil, "error", out, 2)
}

// ConvertCodexResponseToClaudeNonStream converts a non-streaming Codex response to a non-streaming Claude Code response.
// This function processes the complete Codex response and transforms it into a single Claude Code-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the Claude Code API format.
func ConvertCodexResponseToClaudeNonStream(_ context.Context, _ string, originalRequestRawJSON, _ []byte, rawJSON []byte, _ *any) []byte {
	revNames := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)
	cacheScope := codexClaudeToolCallScope(originalRequestRawJSON)

	rootResult := gjson.ParseBytes(rawJSON)
	typeStr := rootResult.Get("type").String()
	if typeStr != "response.completed" && typeStr != "response.incomplete" {
		return []byte{}
	}

	responseData := rootResult.Get("response")
	if !responseData.Exists() {
		return []byte{}
	}

	out := []byte(`{"id":"","type":"message","role":"assistant","model":"","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}`)
	out, _ = sjson.SetBytes(out, "id", responseData.Get("id").String())
	out, _ = sjson.SetBytes(out, "model", responseData.Get("model").String())
	inputTokens, outputTokens, cachedTokens, cacheWriteTokens := extractResponsesUsage(responseData.Get("usage"))
	out, _ = sjson.SetBytes(out, "usage.input_tokens", inputTokens)
	out, _ = sjson.SetBytes(out, "usage.output_tokens", outputTokens)
	if cachedTokens > 0 {
		out, _ = sjson.SetBytes(out, "usage.cache_read_input_tokens", cachedTokens)
	}
	if cacheWriteTokens > 0 {
		out, _ = sjson.SetBytes(out, "usage.cache_creation_input_tokens", cacheWriteTokens)
	}
	out = setClaudeReasoningUsage(out, responseData.Get("usage"))

	hasToolCall := false
	webSearchSeen := make(map[string]struct{})
	var contentBlocks [][]byte

	if output := responseData.Get("output"); output.Exists() && output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "reasoning":
				thinkingBuilder := strings.Builder{}
				signature := item.Get("encrypted_content").String()
				if summary := item.Get("summary"); summary.Exists() {
					if summary.IsArray() {
						summary.ForEach(func(_, part gjson.Result) bool {
							if txt := part.Get("text"); txt.Exists() {
								thinkingBuilder.WriteString(txt.String())
							} else {
								thinkingBuilder.WriteString(part.String())
							}
							return true
						})
					} else {
						thinkingBuilder.WriteString(summary.String())
					}
				}
				if thinkingBuilder.Len() == 0 {
					if content := item.Get("content"); content.Exists() {
						if content.IsArray() {
							content.ForEach(func(_, part gjson.Result) bool {
								if txt := part.Get("text"); txt.Exists() {
									thinkingBuilder.WriteString(txt.String())
								} else {
									thinkingBuilder.WriteString(part.String())
								}
								return true
							})
						} else {
							thinkingBuilder.WriteString(content.String())
						}
					}
				}
				if thinkingBuilder.Len() > 0 || signature != "" {
					block := []byte(`{"type":"thinking","thinking":""}`)
					block, _ = sjson.SetBytes(block, "thinking", thinkingBuilder.String())
					if signature != "" {
						block, _ = sjson.SetBytes(block, "signature", signature)
					}
					contentBlocks = append(contentBlocks, block)
				}
			case "message":
				if content := item.Get("content"); content.Exists() {
					if content.IsArray() {
						content.ForEach(func(_, part gjson.Result) bool {
							if part.Get("type").String() == "output_text" {
								text := part.Get("text").String()
								if text != "" {
									block := []byte(`{"type":"text","text":""}`)
									block, _ = sjson.SetBytes(block, "text", text)
									contentBlocks = append(contentBlocks, block)
								}
							}
							return true
						})
					} else {
						text := content.String()
						if text != "" {
							block := []byte(`{"type":"text","text":""}`)
							block, _ = sjson.SetBytes(block, "text", text)
							contentBlocks = append(contentBlocks, block)
						}
					}
				}
			case "web_search_call":
				contentBlocks = appendCodexWebSearchNonStreamBlocks(contentBlocks, item, webSearchSeen)
			case "function_call":
				hasToolCall = true
				name := item.Get("name").String()
				if original, ok := revNames[name]; ok {
					name = original
				}

				callID := shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(item.Get("call_id").String()))
				rememberCodexClaudeToolCall(cacheScope, callID, item)
				toolBlock := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
				toolBlock, _ = sjson.SetBytes(toolBlock, "id", callID)
				toolBlock, _ = sjson.SetBytes(toolBlock, "name", name)
				inputRaw := "{}"
				if argsStr := item.Get("arguments").String(); argsStr != "" && gjson.Valid(argsStr) {
					argsJSON := gjson.Parse(argsStr)
					if argsJSON.IsObject() {
						inputRaw = argsJSON.Raw
					}
				}
				toolBlock, _ = sjson.SetRawBytes(toolBlock, "input", []byte(inputRaw))
				contentBlocks = append(contentBlocks, toolBlock)
			}
			return true
		})
	}

	if len(contentBlocks) > 0 {
		out = translatorcommon.SetRawArrayItems(out, "content", contentBlocks)
	}

	out, _ = sjson.SetBytes(out, "stop_reason", mapCodexStopReasonToClaude(codexStopReason(responseData), hasToolCall))
	out = setClaudeStopSequence(out, "stop_sequence", responseData)

	return out
}

func codexStopReason(responseData gjson.Result) string {
	if stopReason := responseData.Get("stop_reason"); stopReason.Exists() && stopReason.String() != "" {
		if stopReason.String() == "stop" && codexStopSequence(responseData).String() != "" {
			return "stop_sequence"
		}
		return stopReason.String()
	}
	if reason := responseData.Get("incomplete_details.reason"); reason.Exists() && reason.String() != "" {
		return reason.String()
	}
	if codexStopSequence(responseData).String() != "" {
		return "stop_sequence"
	}
	return ""
}

func mapCodexStopReasonToClaude(stopReason string, hasToolCall bool) string {
	if hasToolCall {
		return "tool_use"
	}

	switch stopReason {
	case "", "stop", "completed":
		return "end_turn"
	case "max_tokens", "max_output_tokens":
		return "max_tokens"
	case "tool_use", "tool_calls", "function_call":
		// FORK: keep "tool_use" (not upstream's "end_turn"). Upstream maps these
		// labels to end_turn when hasToolCall is false; the fork preserves tool_use
		// so Claude clients that only look at stop_reason still pause for tools.
		return "tool_use"
	case "end_turn", "stop_sequence", "pause_turn", "refusal", "model_context_window_exceeded":
		return stopReason
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func codexStopSequence(responseData gjson.Result) gjson.Result {
	return responseData.Get("stop_sequence")
}

func setClaudeStopSequence(out []byte, path string, responseData gjson.Result) []byte {
	if stopSequence := codexStopSequence(responseData); stopSequence.Exists() && stopSequence.String() != "" {
		out, _ = sjson.SetRawBytes(out, path, []byte(stopSequence.Raw))
	}
	return out
}

func codexFunctionCallKey(rootResult, itemResult gjson.Result) string {
	if outputIndex := rootResult.Get("output_index"); outputIndex.Exists() {
		return "output:" + outputIndex.Raw
	}
	if callID := codexFunctionCallID(itemResult); callID != "" {
		return "call:" + callID
	}
	return "last"
}

func codexFunctionCallID(itemResult gjson.Result) string {
	return itemResult.Get("call_id").String()
}

func codexFunctionCallIDKey(callID string) string {
	if callID == "" {
		return ""
	}
	return "call:" + callID
}

func codexArgumentsFunctionCallKey(params *ConvertCodexResponseToClaudeParams, rootResult gjson.Result) string {
	if outputIndex := rootResult.Get("output_index"); outputIndex.Exists() {
		return "output:" + outputIndex.Raw
	}
	return params.LastPendingFunctionCallKey
}

func recordPendingCodexFunctionCall(params *ConvertCodexResponseToClaudeParams, rootResult, itemResult gjson.Result) {
	if params.PendingFunctionCalls == nil {
		params.PendingFunctionCalls = map[string]*pendingCodexFunctionCall{}
	}

	pending := &pendingCodexFunctionCall{CallID: codexFunctionCallID(itemResult)}
	key := codexFunctionCallKey(rootResult, itemResult)
	params.PendingFunctionCalls[key] = pending
	if callIDKey := codexFunctionCallIDKey(pending.CallID); callIDKey != "" {
		params.PendingFunctionCalls[callIDKey] = pending
	}
	params.LastPendingFunctionCallKey = key
}

func pendingCodexFunctionCallForKey(params *ConvertCodexResponseToClaudeParams, key string) (*pendingCodexFunctionCall, string) {
	if params == nil || params.PendingFunctionCalls == nil || key == "" {
		return nil, ""
	}
	pending, ok := params.PendingFunctionCalls[key]
	if !ok {
		return nil, ""
	}
	return pending, key
}

func pendingCodexFunctionCallForDone(params *ConvertCodexResponseToClaudeParams, rootResult, itemResult gjson.Result) (*pendingCodexFunctionCall, []string) {
	if params == nil || params.PendingFunctionCalls == nil {
		return nil, nil
	}

	keys := []string{codexFunctionCallKey(rootResult, itemResult)}
	callID := codexFunctionCallID(itemResult)
	if callID != "" {
		keys = appendUniqueCodexFunctionCallKey(keys, codexFunctionCallIDKey(callID))
	} else if !rootResult.Get("output_index").Exists() && params.LastPendingFunctionCallKey != "" {
		keys = appendUniqueCodexFunctionCallKey(keys, params.LastPendingFunctionCallKey)
	}

	for _, key := range keys {
		if pending, ok := params.PendingFunctionCalls[key]; ok {
			return pending, keysForPendingCodexFunctionCall(params, pending)
		}
	}
	return nil, nil
}

func codexFunctionCallKeys(rootResult, itemResult gjson.Result) []string {
	keys := make([]string, 0, 5)
	if outputIndex := rootResult.Get("output_index"); outputIndex.Exists() {
		keys = appendUniqueCodexFunctionCallKey(keys, "output:"+outputIndex.Raw)
	}
	if callID := codexFunctionCallID(itemResult); callID != "" {
		keys = appendUniqueCodexFunctionCallKey(keys, "call:"+callID)
	}
	if callID := rootResult.Get("call_id").String(); callID != "" {
		keys = appendUniqueCodexFunctionCallKey(keys, "call:"+callID)
	}
	if itemID := itemResult.Get("id").String(); itemID != "" {
		keys = appendUniqueCodexFunctionCallKey(keys, "item:"+itemID)
	}
	if itemID := rootResult.Get("item_id").String(); itemID != "" {
		keys = appendUniqueCodexFunctionCallKey(keys, "item:"+itemID)
	}
	return keys
}

func appendUniqueCodexFunctionCallKey(keys []string, key string) []string {
	if key == "" {
		return keys
	}
	for _, existing := range keys {
		if existing == key {
			return keys
		}
	}
	return append(keys, key)
}

func keysForPendingCodexFunctionCall(params *ConvertCodexResponseToClaudeParams, pending *pendingCodexFunctionCall) []string {
	if params == nil || pending == nil || params.PendingFunctionCalls == nil {
		return nil
	}

	keys := make([]string, 0, 2)
	for key, candidate := range params.PendingFunctionCalls {
		if candidate == pending {
			keys = append(keys, key)
		}
	}
	return keys
}

func deletePendingCodexFunctionCallAliases(params *ConvertCodexResponseToClaudeParams, keys []string) {
	if params == nil || params.PendingFunctionCalls == nil {
		return
	}
	for _, key := range keys {
		delete(params.PendingFunctionCalls, key)
		if params.LastPendingFunctionCallKey == key {
			params.LastPendingFunctionCallKey = ""
		}
	}
}

func codexFunctionCallForKeys(params *ConvertCodexResponseToClaudeParams, keys []string) *codexFunctionCallStream {
	if params == nil || params.FunctionCalls == nil {
		return nil
	}
	for _, key := range keys {
		if call := params.FunctionCalls[key]; call != nil {
			return call
		}
	}
	return nil
}

func codexFunctionCallForEvent(params *ConvertCodexResponseToClaudeParams, rootResult, itemResult gjson.Result) *codexFunctionCallStream {
	keys := codexFunctionCallKeys(rootResult, itemResult)
	if len(keys) > 0 {
		return codexFunctionCallForKeys(params, keys)
	}
	if params == nil {
		return nil
	}
	return params.LastFunctionCall
}

func recordCodexFunctionCall(params *ConvertCodexResponseToClaudeParams, rootResult, itemResult gjson.Result) *codexFunctionCallStream {
	keys := codexFunctionCallKeys(rootResult, itemResult)
	call := codexFunctionCallForKeys(params, keys)
	if call == nil {
		call = &codexFunctionCallStream{BlockIndex: -1}
		params.FunctionCallQueue = append(params.FunctionCallQueue, call)
	}
	addCodexFunctionCallAliases(params, call, keys)
	params.LastFunctionCall = call
	return call
}

func addCodexFunctionCallAliases(params *ConvertCodexResponseToClaudeParams, call *codexFunctionCallStream, keys []string) {
	if params == nil || call == nil {
		return
	}
	if params.FunctionCalls == nil {
		params.FunctionCalls = map[string]*codexFunctionCallStream{}
	}
	for _, key := range keys {
		params.FunctionCalls[key] = call
	}
}

func updateCodexFunctionCallIdentity(params *ConvertCodexResponseToClaudeParams, call *codexFunctionCallStream, rootResult, itemResult gjson.Result) {
	if call == nil {
		return
	}
	if callID := codexFunctionCallID(itemResult); callID != "" {
		call.CallID = callID
	}
	if name := itemResult.Get("name").String(); name != "" {
		call.Name = name
	}
	addCodexFunctionCallAliases(params, call, codexFunctionCallKeys(rootResult, itemResult))
}

func updateCodexFunctionCallArguments(call *codexFunctionCallStream, arguments string, delta bool) {
	if call == nil || arguments == "" {
		return
	}
	if delta {
		call.Arguments += arguments
		call.HasReceivedArgumentsDelta = true
		return
	}
	if !call.HasReceivedArgumentsDelta {
		call.Arguments = arguments
		return
	}
	if strings.HasPrefix(arguments, call.Arguments) {
		call.Arguments = arguments
	}
}

func appendCodexFunctionCallStart(output []byte, originalRequestRawJSON []byte, callID, name string, blockIndex int) []byte {
	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"","name":"","input":{}}}`)
	template, _ = sjson.SetBytes(template, "index", blockIndex)
	template, _ = sjson.SetBytes(template, "content_block.id", shortenCodexCallIDIfNeeded(util.SanitizeClaudeToolID(callID)))
	template, _ = sjson.SetBytes(template, "content_block.name", resolveCodexClaudeToolUseName(originalRequestRawJSON, name))
	return translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)
}

func appendCodexFunctionCallArgumentDelta(output []byte, partialJSON string, blockIndex int) []byte {
	template := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`)
	template, _ = sjson.SetBytes(template, "index", blockIndex)
	template, _ = sjson.SetBytes(template, "delta.partial_json", partialJSON)
	return translatorcommon.AppendSSEEventBytes(output, "content_block_delta", template, 2)
}

func appendCodexFunctionCallStop(output []byte, blockIndex int) []byte {
	template := []byte(`{"type":"content_block_stop","index":0}`)
	template, _ = sjson.SetBytes(template, "index", blockIndex)
	return translatorcommon.AppendSSEEventBytes(output, "content_block_stop", template, 2)
}

func appendCodexFunctionCallBufferedArguments(output []byte, params *ConvertCodexResponseToClaudeParams, call *codexFunctionCallStream) []byte {
	if params == nil || call == nil || params.ActiveFunctionCall != call || !call.Started || call.Closed {
		return output
	}
	if call.EmittedArgumentsLength >= len(call.Arguments) {
		return output
	}

	output = appendCodexFunctionCallArgumentDelta(output, call.Arguments[call.EmittedArgumentsLength:], call.BlockIndex)
	call.EmittedArgumentsLength = len(call.Arguments)
	return output
}

func appendCodexFunctionCallQueue(output []byte, params *ConvertCodexResponseToClaudeParams, originalRequestRawJSON []byte) []byte {
	if params == nil {
		return output
	}

	for {
		if active := params.ActiveFunctionCall; active != nil {
			output = appendCodexFunctionCallBufferedArguments(output, params, active)
			if !active.Done {
				return output
			}
			output = appendCodexFunctionCallStop(output, active.BlockIndex)
			if params.BlockIndex <= active.BlockIndex {
				params.BlockIndex = active.BlockIndex + 1
			}
			active.Closed = true
			params.ActiveFunctionCall = nil
			removeCodexFunctionCallFromQueue(params, active)
		}

		for len(params.FunctionCallQueue) > 0 && params.FunctionCallQueue[0].Closed {
			params.FunctionCallQueue = params.FunctionCallQueue[1:]
		}
		if len(params.FunctionCallQueue) == 0 {
			return output
		}

		call := params.FunctionCallQueue[0]
		if call.Name == "" {
			return output
		}

		call.BlockIndex = params.BlockIndex
		output = appendCodexFunctionCallStart(output, originalRequestRawJSON, call.CallID, call.Name, call.BlockIndex)
		if call.EmitInitialEmptyDelta {
			output = appendCodexFunctionCallArgumentDelta(output, "", call.BlockIndex)
		}
		call.Started = true
		params.ActiveFunctionCall = call
		params.HasEmittedToolUse = true
		output = appendCodexFunctionCallBufferedArguments(output, params, call)
	}
}

func removeCodexFunctionCallFromQueue(params *ConvertCodexResponseToClaudeParams, call *codexFunctionCallStream) {
	if params == nil || call == nil {
		return
	}
	for index, queued := range params.FunctionCallQueue {
		if queued != call {
			continue
		}
		params.FunctionCallQueue = append(params.FunctionCallQueue[:index], params.FunctionCallQueue[index+1:]...)
		return
	}
}

func appendCodexFunctionCallsFromTerminal(output []byte, params *ConvertCodexResponseToClaudeParams, originalRequestRawJSON []byte, responseData gjson.Result, forceCloseOpenCalls bool) []byte {
	if params == nil {
		return output
	}

	responseData.Get("output").ForEach(func(index, item gjson.Result) bool {
		if item.Get("type").String() != "function_call" {
			return true
		}

		keys := codexFunctionCallKeys(gjson.Result{}, item)
		if itemOutputIndex := item.Get("output_index"); itemOutputIndex.Exists() {
			keys = appendUniqueCodexFunctionCallKey(keys, "output:"+itemOutputIndex.Raw)
		}
		if index.Exists() {
			keys = appendUniqueCodexFunctionCallKey(keys, "output:"+index.String())
		}
		call := codexFunctionCallForKeys(params, keys)
		if call == nil {
			call = &codexFunctionCallStream{BlockIndex: -1}
			params.FunctionCallQueue = append(params.FunctionCallQueue, call)
		}
		addCodexFunctionCallAliases(params, call, keys)
		updateCodexFunctionCallIdentity(params, call, gjson.Result{}, item)
		updateCodexFunctionCallArguments(call, item.Get("arguments").String(), false)
		call.Done = true
		return true
	})

	queuedCalls := params.FunctionCallQueue[:0]
	for _, call := range params.FunctionCallQueue {
		if call.Closed {
			continue
		}
		if call.Name == "" {
			call.Closed = true
			continue
		}
		if !call.Done && !forceCloseOpenCalls && strings.TrimSpace(call.Arguments) == "" {
			// Leave argument-less open blocks unclosed outside the
			// grok-composer repair scope: never invent a stop for an
			// incomplete block. The stream is over, so the dropped queue
			// entry is released by clearCodexFunctionCalls below.
			continue
		}
		call.Done = true
		queuedCalls = append(queuedCalls, call)
	}
	params.FunctionCallQueue = queuedCalls
	output = appendCodexFunctionCallQueue(output, params, originalRequestRawJSON)

	clearCodexFunctionCalls(params)
	return output
}

func clearCodexFunctionCalls(params *ConvertCodexResponseToClaudeParams) {
	if params == nil {
		return
	}
	clear(params.FunctionCalls)
	params.FunctionCallQueue = nil
	params.ActiveFunctionCall = nil
	params.LastFunctionCall = nil
}

func resolveCodexClaudeToolUseName(originalRequestRawJSON []byte, name string) string {
	rev := buildReverseMapFromClaudeOriginalShortToOriginal(originalRequestRawJSON)
	if orig, ok := rev[name]; ok {
		return orig
	}
	return name
}

func extractResponsesUsage(usage gjson.Result) (int64, int64, int64, int64) {
	if !usage.Exists() || usage.Type == gjson.Null {
		return 0, 0, 0, 0
	}

	inputTokens := usage.Get("input_tokens").Int()
	outputTokens := usage.Get("output_tokens").Int()
	cachedTokens := usage.Get("input_tokens_details.cached_tokens").Int()
	cacheWriteTokens := usage.Get("input_tokens_details.cache_write_tokens").Int()
	if cacheWriteTokens == 0 {
		cacheWriteTokens = usage.Get("input_tokens_details.cache_creation_tokens").Int()
	}

	if cachedTokens > 0 {
		if inputTokens >= cachedTokens {
			inputTokens -= cachedTokens
		} else {
			inputTokens = 0
		}
	}

	return inputTokens, outputTokens, cachedTokens, cacheWriteTokens
}

func setClaudeReasoningUsage(out []byte, usage gjson.Result) []byte {
	detail := usage.Get("output_tokens_details.reasoning_tokens")
	if !detail.Exists() || detail.Type != gjson.Number {
		return out
	}
	if strings.HasPrefix(detail.Raw, "-") || detail.Num < 0 {
		return out
	}
	outputTokens := max(int64(0), usage.Get("output_tokens").Int())
	var tokens int64
	if detail.Num >= float64(outputTokens) {
		tokens = outputTokens
	} else {
		tokens = detail.Int()
	}
	updated, errSetBytes := sjson.SetBytes(out, "usage.output_tokens_details.thinking_tokens", tokens)
	if errSetBytes != nil {
		return out
	}
	return updated
}

// buildReverseMapFromClaudeOriginalShortToOriginal builds a map[short]original from original Claude request tools.
func buildReverseMapFromClaudeOriginalShortToOriginal(original []byte) map[string]string {
	tools := gjson.GetBytes(original, "tools")
	rev := map[string]string{}
	if !tools.IsArray() {
		return rev
	}
	var names []string
	arr := tools.Array()
	for i := 0; i < len(arr); i++ {
		n := arr[i].Get("name").String()
		if n != "" {
			names = append(names, n)
		}
	}
	if len(names) > 0 {
		m := buildShortNameMap(names)
		for orig, short := range m {
			rev[short] = orig
		}
	}
	return rev
}

func ClaudeTokenCount(_ context.Context, count int64) []byte {
	return translatorcommon.ClaudeInputTokensJSON(count)
}

func startCodexTextBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if params.TextBlockOpen {
		return nil
	}

	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	template, _ = sjson.SetBytes(template, "index", params.BlockIndex)
	params.TextBlockOpen = true

	return translatorcommon.AppendSSEEventBytes(nil, "content_block_start", template, 2)
}

func stopCodexTextBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if !params.TextBlockOpen {
		return nil
	}

	template := []byte(`{"type":"content_block_stop","index":0}`)
	template, _ = sjson.SetBytes(template, "index", params.BlockIndex)
	params.TextBlockOpen = false
	params.BlockIndex++

	return translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", template, 2)
}

func startCodexThinkingBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if params.ThinkingBlockOpen {
		return nil
	}

	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
	template, _ = sjson.SetBytes(template, "index", params.BlockIndex)
	params.ThinkingBlockOpen = true
	params.ThinkingStopPending = false

	return translatorcommon.AppendSSEEventBytes(nil, "content_block_start", template, 2)
}

// appendCodexThinkingDelta emits a thinking_delta for the currently open thinking block.
func appendCodexThinkingDelta(params *ConvertCodexResponseToClaudeParams, text string) []byte {
	if text == "" {
		return nil
	}

	template := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":""}}`)
	template, _ = sjson.SetBytes(template, "index", params.BlockIndex)
	template, _ = sjson.SetBytes(template, "delta.thinking", text)

	return translatorcommon.AppendSSEEventBytes(nil, "content_block_delta", template, 2)
}

func finalizeCodexSignatureOnlyThinkingBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if params.ThinkingSignature == "" {
		return nil
	}

	output := startCodexThinkingBlock(params)
	output = append(output, finalizeCodexThinkingBlock(params)...)
	return output
}

// ensureCodexTextBlockOpen opens a text content block when one is not already open, closing any
// open thinking block first so the two cannot share a content index. It is idempotent: when a text
// block is already open it emits nothing, which keeps repeated response.content_part.added events
// (grok-composer) from producing duplicate content_block_start events at the same index.
func ensureCodexTextBlockOpen(params *ConvertCodexResponseToClaudeParams) []byte {
	if params.TextBlockOpen {
		return nil
	}

	output := finalizeCodexThinkingBlock(params)
	template := []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	template, _ = sjson.SetBytes(template, "index", params.BlockIndex)
	params.TextBlockOpen = true

	return translatorcommon.AppendSSEEventBytes(output, "content_block_start", template, 2)
}

// finalizeCodexTextBlock closes an open text content block and advances the block index.
// response.content_part.added opens a text block at the current index without incrementing
// it (only response.content_part.done does). When an upstream stream skips content_part.done
// (grok-composer), the next block would otherwise reuse the open text index; closing it here
// keeps content block indexes monotonic and the text block properly stopped.
func finalizeCodexTextBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if !params.TextBlockOpen {
		return nil
	}

	contentBlockStop := []byte(`{"type":"content_block_stop","index":0}`)
	contentBlockStop, _ = sjson.SetBytes(contentBlockStop, "index", params.BlockIndex)
	params.TextBlockOpen = false
	params.BlockIndex++

	return translatorcommon.AppendSSEEventBytes(nil, "content_block_stop", contentBlockStop, 2)
}

func finalizeCodexThinkingBlock(params *ConvertCodexResponseToClaudeParams) []byte {
	if !params.ThinkingBlockOpen {
		return nil
	}

	output := make([]byte, 0, 256)
	if params.ThinkingSignature != "" {
		signatureDelta := []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":""}}`)
		signatureDelta, _ = sjson.SetBytes(signatureDelta, "index", params.BlockIndex)
		signatureDelta, _ = sjson.SetBytes(signatureDelta, "delta.signature", params.ThinkingSignature)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_delta", signatureDelta, 2)
	}

	contentBlockStop := []byte(`{"type":"content_block_stop","index":0}`)
	contentBlockStop, _ = sjson.SetBytes(contentBlockStop, "index", params.BlockIndex)
	output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", contentBlockStop, 2)

	params.BlockIndex++
	params.ThinkingBlockOpen = false
	params.ThinkingStopPending = false

	return output
}

// Branch-preserved stream-repair helpers for non-conforming Codex streams
// (e.g. grok-composer) and cached orphan tool-output repair. They are kept
// alongside the upstream queue-based machinery; the live conversion path
// above does not call them.
func (params *ConvertCodexResponseToClaudeParams) startCodexToolBlock(rootResult, itemResult gjson.Result) int {
	params.ensureCodexToolBlockState()
	blockIndex := params.BlockIndex
	params.BlockIndex++
	params.CurrentToolBlockIndex = blockIndex
	params.HasCurrentToolBlock = true
	params.ToolBlockOpen[blockIndex] = true
	params.ToolBlockOrder = append(params.ToolBlockOrder, blockIndex)
	for _, key := range codexToolStreamKeys(rootResult, itemResult) {
		params.ToolBlockIndexes[key] = blockIndex
	}
	return blockIndex
}
func (params *ConvertCodexResponseToClaudeParams) codexToolBlockIndex(rootResult, itemResult gjson.Result) int {
	params.ensureCodexToolBlockState()
	for _, key := range codexToolStreamKeys(rootResult, itemResult) {
		if blockIndex, ok := params.ToolBlockIndexes[key]; ok {
			return blockIndex
		}
	}
	if params.HasCurrentToolBlock {
		return params.CurrentToolBlockIndex
	}
	return params.BlockIndex
}
func (params *ConvertCodexResponseToClaudeParams) codexOpenToolBlockIndex(rootResult, itemResult gjson.Result) (int, bool) {
	params.ensureCodexToolBlockState()
	keys := codexToolStreamKeys(rootResult, itemResult)
	for _, key := range keys {
		if blockIndex, ok := params.ToolBlockIndexes[key]; ok {
			return blockIndex, params.ToolBlockOpen[blockIndex]
		}
	}
	if params.HasCurrentToolBlock && params.ToolBlockOpen[params.CurrentToolBlockIndex] {
		return params.CurrentToolBlockIndex, true
	}
	if len(keys) == 0 {
		openIndex := -1
		openCount := 0
		for blockIndex, open := range params.ToolBlockOpen {
			if open {
				openIndex = blockIndex
				openCount++
			}
		}
		if openCount == 1 {
			return openIndex, true
		}
	}
	return 0, false
}
func (params *ConvertCodexResponseToClaudeParams) markCodexToolArgumentsDelta(blockIndex int) {
	params.ensureCodexToolBlockState()
	params.ToolArgumentDeltaSeen[blockIndex] = true
}
func (params *ConvertCodexResponseToClaudeParams) codexToolArgumentsDeltaSeen(blockIndex int) bool {
	params.ensureCodexToolBlockState()
	return params.ToolArgumentDeltaSeen[blockIndex]
}
func (params *ConvertCodexResponseToClaudeParams) finishCodexToolBlock(rootResult, itemResult gjson.Result, blockIndex int, keepFinishedMapping bool) {
	params.ensureCodexToolBlockState()
	for _, key := range codexToolStreamKeys(rootResult, itemResult) {
		if keepFinishedMapping {
			params.ToolBlockIndexes[key] = blockIndex
		} else {
			delete(params.ToolBlockIndexes, key)
		}
	}
	params.ToolBlockOpen[blockIndex] = false
	delete(params.ToolArgumentDeltaSeen, blockIndex)
	if params.HasCurrentToolBlock && params.CurrentToolBlockIndex == blockIndex {
		if keepFinishedMapping {
			params.refreshCurrentToolBlock()
		} else {
			params.HasCurrentToolBlock = false
		}
	}
	// Clear legacy single-open-tool tracking so response.completed does not emit a
	// second content_block_stop for a tool that was already closed on output_item.done.
	if params.FunctionCallBlockOpen && params.FunctionCallBlockIndex == blockIndex {
		params.FunctionCallBlockOpen = false
		params.FunctionCallBlockCallID = ""
		params.FunctionCallBlockIndex = 0
	}
}
func (params *ConvertCodexResponseToClaudeParams) ensureCodexToolBlockState() {
	if params.ToolBlockIndexes == nil {
		params.ToolBlockIndexes = make(map[string]int)
	}
	if params.ToolBlockOpen == nil {
		params.ToolBlockOpen = make(map[int]bool)
	}
	if params.ToolArgumentDeltaSeen == nil {
		params.ToolArgumentDeltaSeen = make(map[int]bool)
	}
}
func (params *ConvertCodexResponseToClaudeParams) refreshCurrentToolBlock() {
	params.HasCurrentToolBlock = false
	for i := len(params.ToolBlockOrder) - 1; i >= 0; i-- {
		blockIndex := params.ToolBlockOrder[i]
		if params.ToolBlockOpen[blockIndex] {
			params.CurrentToolBlockIndex = blockIndex
			params.HasCurrentToolBlock = true
			return
		}
	}
}
func finalizeOpenCodexToolBlocks(params *ConvertCodexResponseToClaudeParams) []byte {
	params.ensureCodexToolBlockState()
	output := make([]byte, 0, 128)
	for _, blockIndex := range params.ToolBlockOrder {
		if !params.ToolBlockOpen[blockIndex] {
			continue
		}
		template := []byte(`{"type":"content_block_stop","index":0}`)
		template, _ = sjson.SetBytes(template, "index", blockIndex)
		output = translatorcommon.AppendSSEEventBytes(output, "content_block_stop", template, 2)
		params.ToolBlockOpen[blockIndex] = false
		delete(params.ToolArgumentDeltaSeen, blockIndex)
		// Clear upstream FunctionCallBlockOpen if it points at a block we just closed,
		// so finalizeCodexOpenContentBlocks does not emit a second stop.
		if params.FunctionCallBlockOpen && params.FunctionCallBlockIndex == blockIndex {
			params.FunctionCallBlockOpen = false
			params.FunctionCallBlockCallID = ""
			params.FunctionCallBlockIndex = 0
		}
	}
	params.refreshCurrentToolBlock()
	return output
}
func codexFunctionArgumentsString(itemResult gjson.Result) string {
	argsResult := itemResult.Get("arguments")
	if !argsResult.Exists() {
		return ""
	}
	if argsResult.Type == gjson.String {
		return argsResult.String()
	}
	return argsResult.Raw
}
func isGrokComposerClaudeStreamRepairModel(modelName string) bool {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	return normalized == "grok-composer-2.5-fast" ||
		strings.HasPrefix(normalized, "grok-composer-2.5-fast-") ||
		strings.HasPrefix(normalized, "grok-composer-2.5-fast[")
}
func codexToolStreamKeys(rootResult, itemResult gjson.Result) []string {
	seen := make(map[string]struct{})
	keys := make([]string, 0, 4)
	addKey := func(prefix, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := prefix + ":" + value
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	addKey("item_id", rootResult.Get("item_id").String())
	addKey("item_id", itemResult.Get("id").String())
	addKey("call_id", itemResult.Get("call_id").String())
	if outputIndex := rootResult.Get("output_index"); outputIndex.Exists() {
		addKey("output_index", outputIndex.Raw)
	}
	return keys
}
func appendCodexOpenFunctionCallStop(output []byte, params *ConvertCodexResponseToClaudeParams) []byte {
	if params == nil || !params.FunctionCallBlockOpen {
		return output
	}

	blockIndex := params.FunctionCallBlockIndex
	output = appendCodexFunctionCallStop(output, blockIndex)
	if params.BlockIndex <= blockIndex {
		params.BlockIndex = blockIndex + 1
	}
	params.FunctionCallBlockOpen = false
	params.FunctionCallBlockCallID = ""
	params.FunctionCallBlockIndex = 0
	return output
}
func hydrateOpenCodexFunctionCallFromTerminal(output []byte, params *ConvertCodexResponseToClaudeParams, responseData gjson.Result) []byte {
	if params == nil || !params.FunctionCallBlockOpen || params.HasReceivedArgumentsDelta {
		return output
	}

	responseData.Get("output").ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() != "function_call" || codexFunctionCallID(item) != params.FunctionCallBlockCallID {
			return true
		}
		if args := item.Get("arguments").String(); args != "" {
			output = appendCodexFunctionCallArgumentDelta(output, args, params.FunctionCallBlockIndex)
			params.HasReceivedArgumentsDelta = true
		}
		return false
	})
	return output
}
func appendPendingCodexFunctionCallsFromTerminal(output []byte, params *ConvertCodexResponseToClaudeParams, originalRequestRawJSON []byte, responseData gjson.Result) []byte {
	if params == nil || len(params.PendingFunctionCalls) == 0 {
		return output
	}

	responseData.Get("output").ForEach(func(index, item gjson.Result) bool {
		if item.Get("type").String() != "function_call" {
			return true
		}

		pending, pendingKeys := pendingCodexFunctionCallForTerminalItem(params, index, item)
		if pending == nil {
			return true
		}
		if pending.StartEmitted {
			deletePendingCodexFunctionCallAliases(params, pendingKeys)
			return true
		}

		name := item.Get("name").String()
		if name == "" {
			deletePendingCodexFunctionCallAliases(params, pendingKeys)
			return true
		}
		callID := pending.CallID
		if callID == "" {
			callID = codexFunctionCallID(item)
		}

		blockIndex := params.BlockIndex
		output = appendCodexFunctionCallStart(output, originalRequestRawJSON, callID, name, blockIndex)
		params.HasEmittedToolUse = true
		pending.StartEmitted = true

		args := item.Get("arguments").String()
		if args == "" {
			args = pending.Arguments
		}
		if args != "" {
			output = appendCodexFunctionCallArgumentDelta(output, args, blockIndex)
		}
		output = appendCodexFunctionCallStop(output, blockIndex)
		params.BlockIndex++

		deletePendingCodexFunctionCallAliases(params, pendingKeys)
		return true
	})

	clearPendingCodexFunctionCalls(params)
	return output
}
func pendingCodexFunctionCallForTerminalItem(params *ConvertCodexResponseToClaudeParams, outputIndex, item gjson.Result) (*pendingCodexFunctionCall, []string) {
	if params == nil || params.PendingFunctionCalls == nil {
		return nil, nil
	}

	keys := make([]string, 0, 3)
	if callID := codexFunctionCallID(item); callID != "" {
		keys = appendUniqueCodexFunctionCallKey(keys, codexFunctionCallIDKey(callID))
	}
	if itemOutputIndex := item.Get("output_index"); itemOutputIndex.Exists() {
		keys = appendUniqueCodexFunctionCallKey(keys, "output:"+itemOutputIndex.Raw)
	}
	if outputIndex.Exists() {
		keys = appendUniqueCodexFunctionCallKey(keys, "output:"+outputIndex.Raw)
	}

	for _, key := range keys {
		if pending, ok := params.PendingFunctionCalls[key]; ok {
			return pending, keysForPendingCodexFunctionCall(params, pending)
		}
	}
	return nil, nil
}
func clearPendingCodexFunctionCalls(params *ConvertCodexResponseToClaudeParams) {
	if params == nil || params.PendingFunctionCalls == nil {
		return
	}
	for key := range params.PendingFunctionCalls {
		delete(params.PendingFunctionCalls, key)
	}
	params.LastPendingFunctionCallKey = ""
}
func finalizeCodexOpenContentBlocks(params *ConvertCodexResponseToClaudeParams) []byte {
	output := make([]byte, 0, 256)
	output = append(output, finalizeCodexThinkingBlock(params)...)
	output = append(output, stopCodexTextBlock(params)...)
	if params != nil && params.FunctionCallBlockOpen && params.HasReceivedArgumentsDelta {
		output = appendCodexOpenFunctionCallStop(output, params)
	}
	return output
}
