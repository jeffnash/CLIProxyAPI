// Package gemini provides request translation functionality for Gemini to OpenAI API.
// It handles parsing and transforming Gemini API requests into OpenAI Chat Completions API format,
// extracting model information, generation config, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini API format and OpenAI API's expected format.
package gemini

import (
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToOpenAI parses and transforms a Gemini API request into OpenAI Chat Completions API format.
// It extracts the model name, generation config, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the OpenAI API.
func ConvertGeminiRequestToOpenAI(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	// Base OpenAI Chat Completions API template
	out := []byte(`{"model":"","messages":[]}`)

	root := gjson.ParseBytes(rawJSON)

	// Deterministic tool-call id minter from a function name + call ordinal.
	// Used only when the client truly sends NO id. Because a call and its matching
	// response derive the SAME id from the same (name, index) pair, the ids agree
	// and tool-use continuations round-trip even when ids are not preserved upstream.
	sanitizeToolName := func(name string) string {
		var b strings.Builder
		for _, r := range name {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		if b.Len() == 0 {
			return "fn"
		}
		return b.String()
	}
	mintDeterministicToolCallID := func(name string, index int) string {
		return fmt.Sprintf("call_%s_%d", sanitizeToolName(name), index)
	}

	// Model mapping
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Generation config mapping
	if genConfig := root.Get("generationConfig"); genConfig.Exists() {
		// Temperature
		if temp := genConfig.Get("temperature"); temp.Exists() {
			out, _ = sjson.SetBytes(out, "temperature", temp.Float())
		}

		// Max tokens
		if maxTokens := genConfig.Get("maxOutputTokens"); maxTokens.Exists() {
			out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
		}

		// Top P
		if topP := genConfig.Get("topP"); topP.Exists() {
			out, _ = sjson.SetBytes(out, "top_p", topP.Float())
		}

		// Top K (OpenAI doesn't have direct equivalent, but we can map it)
		if topK := genConfig.Get("topK"); topK.Exists() {
			// Store as custom parameter for potential use
			out, _ = sjson.SetBytes(out, "top_k", topK.Int())
		}

		// Stop sequences
		if stopSequences := genConfig.Get("stopSequences"); stopSequences.Exists() && stopSequences.IsArray() {
			var stops []string
			stopSequences.ForEach(func(_, value gjson.Result) bool {
				stops = append(stops, value.String())
				return true
			})
			if len(stops) > 0 {
				out, _ = sjson.SetBytes(out, "stop", stops)
			}
		}

		// Candidate count (OpenAI 'n' parameter)
		if candidateCount := genConfig.Get("candidateCount"); candidateCount.Exists() {
			out, _ = sjson.SetBytes(out, "n", candidateCount.Int())
		}

		// Map Gemini thinkingConfig to OpenAI reasoning_effort.
		// Always perform conversion to support allowCompat models that may not be in registry.
		// Note: Google official Python SDK sends snake_case fields (thinking_level/thinking_budget).
		if thinkingConfig := genConfig.Get("thinkingConfig"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
			thinkingLevel := thinkingConfig.Get("thinkingLevel")
			if !thinkingLevel.Exists() {
				thinkingLevel = thinkingConfig.Get("thinking_level")
			}
			if thinkingLevel.Exists() {
				effort := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				}
			} else {
				thinkingBudget := thinkingConfig.Get("thinkingBudget")
				if !thinkingBudget.Exists() {
					thinkingBudget = thinkingConfig.Get("thinking_budget")
				}
				if thinkingBudget.Exists() {
					if effort, ok := thinking.ConvertBudgetToLevel(int(thinkingBudget.Int())); ok {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				}
			}
		}
	}

	// Stream parameter
	out, _ = sjson.SetBytes(out, "stream", stream)

	// Process contents (Gemini messages) -> OpenAI messages.
	// Track the tool-call ids we DERIVE (or minted) per function name, in call order,
	// so a functionResponse with no id of its own can pair with its functionCall by
	// name+position. mintedByName[name] is a FIFO of ids waiting to be matched by a
	// response; mintedCountByName[name] counts how many calls we have seen for that
	// name so the deterministic id ordinal stays consistent across both branches.
	mintedByName := make(map[string][]string)
	mintedCountByName := make(map[string]int)

	// System instruction -> OpenAI system message
	// Gemini may provide `systemInstruction` or `system_instruction`; support both keys.
	systemInstruction := root.Get("systemInstruction")
	if !systemInstruction.Exists() {
		systemInstruction = root.Get("system_instruction")
	}
	if systemInstruction.Exists() {
		parts := systemInstruction.Get("parts")
		msg := []byte(`{"role":"system","content":[]}`)
		hasContent := false

		if parts.Exists() && parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				// Handle text parts
				if text := part.Get("text"); text.Exists() {
					contentPart := []byte(`{"type":"text","text":""}`)
					contentPart, _ = sjson.SetBytes(contentPart, "text", text.String())
					msg, _ = sjson.SetRawBytes(msg, "content.-1", contentPart)
					hasContent = true
				}

				// Handle inline data (e.g., images)
				if inlineData := part.Get("inlineData"); inlineData.Exists() {
					mimeType := inlineData.Get("mimeType").String()
					if mimeType == "" {
						mimeType = "application/octet-stream"
					}
					data := inlineData.Get("data").String()
					imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)

					contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
					contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", imageURL)
					msg, _ = sjson.SetRawBytes(msg, "content.-1", contentPart)
					hasContent = true
				}

				// ADD-54: handle fileData (URI-referenced media) in the system instruction
				// too. Mirrors the contents-loop branch: never silently drop the part.
				var ignoreOnlyText bool
				if cp, ok := geminiFileDataPart(part, &ignoreOnlyText); ok {
					msg, _ = sjson.SetRawBytes(msg, "content.-1", cp)
					hasContent = true
				}
				return true
			})
		}

		if hasContent {
			out, _ = sjson.SetRawBytes(out, "messages.-1", msg)
		}
	}

	if contents := root.Get("contents"); contents.Exists() && contents.IsArray() {
		contents.ForEach(func(_, content gjson.Result) bool {
			role := content.Get("role").String()
			parts := content.Get("parts")

			// Convert role: model -> assistant
			if role == "model" {
				role = "assistant"
			}

			msg := []byte(`{"role":"","content":""}`)
			msg, _ = sjson.SetBytes(msg, "role", role)

			var textBuilder strings.Builder
			contentWrapper := []byte(`{"arr":[]}`)
			contentPartsCount := 0
			onlyTextContent := true
			toolCallsWrapper := []byte(`{"arr":[]}`)
			toolCallsCount := 0

			if parts.Exists() && parts.IsArray() {
				parts.ForEach(func(_, part gjson.Result) bool {
					// Handle text parts
					if text := part.Get("text"); text.Exists() {
						formattedText := text.String()
						textBuilder.WriteString(formattedText)
						contentPart := []byte(`{"type":"text","text":""}`)
						contentPart, _ = sjson.SetBytes(contentPart, "text", formattedText)
						contentWrapper, _ = sjson.SetRawBytes(contentWrapper, "arr.-1", contentPart)
						contentPartsCount++
					}

					// Handle inline data (e.g., images)
					if inlineData := part.Get("inlineData"); inlineData.Exists() {
						onlyTextContent = false

						mimeType := inlineData.Get("mimeType").String()
						if mimeType == "" {
							mimeType = "application/octet-stream"
						}
						data := inlineData.Get("data").String()
						imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)

						contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
						contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", imageURL)
						contentWrapper, _ = sjson.SetRawBytes(contentWrapper, "arr.-1", contentPart)
						contentPartsCount++
					}

					// ADD-54: handle fileData (a media part referenced by URI instead of
					// inline base64). Without this branch the part was silently dropped, so
					// a Gemini turn that attaches an image/file by URI reached Composer with
					// the attachment missing. Image MIME types become an OpenAI image_url
					// (the URI is a fetchable reference); any other / missing MIME degrades
					// to a visible text marker so the attachment is NEVER silently dropped.
					if cp, ok := geminiFileDataPart(part, &onlyTextContent); ok {
						contentWrapper, _ = sjson.SetRawBytes(contentWrapper, "arr.-1", cp)
						contentPartsCount++
					}

					// Handle function calls (Gemini) -> tool calls (OpenAI)
					if functionCall := part.Get("functionCall"); functionCall.Exists() {
						fnName := functionCall.Get("name").String()

						// Prefer the client-provided id so the id round-trips: the
						// matching functionResponse can carry the SAME id and the
						// upstream bridge sees the exact id it emitted. Only mint a
						// DETERMINISTIC id (name + ordinal) when the client sent none,
						// minting it identically here and in the response branch so a
						// call and its response always agree.
						var toolCallID string
						if id := functionCall.Get("id").String(); id != "" {
							toolCallID = id
						} else {
							toolCallID = mintDeterministicToolCallID(fnName, mintedCountByName[fnName])
							mintedCountByName[fnName]++
						}
						// Remember it (FIFO per name) for a later id-less response to pair with.
						mintedByName[fnName] = append(mintedByName[fnName], toolCallID)

						toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
						toolCall, _ = sjson.SetBytes(toolCall, "id", toolCallID)
						toolCall, _ = sjson.SetBytes(toolCall, "function.name", fnName)

						// Convert args to arguments JSON string
						if args := functionCall.Get("args"); args.Exists() {
							toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", args.Raw)
						} else {
							toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", "{}")
						}

						toolCallsWrapper, _ = sjson.SetRawBytes(toolCallsWrapper, "arr.-1", toolCall)
						toolCallsCount++
					}

					// Handle function responses (Gemini) -> tool role messages (OpenAI)
					if functionResponse := part.Get("functionResponse"); functionResponse.Exists() {
						// Create tool message for function response
						toolMsg := []byte(`{"role":"tool","tool_call_id":"","content":""}`)

						// Convert response.content to JSON string
						if response := functionResponse.Get("response"); response.Exists() {
							if contentField := response.Get("content"); contentField.Exists() {
								toolMsg, _ = sjson.SetBytes(toolMsg, "content", contentField.Raw)
							} else {
								toolMsg, _ = sjson.SetBytes(toolMsg, "content", response.Raw)
							}
						}

						// Resolve the tool_call_id so it ROUND-TRIPS with the call.
						// Priority:
						//   1. The response's own id (client preserved it) -> use verbatim;
						//      the bridge sees the exact id it emitted.
						//   2. The deterministic id we minted for this function name in the
						//      call branch (FIFO match by name+position) -> a call and its
						//      id-less response always agree.
						//   3. A deterministic id from the response name + position, so even an
						//      orphan response is stable (never the positional last-id heuristic
						//      that misfired when calls/responses interleave).
						respName := functionResponse.Get("name").String()
						var toolCallID string
						if id := functionResponse.Get("id").String(); id != "" {
							toolCallID = id
						} else if q := mintedByName[respName]; len(q) > 0 {
							toolCallID = q[0]
							mintedByName[respName] = q[1:]
						} else {
							toolCallID = mintDeterministicToolCallID(respName, mintedCountByName[respName])
							mintedCountByName[respName]++
						}
						toolMsg, _ = sjson.SetBytes(toolMsg, "tool_call_id", toolCallID)

						out, _ = sjson.SetRawBytes(out, "messages.-1", toolMsg)
					}

					return true
				})
			}

			// Set content
			if contentPartsCount > 0 {
				if onlyTextContent {
					msg, _ = sjson.SetBytes(msg, "content", textBuilder.String())
				} else {
					msg, _ = sjson.SetRawBytes(msg, "content", []byte(gjson.GetBytes(contentWrapper, "arr").Raw))
				}
			}

			// Set tool calls if any
			if toolCallsCount > 0 {
				msg, _ = sjson.SetRawBytes(msg, "tool_calls", []byte(gjson.GetBytes(toolCallsWrapper, "arr").Raw))
			}

			// H14: a function-response-ONLY continuation (only functionResponse parts,
			// no text/image/functionCall) must NOT emit a trailing empty user/assistant
			// message. The functionResponse parts were already appended directly as
			// role:"tool" messages above, so the outer msg carries nothing. Emitting an
			// empty role:"user" here would make the translated history end on an empty
			// user turn, which the executor's continuation detector reads as a fresh
			// (empty) user turn instead of a tool_results continuation — the paused run
			// would never receive the tool output. Append the outer msg only when it has
			// real content (text/image parts) or tool calls.
			if contentPartsCount == 0 && toolCallsCount == 0 {
				return true
			}

			out, _ = sjson.SetRawBytes(out, "messages.-1", msg)
			return true
		})
	}

	// Tools mapping: Gemini tools -> OpenAI tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if functionDeclarations := tool.Get("functionDeclarations"); functionDeclarations.Exists() && functionDeclarations.IsArray() {
				functionDeclarations.ForEach(func(_, funcDecl gjson.Result) bool {
					openAITool := []byte(`{"type":"function","function":{"name":"","description":""}}`)
					openAITool, _ = sjson.SetBytes(openAITool, "function.name", funcDecl.Get("name").String())
					openAITool, _ = sjson.SetBytes(openAITool, "function.description", funcDecl.Get("description").String())

					// Convert parameters schema
					if parameters := funcDecl.Get("parameters"); parameters.Exists() {
						openAITool, _ = sjson.SetRawBytes(openAITool, "function.parameters", []byte(parameters.Raw))
					} else if parameters := funcDecl.Get("parametersJsonSchema"); parameters.Exists() {
						openAITool, _ = sjson.SetRawBytes(openAITool, "function.parameters", []byte(parameters.Raw))
					}

					out, _ = sjson.SetRawBytes(out, "tools.-1", openAITool)
					return true
				})
			}
			return true
		})
	}

	// Tool choice mapping. Gemini's functionCallingConfig.mode maps to OpenAI tool_choice;
	// allowedFunctionNames (H15) further restricts WHICH tools may be called. Gemini and
	// OpenAI default to AUTO when the mode is unset, so an allowedFunctionNames restriction
	// is honored even without an explicit mode.
	if toolConfig := root.Get("toolConfig"); toolConfig.Exists() {
		if functionCallingConfig := toolConfig.Get("functionCallingConfig"); functionCallingConfig.Exists() {
			mode := strings.ToUpper(strings.TrimSpace(functionCallingConfig.Get("mode").String()))

			// Collect allowedFunctionNames (H15): the explicit subset of tool names the
			// model may call. Empty/absent means "no restriction".
			var allowedNames []string
			if allowed := functionCallingConfig.Get("allowedFunctionNames"); allowed.Exists() && allowed.IsArray() {
				allowed.ForEach(func(_, value gjson.Result) bool {
					if name := strings.TrimSpace(value.String()); name != "" {
						allowedNames = append(allowedNames, name)
					}
					return true
				})
			}

			switch mode {
			case "NONE":
				// The model may not call any tool; allowedFunctionNames is irrelevant.
				out, _ = sjson.SetBytes(out, "tool_choice", "none")
			case "ANY":
				// ANY forces the model to call a tool. With exactly one allowed name we can
				// express the strongest constraint the bridge understands — force THAT tool
				// (specific:<name>); the executor resolves the token through the client tools
				// and the bridge advertises only it. With several allowed names we keep
				// "required" (must call SOME tool) and additionally restrict the callable set
				// via allowed_tools (consumed by the executor per the C-TOOLCHOICE contract).
				if len(allowedNames) == 1 {
					out, _ = sjson.SetBytes(out, "tool_choice", "specific:"+allowedNames[0])
				} else {
					out, _ = sjson.SetBytes(out, "tool_choice", "required")
					out = setOpenAIAllowedTools(out, allowedNames)
				}
			default:
				// AUTO (explicit or default): the model MAY call a tool. A single allowed
				// name is NOT a forced call here (the model can still answer in text), so it
				// is an allow-restriction, not specific:<name>. Restrict the callable set via
				// allowed_tools when a subset was provided.
				//
				// Only emit tool_choice when there is a real signal: an explicit AUTO mode,
				// or an allowedFunctionNames restriction (which needs "auto" as its base
				// choice). An empty/unknown mode with no allowed names carries no signal, so
				// leave tool_choice untouched — preserving the prior behavior where OpenAI's
				// implicit default (auto) applies and no field is written.
				if mode == "AUTO" || len(allowedNames) > 0 {
					out, _ = sjson.SetBytes(out, "tool_choice", "auto")
					out = setOpenAIAllowedTools(out, allowedNames)
				}
			}
		}
	}

	return out
}

// geminiFileDataPart translates a Gemini `fileData` part (media referenced by URI
// instead of inline base64) into an OpenAI content part (ADD-54). Without this the
// part is silently dropped and a URI-attached image/file never reaches the model.
//
// Behavior:
//   - Image MIME type with a non-empty URI -> OpenAI {"type":"image_url",...} carrying
//     the URI verbatim (a fetchable reference the downstream consumer can resolve).
//   - Any other MIME type, or a present-but-empty URI -> a visible OpenAI text part that
//     names the attachment and its type. This degrades to a model-visible marker rather
//     than silently discarding the part. (This translator returns []byte with no error
//     channel; a visible marker is the "explicit unsupported-media" surface, mirroring the
//     claude->gemini translator's file_data handling.)
//
// Both the camelCase wire form (`fileData.fileUri`/`fileData.mimeType`) and the snake_case
// form some SDKs emit (`file_data.file_uri`/`file_data.mime_type`) are accepted.
//
// onlyText is set to false whenever a part is produced, because the produced part is
// emitted through the structured content array (not the plain-string fast path), so the
// caller must not collapse the message to bare text and lose it.
//
// Returns the content-part JSON and true when a `fileData` part exists; (nil, false) when
// the part has neither a fileData nor file_data field (nothing to translate).
func geminiFileDataPart(part gjson.Result, onlyText *bool) ([]byte, bool) {
	fileData := part.Get("fileData")
	if !fileData.Exists() {
		fileData = part.Get("file_data")
	}
	if !fileData.Exists() {
		return nil, false
	}

	fileURI := fileData.Get("fileUri").String()
	if fileURI == "" {
		fileURI = fileData.Get("file_uri").String()
	}
	mimeType := fileData.Get("mimeType").String()
	if mimeType == "" {
		mimeType = fileData.Get("mime_type").String()
	}

	if onlyText != nil {
		*onlyText = false
	}

	// Supported case: an image referenced by a usable URI -> OpenAI image_url.
	if fileURI != "" && strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", fileURI)
		return contentPart, true
	}

	// Unsupported / non-image / missing-URI: surface a visible marker. Never drop.
	mimeLabel := mimeType
	if mimeLabel == "" {
		mimeLabel = "unknown type"
	}
	var marker string
	if fileURI != "" {
		marker = fmt.Sprintf("[unsupported file attachment: %s (%s) — this media type cannot be forwarded and was omitted]", fileURI, mimeLabel)
	} else {
		marker = fmt.Sprintf("[unsupported file attachment (%s) — missing file URI; the attachment was omitted]", mimeLabel)
	}
	contentPart := []byte(`{"type":"text","text":""}`)
	contentPart, _ = sjson.SetBytes(contentPart, "text", marker)
	return contentPart, true
}

// setOpenAIAllowedTools writes a first-class `allowed_tools` object onto the OpenAI body
// when a restricted set of callable tool names is provided (H15 / C-TOOLCHOICE). Chat
// Completions has no native allowed_tools field, so — exactly like the Responses request
// translator — we carry the restriction through verbatim in the OpenAI Responses
// allowed_tools shape: {"type":"allowed_tools","mode":"auto","tools":[{"type":"function",
// "name":"<n>"},...]}. The composer executor consumes `allowed_tools.tools[].name` and
// intersects it with the advertised tools to a restricted advertise set (best-effort
// gating via the bridge advertise set — Cursor's native built-in tools cannot be
// structurally un-advertised, so this is not a hard server-side guarantee). The actual
// force-vs-auto decision is carried separately in tool_choice; the object's own "mode" is
// kept "auto" so it expresses only the allow-list, not a force. No restriction is written
// for an empty/nil set so an unrestricted turn still advertises all tools.
func setOpenAIAllowedTools(out []byte, names []string) []byte {
	if len(names) == 0 {
		return out
	}
	allowed := []byte(`{"type":"allowed_tools","mode":"auto","tools":[]}`)
	for _, name := range names {
		toolRef := []byte(`{"type":"function","name":""}`)
		toolRef, _ = sjson.SetBytes(toolRef, "name", name)
		allowed, _ = sjson.SetRawBytes(allowed, "tools.-1", toolRef)
	}
	out, _ = sjson.SetRawBytes(out, "allowed_tools", allowed)
	return out
}
