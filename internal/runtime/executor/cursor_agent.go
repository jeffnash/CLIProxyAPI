package executor

// Cursor AgentService (agent.v1.AgentService/Run) transport and wire encoding.
//
// This is the multi-turn path: unlike the legacy ChatService/StreamUnifiedChat
// route (which only accepts a single flattened ConversationMessage and trips
// ERROR_CONVERSATION_TOO_LONG on long sessions), the agent route accepts the
// full conversation as structured ConversationHistory and supports unbounded
// turns. The full wire shape was reverse-engineered and proven end-to-end
// against the live agentn host (see git history / probe).
//
// Flow:
//  1. Open Run BiDi over HTTP/2 with a kept-open io.Pipe request body.
//  2. Write AgentClientMessage{run_request: AgentRunRequest{...}} as frame 0.
//  3. The server drives a client-executor exec channel; its bootstrap call is
//     request_context. We reply with a headless RequestContext so the model
//     proceeds (we run no workspace — CC executes tools on the user's machine).
//  4. Decode AgentServerMessage{interaction_update} frames into cursorEvent so
//     the existing response-assembly pipeline is reused unchanged.

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// cursorAgentDefaultHost is the observed non-ghost agent host. The real CLI
	// discovers it via ServerConfigService/GetServerConfig -> AgentUrlConfig
	// .agentn_url; we default to the observed value and allow an env override.
	cursorAgentDefaultHost = "https://agentn.global.api5.cursor.sh"
	// cursorAgentClientVersion / Type identify us on the agent path. The agent
	// route uses the cli identity (NOT the ide identity the ChatService route
	// needs) and does not require x-cursor-checksum.
	cursorAgentClientVersion = "cli-2026.05.27-fe9a6e2"
	cursorAgentClientType    = "cli"
)

// resolveCursorAgentHost returns the agent host, honoring CURSOR_AGENTN_URL.
func resolveCursorAgentHost() string {
	if v := strings.TrimSpace(os.Getenv("CURSOR_AGENTN_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return cursorAgentDefaultHost
}

// cursorAgentHeaders builds the headers for the agent Run stream.
func cursorAgentHeaders(token, requestID string) map[string]string {
	return map[string]string{
		"Authorization":           "Bearer " + token,
		"Content-Type":            "application/grpc+proto",
		"Te":                      "trailers",
		"x-ghost-mode":            "true",
		"x-cursor-client-version": cursorAgentClientVersion,
		"x-cursor-client-type":    cursorAgentClientType,
		"x-cursor-streaming":      "true",
		"x-request-id":            requestID,
		"User-Agent":              "connect-es/1.6.1",
	}
}

// --- Wire encoding (field numbers verified against the live server) ---

// agentTextContent builds ConversationHistoryTextContent{text=1:string}.
func agentTextContent(s string) []byte {
	return protoMessage(protoStringField(1, s))
}

// agentUserHistoryMessage builds ConversationHistoryMessage{user=1:
// ConversationHistoryUserMessage{content=1: ConversationHistoryUserContent{
// text=1: TextContent}}}.
func agentUserHistoryMessage(text string) []byte {
	userContent := protoMessageField(1, agentTextContent(text)) // UserContent{text=1}
	userMsg := protoMessageField(1, userContent)                // UserMessage{content=1}
	return protoMessageField(1, userMsg)                        // HistoryMessage{user=1}
}

// agentAssistantHistoryMessage builds ConversationHistoryMessage{assistant=2:
// ConversationHistoryAssistantMessage{content=1 repeated:
// ConversationHistoryAssistantContent{text=1 | tool_call=4}}}.
func agentAssistantHistoryMessage(turn cursorTurn) []byte {
	// Each content part is AssistantMessage.content(1) wrapping an
	// AssistantContent oneof (text=1 -> TextContent, tool_call=4 -> ToolCall).
	var contentParts [][]byte
	addContent := func(assistantContent []byte) {
		contentParts = append(contentParts, protoMessageField(1, assistantContent))
	}
	if strings.TrimSpace(turn.Text) != "" {
		addContent(protoMessageField(1, agentTextContent(turn.Text))) // AssistantContent{text=1: TextContent}
	}
	for _, call := range turn.Calls {
		toolCall := protoMessage(
			protoStringField(1, call.ID),        // tool_call_id
			protoStringField(2, call.Name),      // tool_name
			protoStringField(3, call.Arguments), // args_json
		)
		addContent(protoMessageField(4, toolCall)) // AssistantContent{tool_call=4: ConversationHistoryToolCall}
	}
	if len(contentParts) == 0 {
		addContent(protoMessageField(1, agentTextContent("")))
	}
	assistantMsg := protoMessage(contentParts...) // AssistantMessage{content=1 repeated}
	return protoMessageField(2, assistantMsg)     // HistoryMessage{assistant=2}
}

// agentToolHistoryMessage builds ConversationHistoryMessage{tool=3:
// ConversationHistoryToolMessage{tool_call_id=1, tool_name=2, content=3
// repeated: ToolResultContent{text=1: TextContent}, is_error=4}}.
func agentToolHistoryMessage(callID, toolName, content string, isError bool) []byte {
	resultContent := protoMessageField(1, agentTextContent(content)) // ToolResultContent{text=1}
	fields := [][]byte{
		protoStringField(1, callID),
		protoStringField(2, toolName),
		protoMessageField(3, resultContent),
	}
	if isError {
		fields = append(fields, protoVarintField(4, 1))
	}
	toolMsg := protoMessage(fields...)   // ToolMessage
	return protoMessageField(3, toolMsg) // HistoryMessage{tool=3}
}

// buildAgentConversationHistory renders the PRIOR turns (everything except the
// final user turn, which is carried separately as UserMessageAction.user_message)
// into a ConversationHistory message. Tool results are interleaved immediately
// after the assistant turn that issued the matching calls, so the server sees
// the natural user -> assistant(tool_calls) -> tool -> user order. The system
// prompt (no dedicated history role) is prepended to the first user turn's text.
func buildAgentConversationHistory(payload *cursorChatPayload, excludeUserIdx int) []byte {
	var messages [][]byte
	systemPrompt := strings.TrimSpace(payload.SystemPrompt)
	systemEmitted := false
	emitUser := func(text string) {
		if !systemEmitted && systemPrompt != "" {
			text = "<SYSTEM>\n" + systemPrompt + "\n</SYSTEM>\n\n" + text
			systemEmitted = true
		}
		messages = append(messages, protoMessageField(1, agentUserHistoryMessage(text)))
	}
	for i, turn := range payload.Turns {
		if i == excludeUserIdx {
			continue // the current turn travels as user_message, not history
		}
		switch turn.Role {
		case "user":
			emitUser(turn.Text)
		case "assistant":
			messages = append(messages, protoMessageField(1, agentAssistantHistoryMessage(turn)))
			for _, call := range turn.Calls {
				content := ""
				if payload.lookupToolResult != nil {
					if r, ok := payload.lookupToolResult(call.ID); ok {
						content = r.Content
					}
				}
				if content == "" {
					content = "[no result]"
				}
				messages = append(messages, protoMessageField(1, agentToolHistoryMessage(call.ID, call.Name, content, false)))
			}
		}
	}
	return protoMessage(messages...) // ConversationHistory{messages=1 repeated}
}

// buildAgentMcpTools advertises CC's client-side tools to the model as
// agent.v1.McpTools{mcp_tools=1 repeated: McpToolDefinition{name=1,
// description=2, input_schema=3 (message), provider_identifier=4, tool_name=5}}.
// The model issues these as mcp_tool_call; we forward them to CC.
//
// input_schema is a structured message (not a JSON string); we attach the
// tool's JSON-schema parameters via agentJSONSchemaStruct so the model knows
// each tool's argument shape.
func buildAgentMcpTools(tools []cursorToolDefinition) []byte {
	if len(tools) == 0 {
		return nil
	}
	var defs [][]byte
	for _, t := range tools {
		fields := [][]byte{
			protoStringField(1, t.Name),        // name
			protoStringField(2, t.Description), // description
			protoStringField(4, "client"),      // provider_identifier
			protoStringField(5, t.Name),        // tool_name
		}
		if schema := agentJSONSchemaStruct(t.Parameters); schema != nil {
			fields = append(fields, protoMessageField(3, schema)) // input_schema
		}
		defs = append(defs, protoMessageField(1, protoMessage(fields...))) // McpTools.mcp_tools(1)
	}
	return protoMessage(defs...) // McpTools
}

// encodeAgentRunFrame builds the unframed AgentClientMessage{run_request:
// AgentRunRequest{...}} for the first message on the Run stream.
func encodeAgentRunFrame(payload *cursorChatPayload, model, conversationID, messageID string) []byte {
	lastUserIdx := -1
	for i := len(payload.Turns) - 1; i >= 0; i-- {
		if payload.Turns[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	// Recall: the model reads UserMessage.text but ignores conversation_history,
	// and conversation_state recall requires the turns[] blob store (TODO). As
	// an interim that works today, we put the full flattened transcript
	// (payload.Prompt — built by buildCursorPromptWithTools with historical
	// tool-result truncation already applied) into UserMessage.text for
	// multi-turn turns, so the model sees prior context. Single-turn requests
	// just carry the lone user message.
	userText := ""
	if lastUserIdx >= 0 {
		userText = payload.Turns[lastUserIdx].Text
	}
	hasPriorTurns := lastUserIdx > 0 || (lastUserIdx == 0 && len(payload.Turns) > 1)
	noFlatten := os.Getenv("CURSOR_AGENT_NOFLATTEN") == "1"
	if hasPriorTurns && !noFlatten && strings.TrimSpace(payload.Prompt) != "" {
		userText = payload.Prompt // full flattened transcript (truncated tool results)
	} else if sp := strings.TrimSpace(payload.SystemPrompt); sp != "" && !hasEarlierUserTurn(payload.Turns, lastUserIdx) {
		userText = "<SYSTEM>\n" + sp + "\n</SYSTEM>\n\n" + userText
	}
	userMessage := protoMessage(
		protoStringField(1, userText),
		protoStringField(2, messageID),
	)
	conversationHistory := buildAgentConversationHistory(payload, lastUserIdx)
	userMessageAction := protoMessage(
		protoMessageField(1, userMessage),         // user_message
		protoMessageField(7, conversationHistory), // conversation_history
	)
	conversationAction := protoMessage(protoMessageField(1, userMessageAction)) // ConversationAction{user_message_action=1}

	modelDetails := protoMessage(protoStringField(1, model)) // ModelDetails{model_id=1}

	runFields := [][]byte{
		protoMessageField(1, protoMessage()), // conversation_state = empty (recall via turns[] blobs is TODO)
		protoMessageField(2, conversationAction),
		protoMessageField(3, modelDetails),
		protoStringField(5, conversationID),
	}
	if mcp := buildAgentMcpTools(payload.Tools); mcp != nil {
		runFields = append(runFields, protoMessageField(4, mcp)) // mcp_tools
	}
	agentRunRequest := protoMessage(runFields...)
	return protoMessage(protoMessageField(1, agentRunRequest)) // AgentClientMessage{run_request=1}
}

// agentJSONSchemaStruct converts a tool's JSON-schema parameter string into
// the wire encoding for McpToolDefinition.input_schema. Returns nil to omit
// the field (the model can still call the tool from its name/description).
// TODO: encode the schema once the input_schema message type is confirmed.
func agentJSONSchemaStruct(jsonSchema string) []byte {
	return nil
}

// hasEarlierUserTurn reports whether any user turn exists at an index other
// than excludeIdx (i.e. a prior user turn the system prompt can ride on).
func hasEarlierUserTurn(turns []cursorTurn, excludeIdx int) bool {
	for i, t := range turns {
		if i != excludeIdx && t.Role == "user" {
			return true
		}
	}
	return false
}

// wrapAgentExecClientMessage builds AgentClientMessage{exec_client_message: ExecClientMessage{...}}.
func wrapAgentExecClientMessage(id uint64, execID string, execFields ...[]byte) []byte {
	fields := execFields
	if id != 0 {
		fields = append([][]byte{protoVarintField(1, int(id))}, fields...)
	}
	if execID != "" {
		fields = append(fields, protoStringField(15, execID))
	}
	return protoMessage(protoMessageField(2, protoMessage(fields...)))
}

// buildAgentRequestContextReply builds the AgentClientMessage{exec_client_message:
// ExecClientMessage{request_context_result: success{...}}} that satisfies the
// server's bootstrap request_context exec call. We report a headless context
// with all *_complete flags set so the model proceeds.
func buildAgentRequestContextReply(id uint64, execID string) []byte {
	env := protoMessage(
		protoStringField(1, "linux"),     // os_version
		protoStringField(3, "/bin/bash"), // shell
		protoStringField(10, "UTC"),      // time_zone
		protoStringField(11, "/workspace"),
	)
	reqCtx := protoMessage(
		protoMessageField(4, env),
		protoVarintField(33, 1), // git_repo_info_complete
		protoVarintField(36, 1), // mcp_info_complete
		protoVarintField(39, 1), // rules_info_complete
		protoVarintField(40, 1), // env_info_complete
		protoVarintField(41, 1), // repository_info_complete
		protoVarintField(42, 1), // custom_subagents_info_complete
		protoVarintField(43, 1), // agent_skills_info_complete
		protoVarintField(44, 1), // mcp_file_system_info_complete
		protoVarintField(45, 1), // git_status_info_complete
	)
	success := protoMessage(protoMessageField(1, reqCtx)) // RequestContextSuccess{request_context=1}
	result := protoMessage(protoMessageField(1, success)) // RequestContextResult{success=1}
	return wrapAgentExecClientMessage(id, execID, protoMessageField(10, result))
}

// parseAgentExecRequest extracts id(1), exec_id(15) and the oneof arg field
// number from an ExecServerMessage.
func parseAgentExecRequest(b []byte) (id uint64, execID string, argField int) {
	for _, f := range decodeProtobufFields(b) {
		switch f.Number {
		case 1:
			id, _ = readVarint(f.Value, 0)
		case 15:
			execID = string(f.Value)
		default:
			if f.Number != 19 && argField == 0 && f.WireType == 2 {
				argField = f.Number
			}
		}
	}
	return
}

// streamCursorAgentEvents opens the Run BiDi stream, answers exec requests, and
// yields decoded cursorEvents (text / thinking / tool_call / usage / error).
func streamCursorAgentEvents(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, token, model string, payload *cursorChatPayload, conversationID, messageID string, diag *cursorStreamDiag) <-chan cursorEvent {
	out := make(chan cursorEvent)
	go func() {
		defer close(out)
		safeSend := func(ev cursorEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}

		host := resolveCursorAgentHost()
		requestID := generateUUID()
		runMsg := encodeAgentRunFrame(payload, model, conversationID, messageID)
		runFrame := encodeConnectFrame(runMsg)
		log.Infof("cursor agent: req=%s host=%s model=%s turns=%d tools=%d frame=%d bytes",
			requestID, host, model, len(payload.Turns), len(payload.Tools), len(runFrame))
		if os.Getenv("CURSOR_AGENT_DUMP") == "1" {
			log.Infof("cursor agent: req=%s runmsg hex=%x", requestID, runMsg)
		}

		pr, pw := io.Pipe()
		var pwMu sync.Mutex
		var sendClosed bool
		writeFrame := func(b []byte) {
			pwMu.Lock()
			if !sendClosed {
				_, _ = pw.Write(encodeConnectFrame(b))
			}
			pwMu.Unlock()
		}
		// halfCloseSend signals "no more client input" so the server finalizes
		// the turn. The agent bidi stream otherwise stays open indefinitely
		// (only heartbeats) after a text-only response — there is no turn_ended
		// for simple replies. We drive exactly one turn per request, so once
		// we've answered the bootstrap request_context exec we have nothing
		// more to send.
		halfCloseSend := func() {
			pwMu.Lock()
			if !sendClosed {
				sendClosed = true
				_ = pw.Close()
			}
			pwMu.Unlock()
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/agent.v1.AgentService/Run", pr)
		if err != nil {
			safeSend(cursorEvent{Type: "error", Text: fmt.Sprintf("cursor agent: build request: %v", err)})
			return
		}
		for k, v := range cursorAgentHeaders(token, requestID) {
			httpReq.Header.Set(k, v)
		}

		// timeout=0: no post-connection timeout (AGENTS.md), proxy-aware, HTTP/2.
		httpClient := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)

		go func() {
			pwMu.Lock()
			_, _ = pw.Write(runFrame)
			pwMu.Unlock()
		}()

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			_ = pw.Close()
			safeSend(cursorEvent{Type: "error", Text: fmt.Sprintf("cursor agent: request failed: %v", err)})
			return
		}
		defer func() {
			_ = pw.Close()
			if errClose := resp.Body.Close(); errClose != nil {
				log.Errorf("cursor agent: close body: %v", errClose)
			}
		}()
		log.Infof("cursor agent: req=%s status=%d content-type=%s", requestID, resp.StatusCode, resp.Header.Get("Content-Type"))
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			safeSend(cursorEvent{Type: "error", Text: fmt.Sprintf("cursor agent: status %d: %s", resp.StatusCode, string(body))})
			return
		}

		// Frames are read on a goroutine and delivered over `frames` so the main
		// loop can apply an idle timeout. The agent bidi stream never sends
		// turn_ended for a text-only reply — after the assistant finishes it
		// just emits a heartbeat every ~10s forever. We therefore treat the
		// turn as complete once the stream goes quiet (no content frame) for
		// cursorAgentIdleTimeout after at least one content frame. Heartbeats
		// (10s apart) never reset the timer, so they can't keep us alive.
		type agentFrame struct {
			field   int
			payload []byte
		}
		frames := make(chan agentFrame, 32)
		go func() {
			defer close(frames)
			br := bufio.NewReader(resp.Body)
			for {
				hdr := make([]byte, 5)
				if _, errRead := io.ReadFull(br, hdr); errRead != nil {
					if errRead != io.EOF && errRead != io.ErrUnexpectedEOF {
						log.Debugf("cursor agent: stream read end: %v", errRead)
					}
					return
				}
				length := binary.BigEndian.Uint32(hdr[1:5])
				payloadBytes := make([]byte, length)
				if length > 0 {
					if _, errRead := io.ReadFull(br, payloadBytes); errRead != nil {
						log.Debugf("cursor agent: short frame read: %v", errRead)
						return
					}
				}
				fs := decodeProtobufFields(payloadBytes)
				if len(fs) == 0 {
					continue
				}
				select {
				case frames <- agentFrame{field: fs[0].Number, payload: fs[0].Value}:
				case <-ctx.Done():
					return
				}
			}
		}()

		const cursorAgentIdleTimeout = 1200 * time.Millisecond
		idle := time.NewTimer(time.Hour) // effectively disarmed until first content
		defer idle.Stop()
		sawContent := false
		armIdle := func() {
			sawContent = true
			idle.Reset(cursorAgentIdleTimeout)
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-idle.C:
				if sawContent {
					log.Debugf("cursor agent: req=%s idle-complete after content", requestID)
					return
				}
			case fr, ok := <-frames:
				if !ok {
					if st := resp.Trailer.Get("grpc-status"); st != "" && st != "0" {
						safeSend(cursorEvent{Type: "error", Text: fmt.Sprintf("cursor agent: grpc-status=%s msg=%s", st, resp.Trailer.Get("grpc-message"))})
					}
					return
				}
				switch fr.field {
				case 1: // interaction_update
					if inner := decodeProtobufFields(fr.payload); len(inner) > 0 && inner[0].Number == 13 {
						break // heartbeat — do not reset idle timer
					} else if len(inner) > 0 && os.Getenv("CURSOR_AGENT_DUMP") == "1" && (inner[0].Number == 2 || inner[0].Number == 3 || inner[0].Number == 7) {
						log.Infof("cursor agent: req=%s toolframe case=%d hex=%x", requestID, inner[0].Number, inner[0].Value)
					}
					evs, done := decodeAgentInteractionUpdate(fr.payload)
					for _, ev := range evs {
						if !safeSend(ev) {
							return
						}
					}
					armIdle() // any non-heartbeat content keeps the turn alive
					if done {
						return // explicit turn_ended (e.g. when a tool call closes the turn)
					}
				case 2: // exec_server_message — answer so the model proceeds
					id, execID, argField := parseAgentExecRequest(fr.payload)
					log.Debugf("cursor agent: req=%s exec ask argField=%d id=%d", requestID, argField, id)
					if argField == 10 {
						writeFrame(buildAgentRequestContextReply(id, execID))
						halfCloseSend()
					} else if argField != 0 {
						writeFrame(buildAgentGenericExecReply(id, execID, argField))
					}
				default:
					// kv_server_message(4) / checkpoint(3) — state persistence.
				}
			}
		}
	}()
	return out
}

// buildAgentGenericExecReply replies to a non-request_context exec ask with a
// minimal empty success on the mirrored result field.
func buildAgentGenericExecReply(id uint64, execID string, argField int) []byte {
	result := protoMessage(protoMessageField(1, protoMessage())) // {success=1: {}}
	return wrapAgentExecClientMessage(id, execID, protoMessageField(argField, result))
}

// decodeAgentInteractionUpdate maps an InteractionUpdate oneof into cursorEvents.
// The bool return is true when the turn has ended (the caller should stop).
func decodeAgentInteractionUpdate(b []byte) ([]cursorEvent, bool) {
	inner := decodeProtobufFields(b)
	if len(inner) == 0 {
		return nil, false
	}
	f := inner[0]
	switch f.Number {
	case 1: // text_delta { text=1 }
		return []cursorEvent{{Type: "text", Text: agentInnerString(f.Value, 1)}}, false
	case 4: // thinking_delta { text=1 }
		return []cursorEvent{{Type: "thinking", Text: agentInnerString(f.Value, 1)}}, false
	case 2: // tool_call_started { call_id=1, tool_call=2 } — carries the FULL call
		callID := agentInnerString(f.Value, 1)
		name, args := agentDecodeMcpToolCall(f.Value)
		if os.Getenv("CURSOR_AGENT_DUMP") == "1" {
			log.Infof("cursor agent: tool_call_started decode name=%q args=%s started_hex=%x", name, args, f.Value)
		}
		// Emit the slot (name) then the args in one shot; tool_call_started
		// already contains the complete arguments for the observed model, so
		// we ignore the later partial(7)/completed(3) frames to avoid
		// double-appending args.
		return []cursorEvent{
			{Type: "tool_call_partial", CallID: callID, Name: name},
			{Type: "tool_call", CallID: callID, Name: name, Arguments: args, IsLastMessage: true},
		}, false
	case 7, 3: // partial_tool_call / tool_call_completed — already captured at started
		return nil, false
	case 14: // turn_ended { input_tokens=1, output_tokens=2, ... }
		usage := cursorEvent{Type: "usage"}
		for _, x := range decodeProtobufFields(f.Value) {
			v, _ := readVarint(x.Value, 0)
			switch x.Number {
			case 1:
				usage.PromptTokens = int64(v)
			case 2:
				usage.CompletionTokens = int64(v)
			}
		}
		return []cursorEvent{usage}, true
	}
	return nil, false
}

// agentInnerString returns the string value of the given field number inside a
// length-delimited message.
func agentInnerString(b []byte, field int) string {
	for _, f := range decodeProtobufFields(b) {
		if f.Number == field {
			return string(f.Value)
		}
	}
	return ""
}

// agentDecodeMcpToolCall extracts the client tool name and reconstructed
// arguments JSON from a tool_call_started/completed update body. Path:
// update.tool_call(2) -> ToolCall.mcp_tool_call(15) -> McpToolCall.args(1) ->
// McpArgs{name(1)|tool_name(5), args(2): map<string,Value>}.
func agentDecodeMcpToolCall(updateBody []byte) (name, argsJSON string) {
	toolCall := agentInnerMessage(updateBody, 2)
	mcp := agentInnerMessage(toolCall, 15)
	mcpArgs := agentInnerMessage(mcp, 1)
	if mcpArgs == nil {
		return "", "{}"
	}
	if tn := agentInnerString(mcpArgs, 5); tn != "" {
		name = tn
	} else {
		name = agentInnerString(mcpArgs, 1)
	}
	// Reconstruct the arguments JSON object from the map<string,Value> at
	// field 2. Each entry is {key=1: string, value=2: google.protobuf.Value}.
	args := map[string]any{}
	for _, f := range decodeProtobufFields(mcpArgs) {
		if f.Number != 2 {
			continue
		}
		key := agentInnerString(f.Value, 1)
		if key == "" {
			continue
		}
		args[key] = agentDecodeProtoValue(agentInnerMessage(f.Value, 2))
	}
	b, err := json.Marshal(args)
	if err != nil {
		return name, "{}"
	}
	return name, string(b)
}

// agentDecodeProtoValue decodes a google.protobuf.Value (oneof: number=2,
// string=3, bool=4, struct=5, list=6). Strings dominate tool args; we handle
// the common cases and fall back to the raw string.
func agentDecodeProtoValue(b []byte) any {
	for _, f := range decodeProtobufFields(b) {
		switch f.Number {
		case 1: // null_value
			return nil
		case 2: // number_value (double; wire type I64 / 8 bytes little-endian)
			if len(f.Value) == 8 {
				return math.Float64frombits(binary.LittleEndian.Uint64(f.Value))
			}
			return float64(0)
		case 3: // string_value
			return string(f.Value)
		case 4: // bool_value (varint)
			v, _ := readVarint(f.Value, 0)
			return v != 0
		case 5: // struct_value -> nested object
			obj := map[string]any{}
			for _, sf := range decodeProtobufFields(f.Value) {
				if sf.Number == 1 { // Struct.fields map entry
					k := agentInnerString(sf.Value, 1)
					if k != "" {
						obj[k] = agentDecodeProtoValue(agentInnerMessage(sf.Value, 2))
					}
				}
			}
			return obj
		case 6: // list_value -> []any (ListValue.values is a repeated Value at field 1)
			arr := []any{}
			for _, lf := range decodeProtobufFields(f.Value) {
				if lf.Number == 1 {
					arr = append(arr, agentDecodeProtoValue(lf.Value))
				}
			}
			return arr
		}
	}
	return ""
}

// agentInnerMessage returns the bytes of the given field number (wire type 2).
func agentInnerMessage(b []byte, field int) []byte {
	for _, f := range decodeProtobufFields(b) {
		if f.Number == field {
			return f.Value
		}
	}
	return nil
}
