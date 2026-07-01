package responses

import (
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIResponsesRequestToOpenAIChatCompletions converts OpenAI responses format to OpenAI chat completions format.
// It transforms the OpenAI responses API format (with instructions and input array) into the standard
// OpenAI chat completions format (with messages array and system content).
//
// The conversion handles:
// 1. Model name and streaming configuration
// 2. Instructions to system message conversion
// 3. Input array to messages array transformation
// 4. Tool definitions and tool choice conversion
// 5. Function calls and function results handling
// 6. Generation parameters mapping (max_tokens, reasoning, etc.)
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data in OpenAI responses format
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI chat completions format
func ConvertOpenAIResponsesRequestToOpenAIChatCompletions(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Base OpenAI chat completions template with default values
	out := []byte(`{"model":"","messages":[],"stream":false}`)

	root := gjson.ParseBytes(rawJSON)

	// Set model name
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Set stream configuration
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Map generation parameters from responses format to chat completions format
	if maxTokens := root.Get("max_output_tokens"); maxTokens.Exists() {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	// H20: carry parallel_tool_calls through verbatim so the executor can enforce it
	// where structurally possible (tool-advertisement gating). NOTE: the composer path
	// CANNOT hard-cap how many tool calls Cursor emits in a single turn (Cursor generates
	// the tokens; the bridge only relays), so parallel_tool_calls:false is a best-effort /
	// explicit-unsupported signal downstream — not a server-side hard guarantee. We must
	// still carry it (dropping it would silently lose the client's intent).
	if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Exists() {
		out, _ = sjson.SetBytes(out, "parallel_tool_calls", parallelToolCalls.Bool())
	}

	// H16 / C-RESPID: surface the conversation-stable Responses identifiers onto the
	// translated body so they remain readable for the executor's response-id->sessionID
	// map (and any consumer that reads the translated OpenAI body rather than the original
	// Responses body). The executor primarily reads these directly from opts.OriginalRequest,
	// so this is additive and must never strip them. previous_response_id is the id the
	// client echoes from a prior response; conversation_id (or the conversation.id object
	// form) is stable across a conversation's turns.
	//
	// ADD-65 / C-ADD65-RESPID-CONT: previous_response_id MUST survive even on a continuation
	// turn whose input is [function_call_output, message(user)]. It is one of the two signals
	// (the other is tool_call_id ownership) the executor's composerToolResults branch (c) uses
	// to classify that shape as a continuation rather than a fresh user turn. Surfacing it here
	// unconditionally (independent of whether tools/tool-results are present) keeps that
	// classification path fed.
	if prevID := root.Get("previous_response_id"); prevID.Exists() && prevID.Type == gjson.String {
		if v := strings.TrimSpace(prevID.String()); v != "" {
			out, _ = sjson.SetBytes(out, "previous_response_id", v)
		}
	}
	if convID := responsesConversationID(root); convID != "" {
		out, _ = sjson.SetBytes(out, "conversation_id", convID)
	}

	// Convert instructions to system message
	if instructions := root.Get("instructions"); instructions.Exists() {
		systemMessage := []byte(`{"role":"system","content":""}`)
		systemMessage, _ = sjson.SetBytes(systemMessage, "content", instructions.String())
		out, _ = sjson.SetRawBytes(out, "messages.-1", systemMessage)
	}

	// Convert input array to messages
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		inputItems := input.Array()
		outputCallIDs := make(map[string]struct{})
		for _, item := range inputItems {
			if item.Get("type").String() != "function_call_output" {
				continue
			}
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID == "" {
				continue
			}
			outputCallIDs[callID] = struct{}{}
		}

		pendingToolCalls := make([]interface{}, 0)
		pendingToolCallIDs := make([]string, 0)
		pendingReasoningContent := ""
		awaitingToolOutputs := make(map[string]struct{})
		deferredMessages := make([][]byte, 0)

		takePendingReasoningContent := func() string {
			reasoningContent := pendingReasoningContent
			pendingReasoningContent = ""
			return reasoningContent
		}
		flushPendingToolCalls := func() {
			if len(pendingToolCalls) == 0 {
				return
			}
			assistantMessage := []byte(`{"role":"assistant","tool_calls":[]}`)
			assistantMessage, _ = sjson.SetBytes(assistantMessage, "tool_calls", pendingToolCalls)
			if reasoningContent := takePendingReasoningContent(); reasoningContent != "" {
				assistantMessage, _ = sjson.SetBytes(assistantMessage, "reasoning_content", reasoningContent)
			}
			out, _ = sjson.SetRawBytes(out, "messages.-1", assistantMessage)
			for _, id := range pendingToolCallIDs {
				if strings.TrimSpace(id) == "" {
					continue
				}
				awaitingToolOutputs[id] = struct{}{}
			}
			pendingToolCalls = pendingToolCalls[:0]
			pendingToolCallIDs = pendingToolCallIDs[:0]
		}
		flushDeferredMessages := func() {
			for _, message := range deferredMessages {
				out, _ = sjson.SetRawBytes(out, "messages.-1", message)
			}
			deferredMessages = deferredMessages[:0]
		}
		hasAwaitingToolOutput := func() bool {
			for id := range awaitingToolOutputs {
				if _, ok := outputCallIDs[id]; ok {
					return true
				}
			}
			return false
		}
		appendRegularMessage := func(message []byte) {
			// Keep tool-call adjacency strict for providers that require
			// assistant(tool_calls) -> tool(tool_call_id) with no message in between.
			if hasAwaitingToolOutput() {
				deferredMessages = append(deferredMessages, message)
				return
			}
			out, _ = sjson.SetRawBytes(out, "messages.-1", message)
		}
		appendPendingReasoningMessage := func() {
			reasoningContent := takePendingReasoningContent()
			if reasoningContent == "" {
				return
			}
			message := []byte(`{"role":"assistant","content":"","reasoning_content":""}`)
			message, _ = sjson.SetBytes(message, "reasoning_content", reasoningContent)
			appendRegularMessage(message)
		}

		for _, item := range inputItems {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}
			if itemType != "function_call" {
				flushPendingToolCalls()
			}

			switch itemType {
			case "message", "":
				// Handle regular message conversion.
				// H19: do NOT downgrade a Responses `developer` message to `user`. The
				// developer role carries elevated (system-adjacent) instructions; collapsing
				// it to a normal user/history message silently strips that priority. Preserve
				// the role verbatim — downstream composer extraction treats `developer` the
				// same as `system` (folds it into the composer system prompt), and the OpenAI
				// Chat schema accepts a `developer` role.
				role := item.Get("role").String()
				if role != "assistant" {
					appendPendingReasoningMessage()
				}
				message := []byte(`{"role":"","content":[]}`)
				message, _ = sjson.SetBytes(message, "role", role)

				if content := item.Get("content"); content.Exists() && content.IsArray() {
					var messageContent string
					var toolCalls []interface{}

					content.ForEach(func(_, contentItem gjson.Result) bool {
						contentType := contentItem.Get("type").String()
						if contentType == "" {
							contentType = "input_text"
						}

						switch contentType {
						case "input_text", "output_text":
							text := contentItem.Get("text").String()
							contentPart := []byte(`{"type":"text","text":""}`)
							contentPart, _ = sjson.SetBytes(contentPart, "text", text)
							message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
						case "input_image":
							// ADD-55: a Responses `input_image` is not always a scalar `image_url`
							// string. Clients (and Chat-Completions-shaped relays) send the canonical
							// object form `image_url:{url:...}`, and OpenAI also accepts a `file_id`
							// reference to a previously-uploaded file. The previous code did
							// `contentItem.Get("image_url").String()`, which returns "" for the object
							// form (gjson stringifies an object to its raw JSON, not the URL) and for
							// `file_id`, then emitted an `image_url` part with an EMPTY url. The composer
							// image extractor (extractComposerImages) silently skips empty/unsupported
							// urls, so the user's image was lost with no error (false success on an
							// image-only turn). Resolve every shape and NEVER emit an empty image part:
							//   - scalar `image_url` string                -> use verbatim
							//   - object `image_url:{url:...}`              -> use the nested url
							//   - top-level `url` (some relays)             -> use it as a fallback
							//   - `file_id` (no resolvable url)             -> this gateway cannot fetch
							//     uploaded files into the composer path, so emit a model-VISIBLE text
							//     marker instead of an empty image part (pairs with ADD-56's executor
							//     placeholder), surfacing the unsupported attachment rather than dropping
							//     it silently.
							imageURL := responsesInputImageURL(contentItem)
							if imageURL != "" {
								contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
								contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", imageURL)
								if detail := contentItem.Get("detail"); detail.Exists() {
									contentPart, _ = sjson.SetBytes(contentPart, "image_url.detail", detail.String())
								}
								message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
								break
							}
							// No usable URL. Surface the unsupported attachment as visible text so the
							// model is not handed an empty turn (never emit an empty image_url part).
							marker := responsesUnsupportedImageMarker(contentItem)
							contentPart := []byte(`{"type":"text","text":""}`)
							contentPart, _ = sjson.SetBytes(contentPart, "text", marker)
							message, _ = sjson.SetRawBytes(message, "content.-1", contentPart)
						}
						return true
					})

					if messageContent != "" {
						message, _ = sjson.SetBytes(message, "content", messageContent)
					}

					if len(toolCalls) > 0 {
						message, _ = sjson.SetBytes(message, "tool_calls", toolCalls)
					}
				} else if content.Type == gjson.String {
					message, _ = sjson.SetBytes(message, "content", content.String())
				}

				if role == "assistant" {
					reasoningContent := item.Get("reasoning_content").String()
					if reasoningContent == "" {
						reasoningContent = takePendingReasoningContent()
					} else {
						pendingReasoningContent = ""
					}
					if reasoningContent != "" {
						message, _ = sjson.SetBytes(message, "reasoning_content", reasoningContent)
					}
				}

				appendRegularMessage(message)

			case "reasoning":
				reasoningContent := collectOpenAIResponsesReasoningContent(item)
				if pendingReasoningContent == "" {
					pendingReasoningContent = reasoningContent
				} else {
					pendingReasoningContent += reasoningContent
				}

			case "function_call":
				// Buffer consecutive function calls and emit them as one assistant message.
				toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)

				if callId := item.Get("call_id"); callId.Exists() {
					toolCall, _ = sjson.SetBytes(toolCall, "id", callId.String())
				}

				if name := item.Get("name"); name.Exists() {
					toolCall, _ = sjson.SetBytes(toolCall, "function.name", name.String())
				}

				if arguments := item.Get("arguments"); arguments.Exists() {
					toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", arguments.String())
				}
				pendingToolCalls = append(pendingToolCalls, gjson.ParseBytes(toolCall).Value())
				if callID := strings.TrimSpace(item.Get("call_id").String()); callID != "" {
					pendingToolCallIDs = append(pendingToolCallIDs, callID)
				}

			case "function_call_output":
				// Handle function call output conversion to tool message.
				//
				// ADD-65 / C-ADD65-RESPID-CONT (request-side contract): a Responses/Codex
				// client using stateful continuation sends ONLY the current
				// function_call_output plus new user text, NOT the prior assistant
				// function_call (the assistant call is chained server-side via
				// previous_response_id). That input array is
				//   [{type:function_call_output,...}, {type:message,role:user,...}]
				// which this translator must (and does) normalize to
				//   [..., {role:"tool",tool_call_id:<call_id>,...}, {role:"user",...}].
				// There is intentionally NO synthetic assistant tool_calls message here
				// (we must not fabricate the prior call). The executor's composerToolResults
				// branch (c) classifies this [role:tool, role:user] shape as a CONTINUATION
				// by tool_call_id ownership OR a present previous_response_id (surfaced onto
				// the translated body above). The request side's only obligation is to
				// preserve all three signals — the role:tool message with its real call_id,
				// the trailing user text, and previous_response_id — which it does. Do NOT
				// drop the trailing user message or stringify previous_response_id away, or
				// the turn falls back to a fresh user turn and strands the tool output.
				toolMessage := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
				callID := ""

				if callId := item.Get("call_id"); callId.Exists() {
					callID = strings.TrimSpace(callId.String())
					toolMessage, _ = sjson.SetBytes(toolMessage, "tool_call_id", callID)
				}

				if output := item.Get("output"); output.Exists() {
					// L34: Responses function_call_output.output may be structured
					// (an array of content parts, or file/image objects) rather than a
					// plain string. Do NOT flatten lossily via .String() — preserve the
					// structured shape so downstream consumers that support array content
					// (e.g. the composer text extractor reads `text` parts; future
					// renderers can pick up image/file parts) keep the data. A bare string
					// stays a string (current, lossless behavior); an array/object is
					// carried as raw JSON. This is a usable degrade, not a silent drop.
					if output.IsArray() || output.IsObject() {
						toolMessage, _ = sjson.SetRawBytes(toolMessage, "content", []byte(output.Raw))
					} else {
						toolMessage, _ = sjson.SetBytes(toolMessage, "content", output.String())
					}
				}

				out, _ = sjson.SetRawBytes(out, "messages.-1", toolMessage)
				if callID != "" {
					delete(awaitingToolOutputs, callID)
				}
				if len(awaitingToolOutputs) == 0 && len(deferredMessages) > 0 {
					flushDeferredMessages()
				}
			}

		}
		flushPendingToolCalls()
		appendPendingReasoningMessage()
		flushDeferredMessages()
	} else if input.Type == gjson.String {
		msg := []byte(`{}`)
		msg, _ = sjson.SetBytes(msg, "role", "user")
		msg, _ = sjson.SetBytes(msg, "content", input.String())
		out, _ = sjson.SetRawBytes(out, "messages.-1", msg)
	}

	// Convert tools from responses format to chat completions format
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		var chatCompletionsTools []interface{}

		tools.ForEach(func(_, tool gjson.Result) bool {
			for _, chatTool := range convertResponsesToolToOpenAIChatTools(tool) {
				chatCompletionsTools = append(chatCompletionsTools, gjson.ParseBytes(chatTool).Value())
			}
			return true
		})

		if len(chatCompletionsTools) > 0 {
			out, _ = sjson.SetBytes(out, "tools", chatCompletionsTools)
		}
	}

	if reasoningEffort := root.Get("reasoning.effort"); reasoningEffort.Exists() {
		effort := strings.ToLower(strings.TrimSpace(reasoningEffort.String()))
		if effort != "" {
			out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
		}
	}

	// ADD-92 / Comment 3: translate a Responses structured-output request (`text.format`)
	// into the Chat-Completions `response_format` shape the executor's
	// extractComposerResponseFormat consumes. The OpenAI Responses API expresses Structured
	// Outputs via text.format (NOT response_format), so without this the schema/json request
	// was silently dropped while the Responses response translator still echoed `text` back —
	// making the proxy appear to honor a contract it never forwarded.
	//
	// extractComposerResponseFormat requires an OBJECT and, for json_schema, reads
	// `response_format.type == "json_schema"` plus `response_format.json_schema`. We therefore
	// build exactly that shape:
	//   - {type:"json_object"}                       -> {type:"json_object"}
	//   - {type:"json_schema", name, schema, strict} -> {type:"json_schema",
	//       json_schema:{name?, schema, strict?}}     (Responses nests the schema directly
	//       under text.format with name+schema+strict)
	//   - {type:"json_schema", json_schema:{...}}    -> carried through (already the nested
	//       Chat-Completions form some relays emit)
	// The schema body is copied with SetRawBytes so a large/strict JSON Schema is preserved
	// verbatim (no lossy re-encode). Per the standing decision this is surfaced as a
	// best-effort constraint downstream (composerConstraints flags strict json_schema in
	// unsupportedHardGuarantees); the translator's only job is to forward it, never to claim
	// hard enforcement.
	if rf := responsesResponseFormatFromTextFormat(root.Get("text.format")); rf != nil {
		out, _ = sjson.SetRawBytes(out, "response_format", rf)
	}

	// ADD-94 / Comment 4: preserve `store` so the executor can act on it. A Responses client
	// uses store:false to demand a one-shot, non-durable turn. The composer path persists/
	// resumes durable Cursor agent state by a stable session id, so silently dropping store
	// here (and echoing store:false back from the response translator) is a privacy/compliance
	// foot-gun. Surface it as a synthetic field: per the standing decision the executor rejects
	// store:false with a typed 4xx ("Cursor Composer requires durable state") rather than
	// faking an ephemeral mode — the translator's sole responsibility is not to drop it. We
	// only emit the field when it is explicitly false (store:true is the default and needs no
	// signal).
	if store := root.Get("store"); store.Exists() && store.Type == gjson.False {
		out, _ = sjson.SetBytes(out, "store", false)
	}

	// Convert tool_choice if present.
	// H07 / C-TOOLCHOICE: the executor's extractComposerToolChoice reads a STRING tool_choice
	// ("auto"/"none"/"required") or an OBJECT function shape `tool_choice.function.name` ->
	// "specific:<name>". Previously this stringified an OBJECT tool_choice (.String()), so the
	// executor saw `{"type":...}` as a useless string and the forced/allowed restriction was
	// lost. Preserve the shapes the executor understands:
	//   - STRING            -> pass through as a string (auto/none/required).
	//   - {type:function, function:{name:X}} -> emit the object verbatim (Chat Completions shape).
	//   - {type:function, name:X}            -> move the top-level name under function.name
	//                                            (Responses variant) so it matches the reader.
	//   - {type:allowed_tools, mode, tools:[{type:function, name:N}, ...]} -> Chat Completions has
	//     no native allowed_tools; carry it as a first-class `allowed_tools` raw object for the
	//     executor to intersect with the advertised set (best-effort gating), and keep tool_choice
	//     itself as "auto" (it is not a single forced tool). Never silently widen to all tools.
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
		if toolChoice.Type == gjson.String {
			out, _ = sjson.SetBytes(out, "tool_choice", toolChoice.String())
		} else if toolChoice.IsObject() {
			switch toolChoice.Get("type").String() {
			case "allowed_tools":
				// Carry the allowed_tools object verbatim for the executor (C-TOOLCHOICE).
				out, _ = sjson.SetRawBytes(out, "allowed_tools", []byte(toolChoice.Raw))
				// allowed_tools is a restriction set, not a single forced tool: keep auto.
				out, _ = sjson.SetBytes(out, "tool_choice", "auto")
			case "function":
				// Normalize to the Chat Completions object shape the executor reads:
				// {"type":"function","function":{"name":"<name>"}}.
				name := toolChoice.Get("function.name").String()
				if name == "" {
					// Responses variant carries the forced name at the top level.
					name = toolChoice.Get("name").String()
				}
				if name != "" {
					choice := []byte(`{"type":"function","function":{"name":""}}`)
					choice, _ = sjson.SetBytes(choice, "function.name", name)
					out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
				} else {
					// No resolvable name: pass the object through verbatim rather than a
					// lossy string, so the executor sees structure (not a fake forced tool).
					out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(toolChoice.Raw))
				}
			default:
				// Unknown object form: carry verbatim (do not stringify into a useless blob).
				out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(toolChoice.Raw))
			}
		}
	}

	return out
}

// responsesInputImageURL resolves the image URL from a Responses `input_image` content
// part across the shapes real clients send (ADD-55). It returns "" when no usable URL is
// present (e.g. a bare `file_id` reference, or an object with no `url`), so the caller can
// degrade to a visible marker instead of emitting an empty `image_url` part.
//
// Accepted shapes (in priority order):
//   - scalar string:  {"type":"input_image","image_url":"data:..."|"https://..."}
//   - object form:    {"type":"input_image","image_url":{"url":"..."}}
//   - top-level url:   {"type":"input_image","url":"..."}
func responsesInputImageURL(contentItem gjson.Result) string {
	img := contentItem.Get("image_url")
	if img.Type == gjson.String {
		if s := strings.TrimSpace(img.String()); s != "" {
			return s
		}
	}
	if img.IsObject() {
		if s := strings.TrimSpace(img.Get("url").String()); s != "" {
			return s
		}
	}
	if s := strings.TrimSpace(contentItem.Get("url").String()); s != "" {
		return s
	}
	return ""
}

// responsesUnsupportedImageMarker builds a short, model-visible text marker for an
// `input_image` part that carries no resolvable URL (ADD-55). The composer path cannot fetch
// an uploaded `file_id` into an image part, so rather than dropping the attachment silently
// (which would strand an image-only turn as an empty turn — false success) we surface it as
// text. The marker is bounded and contains no secrets (a file_id is an opaque reference id).
func responsesUnsupportedImageMarker(contentItem gjson.Result) string {
	if fid := strings.TrimSpace(contentItem.Get("file_id").String()); fid != "" {
		// Bound the id length defensively; file ids are short opaque tokens.
		if len(fid) > 128 {
			fid = fid[:128]
		}
		return "[image attachment unsupported: file_id reference cannot be resolved by this gateway (file_id=" + fid + ")]"
	}
	return "[image attachment unsupported: image_url is missing or in an unsupported form]"
}

// responsesResponseFormatFromTextFormat normalizes a Responses `text.format` value into the
// Chat-Completions `response_format` raw JSON the executor's extractComposerResponseFormat
// reads (ADD-92 / Comment 3). It returns nil when there is no usable structured-output request
// so the caller leaves `response_format` unset.
//
// Accepted shapes:
//   - {"type":"json_object"}                                   -> {"type":"json_object"}
//   - {"type":"json_schema","name":N,"schema":S,"strict":B}    -> {"type":"json_schema",
//     "json_schema":{"name":N,"schema":S,"strict":B}}        (Responses-native: schema nested
//     directly under text.format)
//   - {"type":"json_schema","json_schema":{...}}               -> {"type":"json_schema",
//     "json_schema":{...}}                                   (already the nested form)
//
// The schema body is copied with SetRawBytes so the JSON Schema is preserved verbatim; name and
// strict are carried only when present.
func responsesResponseFormatFromTextFormat(format gjson.Result) []byte {
	if !format.Exists() || !format.IsObject() {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(format.Get("type").String())) {
	case "json_object":
		return []byte(`{"type":"json_object"}`)
	case "json_schema":
		// Resolve the schema source: prefer the Responses-native nested fields under
		// text.format directly, then fall back to an embedded json_schema object.
		nested := format.Get("json_schema")
		schema := format.Get("schema")
		if !schema.Exists() && nested.IsObject() {
			schema = nested.Get("schema")
		}
		name := format.Get("name")
		if !name.Exists() && nested.IsObject() {
			name = nested.Get("name")
		}
		strict := format.Get("strict")
		if !strict.Exists() && nested.IsObject() {
			strict = nested.Get("strict")
		}
		jsonSchema := []byte(`{}`)
		if name.Exists() {
			jsonSchema, _ = sjson.SetBytes(jsonSchema, "name", name.String())
		}
		if schema.Exists() {
			jsonSchema, _ = sjson.SetRawBytes(jsonSchema, "schema", []byte(schema.Raw))
		}
		if strict.Exists() {
			jsonSchema, _ = sjson.SetBytes(jsonSchema, "strict", strict.Bool())
		}
		rf := []byte(`{"type":"json_schema"}`)
		rf, _ = sjson.SetRawBytes(rf, "json_schema", jsonSchema)
		return rf
	default:
		return nil
	}
}

// responsesConversationID extracts a conversation-stable identifier from a Responses
// request body. The OpenAI Responses API exposes the conversation either as a top-level
// `conversation_id` string or as a `conversation` value (a string id, or an object with an
// `id` field). Returns "" when no conversation id is present. Used by H16 / C-RESPID to
// surface the id onto the translated body.
func responsesConversationID(root gjson.Result) string {
	if v := root.Get("conversation_id"); v.Exists() && v.Type == gjson.String {
		if s := strings.TrimSpace(v.String()); s != "" {
			return s
		}
	}
	conv := root.Get("conversation")
	if !conv.Exists() {
		return ""
	}
	if conv.Type == gjson.String {
		return strings.TrimSpace(conv.String())
	}
	if conv.IsObject() {
		return strings.TrimSpace(conv.Get("id").String())
	}
	return ""
}

func collectOpenAIResponsesReasoningContent(item gjson.Result) string {
	var reasoningText strings.Builder
	if summary := item.Get("summary"); summary.Exists() && summary.IsArray() {
		summary.ForEach(func(_, summaryItem gjson.Result) bool {
			if summaryItem.Get("type").String() != "summary_text" {
				return true
			}
			reasoningText.WriteString(summaryItem.Get("text").String())
			return true
		})
	}
	if reasoningText.Len() == 0 {
		return "[reasoning unavailable]"
	}
	return reasoningText.String()
}
