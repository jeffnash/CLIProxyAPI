package helps

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
)

// CodeBuddyStream decodes one CodeBuddy SSE response into validated
// OpenAI-compatible JSON chunks. It never returns SSE framing bytes and never
// buffers the whole stream: only tool arguments needed for final validation
// are retained, bounded to 8 MiB per stream.
type CodeBuddyStream struct {
	reader            *bufio.Reader
	expectedModel     string
	data              []byte
	seenPayload       bool
	finished          bool
	done              bool
	terminalErr       error
	tools             map[int]*codeBuddyToolState
	toolIDs           map[string]int
	toolArgumentBytes int
	// seenContent tracks emitted content/tool/message output for terminal
	// classification; sawMessage rejects cumulative/delta mixing.
	seenContent bool
	sawMessage  bool
	// observedModel records the last upstream model identifier actually
	// returned. Absent identifiers stay unreported, never invented.
	observedModel    string
	hasObservedModel bool
}

// ObservedModel returns the upstream model identifier actually returned, if
// any event carried one.
func (s *CodeBuddyStream) ObservedModel() (string, bool) {
	if s == nil || !s.hasObservedModel {
		return "", false
	}
	return s.observedModel, true
}

// codeBuddyToolState accumulates one tool call across fragmented deltas.
type codeBuddyToolState struct {
	ID        string
	Name      string
	Type      string
	Arguments strings.Builder
}

// NewCodeBuddyStream builds a decoder expecting the requested upstream model.
func NewCodeBuddyStream(r io.Reader, upstreamModel string) *CodeBuddyStream {
	return &CodeBuddyStream{
		reader:        bufio.NewReader(r),
		expectedModel: upstreamModel,
		tools:         map[int]*codeBuddyToolState{},
		toolIDs:       map[string]int{},
	}
}

// Next returns one validated OpenAI-compatible JSON chunk, or io.EOF only
// after a valid terminal sequence.
func (s *CodeBuddyStream) Next() ([]byte, error) {
	if s.terminalErr != nil {
		return nil, s.terminalErr
	}
	for {
		event, terminal, err := s.nextEvent()
		if err != nil {
			s.terminalErr = err
			return nil, err
		}
		if terminal {
			return nil, io.EOF
		}
		chunk, skip, err := s.processEvent(event)
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			s.terminalErr = err
			return nil, err
		}
		if skip {
			continue
		}
		return chunk, nil
	}
}

// nextEvent reads one SSE data payload. It returns terminal=true at a clean
// terminal (valid DONE or valid finish then EOF). Event framing accepts
// LF/CRLF, ignores comments and metadata fields, and joins consecutive data
// lines with newline. Line bytes are assembled in bounded fragments so the
// event budget is enforced before additional allocation.
func (s *CodeBuddyStream) nextEvent() (event []byte, terminal bool, err error) {
	s.data = s.data[:0]
	var eventBytes int64
	flush := func() ([]byte, bool) {
		if len(s.data) == 0 {
			return nil, false
		}
		out := bytes.TrimSpace(append([]byte(nil), s.data...))
		s.data = s.data[:0]
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	}
	for {
		line, readErr := s.readBoundedLine(&eventBytes)
		if len(line) > 0 {
			eventBytes += int64(len(line))
			text := strings.TrimSuffix(string(line), "\n")
			text = strings.TrimSuffix(text, "\r")
			switch {
			case text == "":
				if payload, ok := flush(); ok {
					return payload, false, nil
				}
			case strings.HasPrefix(text, ":"):
				// Comment/heartbeat: ignored.
			case strings.HasPrefix(text, "data:"):
				value := strings.TrimPrefix(text[len("data:"):], " ")
				if len(s.data) > 0 {
					s.data = append(s.data, '\n')
				}
				s.data = append(s.data, value...)
			default:
				// event:/id:/retry: metadata: ignored.
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				var typed *CodeBuddyError
				if errors.As(readErr, &typed) {
					return nil, false, readErr
				}
				return nil, false, fmt.Errorf("codebuddy: stream read failed: %w", readErr)
			}
			if payload, ok := flush(); ok {
				return payload, false, nil
			}
			if termErr := s.terminalAtEOF(); termErr != nil {
				if termErr == io.EOF {
					return nil, true, nil
				}
				return nil, false, termErr
			}
			return nil, true, nil
		}
	}
}

// readBoundedLine reads one LF-terminated line in bounded fragments,
// enforcing the remaining event budget before additional allocation. It
// returns unwrapped io.EOF at end of input, like ReadBytes.
func (s *CodeBuddyStream) readBoundedLine(eventBytes *int64) ([]byte, error) {
	var line []byte
	for {
		fragment, readErr := s.reader.ReadSlice('\n')
		if len(fragment) > 0 {
			if *eventBytes+int64(len(line)+len(fragment)) > codebuddy.SSEEventLimit {
				return nil, codeBuddyStreamError("response_too_large", "codebuddy SSE event exceeds size limit")
			}
			line = append(line, fragment...)
		}
		if readErr == nil {
			return line, nil
		}
		if readErr == bufio.ErrBufferFull {
			continue
		}
		return line, readErr
	}
}

// terminalAtEOF validates a clean EOF: either DONE was seen or a valid
// finish arrived. It returns nil only for a valid terminal, and io.EOF as a
// sentinel the caller converts; any other error is terminal.
func (s *CodeBuddyStream) terminalAtEOF() error {
	if s.done {
		return io.EOF
	}
	if s.finished {
		return io.EOF
	}
	if !s.seenPayload {
		return fmt.Errorf("codebuddy: empty stream (no payload before close)")
	}
	if !s.seenContent {
		return fmt.Errorf("codebuddy: incomplete stream (role-only before close)")
	}
	return fmt.Errorf("codebuddy: truncated stream (no finish or DONE before close)")
}

// processEvent validates one data payload. done responses are handled here;
// JSON events return a chunk or skip=true for heartbeats.
func (s *CodeBuddyStream) processEvent(event []byte) (chunk []byte, skip bool, err error) {
	if string(event) == "[DONE]" {
		if s.done {
			return nil, false, fmt.Errorf("codebuddy: duplicate DONE")
		}
		if !s.finished {
			return nil, false, fmt.Errorf("codebuddy: DONE before finish")
		}
		s.done = true
		return nil, false, io.EOF
	}
	decoder := json.NewDecoder(bytes.NewReader(event))
	decoder.UseNumber()
	var parsed map[string]any
	if err := decoder.Decode(&parsed); err != nil {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: malformed stream event")
	}
	if rawCode, ok := parsed["code"]; ok && isCodeBuddyFailureCode(rawCode) {
		return nil, false, ClassifyCodeBuddyError(http.StatusOK, nil, event, time.Now())
	}
	if rawErr, ok := parsed["error"]; ok && rawErr != nil {
		return nil, false, ClassifyCodeBuddyError(http.StatusOK, nil, event, time.Now())
	}
	if s.done {
		return nil, false, fmt.Errorf("codebuddy: data after DONE")
	}
	if model, ok := parsed["model"].(string); ok && model != "" {
		if s.expectedModel != "" && model != s.expectedModel {
			return nil, false, &CodeBuddyError{Status: http.StatusBadGateway, Code: "model_mismatch", Message: fmt.Sprintf("codebuddy returned model %q for requested %q", model, s.expectedModel)}
		}
		s.observedModel, s.hasObservedModel = model, true
	}
	rawChoices, hasChoices := parsed["choices"]
	if !hasChoices || rawChoices == nil {
		if _, ok := parsed["usage"]; ok {
			s.seenPayload = true
			return event, false, nil
		}
		return nil, true, nil
	}
	choices, ok := rawChoices.([]any)
	if !ok {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: stream choices must be an array")
	}
	if len(choices) == 0 {
		if _, ok := parsed["usage"]; ok {
			s.seenPayload = true
			return event, false, nil
		}
		return nil, true, nil
	}
	if len(choices) > 1 {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: stream carries multiple choices")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: stream choice must be an object")
	}
	if index, ok := choice["index"]; ok && index != nil {
		if !isCodeBuddyChoiceIndexZero(index) {
			return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: stream choice index must be 0")
		}
	}
	_, hasDelta := choice["delta"]
	rawMessage, hasMessage := choice["message"]
	if hasDelta && hasMessage && rawMessage != nil {
		if delta, ok := choice["delta"].(map[string]any); !ok || delta != nil {
			return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: stream choice mixes delta and message")
		}
	}
	finish := codeBuddyFinishReason(choice["finish_reason"])
	if hasMessage && rawMessage != nil {
		return s.processMessageEvent(event, parsed, choice, rawMessage, finish)
	}
	return s.processDeltaEvent(event, parsed, choice, finish)
}

// processMessageEvent normalizes a terminal cumulative message payload when no
// content/tool deltas were already emitted.
func (s *CodeBuddyStream) processMessageEvent(event []byte, parsed map[string]any, choice map[string]any, rawMessage any, finish string) ([]byte, bool, error) {
	message, ok := rawMessage.(map[string]any)
	if !ok {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: stream message must be an object")
	}
	if s.seenContent {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: cumulative message after deltas")
	}
	delta := map[string]any{}
	if role, ok := message["role"].(string); ok && role != "" {
		delta["role"] = role
	}
	content, _ := message["content"].(string)
	if content != "" {
		delta["content"] = content
	}
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if rawCalls, ok := message["tool_calls"]; ok && rawCalls != nil {
		calls, ok := rawCalls.([]any)
		if !ok {
			return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: message tool_calls must be an array")
		}
		normalized, err := s.registerMessageTools(calls)
		if err != nil {
			return nil, false, err
		}
		delta["tool_calls"] = normalized
	}
	if content == "" {
		if _, hasReasoning := delta["reasoning_content"]; !hasReasoning {
			if _, hasTools := delta["tool_calls"]; !hasTools && finish == "" {
				return nil, true, nil
			}
		}
	}
	choice["delta"] = delta
	delete(choice, "message")
	if finish != "" {
		if err := s.finishStream(finish); err != nil {
			return nil, false, err
		}
	}
	s.seenPayload = true
	s.seenContent = true
	s.sawMessage = true
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: encode stream chunk: "+err.Error())
	}
	return encoded, false, nil
}

// processDeltaEvent validates one delta chunk, normalizing missing tool
// indices deterministically.
func (s *CodeBuddyStream) processDeltaEvent(event []byte, parsed map[string]any, choice map[string]any, finish string) ([]byte, bool, error) {
	rawDelta, hasDelta := choice["delta"]
	if !hasDelta || rawDelta == nil {
		if finish == "" {
			if _, ok := parsed["usage"]; ok {
				s.seenPayload = true
				return event, false, nil
			}
			return nil, true, nil
		}
		if err := s.finishStream(finish); err != nil {
			return nil, false, err
		}
		s.seenPayload = true
		return event, false, nil
	}
	delta, ok := rawDelta.(map[string]any)
	if !ok {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: stream delta must be an object")
	}
	content, _ := delta["content"].(string)
	reasoning, _ := delta["reasoning_content"].(string)
	rawCalls, hasCalls := delta["tool_calls"]
	hasOutput := content != "" || reasoning != "" || hasCalls
	if hasOutput && s.sawMessage {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: deltas after cumulative message")
	}
	if hasOutput && s.finished {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: content after finish")
	}
	normalized := false
	if hasCalls && rawCalls != nil {
		calls, ok := rawCalls.([]any)
		if !ok {
			return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: delta tool_calls must be an array")
		}
		rewrite, err := s.registerDeltaTools(calls)
		if err != nil {
			return nil, false, err
		}
		if rewrite {
			delta["tool_calls"] = calls
			normalized = true
		}
	}
	if finish != "" {
		if err := s.finishStream(finish); err != nil {
			return nil, false, err
		}
	}
	s.seenPayload = true
	if hasOutput {
		s.seenContent = true
	}
	if !normalized {
		return event, false, nil
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		return nil, false, codeBuddyStreamError("upstream_error", "codebuddy: encode stream chunk: "+err.Error())
	}
	return encoded, false, nil
}

// finishStream records the terminal finish reason. Complete tool calls are
// validated exactly once at a tool_calls finish; any other terminal reason
// (for example length truncation mid-arguments) preserves partial arguments
// verbatim instead of failing the response.
func (s *CodeBuddyStream) finishStream(finish string) error {
	if s.finished {
		return codeBuddyStreamError("upstream_error", "codebuddy: duplicate finish")
	}
	s.finished = true
	if finish != "tool_calls" {
		return nil
	}
	return s.validateFinalTools()
}

// validateFinalTools requires complete tool identity and JSON-object
// arguments at the terminal boundary.
func (s *CodeBuddyStream) validateFinalTools() error {
	for index, state := range s.tools {
		if strings.TrimSpace(state.ID) == "" || strings.TrimSpace(state.Name) == "" {
			return codeBuddyStreamError("upstream_error", fmt.Sprintf("codebuddy: tool %d missing id or name at finish", index))
		}
		var args map[string]any
		decoder := json.NewDecoder(strings.NewReader(state.Arguments.String()))
		decoder.UseNumber()
		if err := decoder.Decode(&args); err != nil || args == nil {
			return codeBuddyStreamError("upstream_error", fmt.Sprintf("codebuddy: tool %d has invalid arguments at finish", index))
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return codeBuddyStreamError("upstream_error", fmt.Sprintf("codebuddy: tool %d has trailing content after arguments at finish", index))
		}
	}
	return nil
}

// registerDeltaTools appends fragmented tool arguments, resolving missing
// indices deterministically. It reports whether the chunk needs rewriting.
func (s *CodeBuddyStream) registerDeltaTools(calls []any) (bool, error) {
	rewrite := false
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			return false, codeBuddyStreamError("upstream_error", "codebuddy: tool call must be an object")
		}
		id, _ := call["id"].(string)
		index, hasIndex, err := s.resolveToolIndex(call, id)
		if err != nil {
			return false, err
		}
		if !hasIndex {
			call["index"] = index
			rewrite = true
		}
		state := s.tools[index]
		if state == nil {
			state = &codeBuddyToolState{}
			s.tools[index] = state
		}
		callType, _ := call["type"].(string)
		var name, args string
		if rawFunction, ok := call["function"]; ok && rawFunction != nil {
			function, ok := rawFunction.(map[string]any)
			if !ok {
				return false, codeBuddyStreamError("upstream_error", "codebuddy: tool function must be an object")
			}
			name, _ = function["name"].(string)
			if rawArgs, ok := function["arguments"]; ok && rawArgs != nil {
				args, ok = rawArgs.(string)
				if !ok {
					return false, codeBuddyStreamError("upstream_error", "codebuddy: tool arguments must be a string")
				}
			}
		}
		if id != "" {
			if prev, ok := s.toolIDs[id]; ok && prev != index {
				return false, codeBuddyStreamError("upstream_error", fmt.Sprintf("codebuddy: tool id %q moved across indices", id))
			}
			if state.ID != "" && state.ID != id {
				return false, codeBuddyStreamError("upstream_error", fmt.Sprintf("codebuddy: tool index %d changed id", index))
			}
			state.ID = id
			s.toolIDs[id] = index
		}
		if callType != "" {
			if state.Type != "" && state.Type != callType {
				return false, codeBuddyStreamError("upstream_error", fmt.Sprintf("codebuddy: tool index %d changed type", index))
			}
			state.Type = callType
		}
		if name != "" {
			if state.Name != "" && state.Name != name {
				return false, codeBuddyStreamError("upstream_error", fmt.Sprintf("codebuddy: tool index %d changed name", index))
			}
			state.Name = name
		}
		if args != "" {
			s.toolArgumentBytes += len(args)
			if s.toolArgumentBytes > int(codebuddy.SSEEventLimit) {
				return false, codeBuddyStreamError("response_too_large", "codebuddy: tool arguments exceed size limit")
			}
			state.Arguments.WriteString(args)
		}
	}
	return rewrite, nil
}

// resolveToolIndex maps a delta tool entry to its index.
func (s *CodeBuddyStream) resolveToolIndex(call map[string]any, id string) (int, bool, error) {
	if rawIndex, ok := call["index"]; ok && rawIndex != nil {
		index, err := codeBuddyToolIndexValue(rawIndex)
		if err != nil {
			return 0, false, codeBuddyStreamError("upstream_error", "codebuddy: tool index must be a non-negative integer")
		}
		return index, true, nil
	}
	if id != "" {
		if index, ok := s.toolIDs[id]; ok {
			return index, false, nil
		}
		return s.nextToolIndex(), false, nil
	}
	if len(s.tools) == 1 {
		for index := range s.tools {
			return index, false, nil
		}
	}
	if len(s.tools) == 0 {
		return 0, false, nil
	}
	return 0, false, codeBuddyStreamError("upstream_error", "codebuddy: ambiguous tool arguments without index or id")
}

// nextToolIndex returns the next unused tool index deterministically.
func (s *CodeBuddyStream) nextToolIndex() int {
	next := 0
	for index := range s.tools {
		if index >= next {
			next = index + 1
		}
	}
	return next
}

// registerMessageTools registers complete tool calls from a cumulative
// message payload.
func (s *CodeBuddyStream) registerMessageTools(calls []any) ([]any, error) {
	normalized := make([]any, 0, len(calls))
	for position, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			return nil, codeBuddyStreamError("upstream_error", "codebuddy: tool call must be an object")
		}
		id, _ := call["id"].(string)
		index := position
		if rawIndex, ok := call["index"]; ok && rawIndex != nil {
			value, err := codeBuddyToolIndexValue(rawIndex)
			if err != nil {
				return nil, codeBuddyStreamError("upstream_error", "codebuddy: tool index must be a non-negative integer")
			}
			index = value
		} else {
			call["index"] = index
		}
		callType, _ := call["type"].(string)
		function, _ := call["function"].(map[string]any)
		var name, args string
		if function != nil {
			name, _ = function["name"].(string)
			if rawArgs, ok := function["arguments"]; ok && rawArgs != nil {
				var okArgs bool
				args, okArgs = rawArgs.(string)
				if !okArgs {
					return nil, codeBuddyStreamError("upstream_error", "codebuddy: tool arguments must be a string")
				}
			}
		}
		if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" {
			return nil, codeBuddyStreamError("upstream_error", "codebuddy: message tool call missing id or name")
		}
		if prev, ok := s.toolIDs[id]; ok && prev != index {
			return nil, codeBuddyStreamError("upstream_error", fmt.Sprintf("codebuddy: tool id %q moved across indices", id))
		}
		state := s.tools[index]
		if state == nil {
			state = &codeBuddyToolState{}
			s.tools[index] = state
		}
		state.ID, state.Name, state.Type = id, name, callType
		state.Arguments.WriteString(args)
		s.toolArgumentBytes += len(args)
		if s.toolArgumentBytes > int(codebuddy.SSEEventLimit) {
			return nil, codeBuddyStreamError("response_too_large", "codebuddy: tool arguments exceed size limit")
		}
		s.toolIDs[id] = index
		normalized = append(normalized, call)
	}
	return normalized, nil
}

// codeBuddyToolIndexValue parses a tool index number.
func codeBuddyToolIndexValue(raw any) (int, error) {
	switch value := raw.(type) {
	case json.Number:
		text := strings.TrimSpace(value.String())
		var index int
		for i := 0; i < len(text); i++ {
			if text[i] < '0' || text[i] > '9' {
				return 0, fmt.Errorf("invalid index")
			}
			index = index*10 + int(text[i]-'0')
		}
		return index, nil
	case float64:
		if value < 0 || value != float64(int(value)) {
			return 0, fmt.Errorf("invalid index")
		}
		return int(value), nil
	default:
		return 0, fmt.Errorf("invalid index")
	}
}

// isCodeBuddyChoiceIndexZero reports whether a choice index equals zero.
func isCodeBuddyChoiceIndexZero(raw any) bool {
	index, err := codeBuddyToolIndexValue(raw)
	return err == nil && index == 0
}

// codeBuddyFinishReason extracts a finish reason string.
func codeBuddyFinishReason(raw any) string {
	reason, _ := raw.(string)
	return strings.TrimSpace(reason)
}

// isCodeBuddyFailureCode reports whether a Tencent business code on a stream
// event indicates failure. Zero and 200 in numeric or string form are
// success markers; any other present value is an error envelope.
func isCodeBuddyFailureCode(raw any) bool {
	switch value := raw.(type) {
	case nil:
		return false
	case json.Number:
		return codeBuddyStreamCodeTextFails(strings.TrimSpace(value.String()))
	case float64:
		return value != 0 && value != 200
	case string:
		return codeBuddyStreamCodeTextFails(strings.TrimSpace(value))
	default:
		return true
	}
}

// codeBuddyStreamCodeTextFails reports whether a textual business code is a
// failure marker. Only exact 0 and 200 count as success.
func codeBuddyStreamCodeTextFails(text string) bool {
	if text == "" {
		return false
	}
	trimmed := strings.TrimLeft(text, "+")
	if trimmed == "" {
		return false
	}
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] < '0' || trimmed[i] > '9' {
			return true
		}
	}
	digits := strings.TrimLeft(trimmed, "0")
	if digits == "" {
		return false
	}
	return digits != "200"
}

// codeBuddyStreamError builds a generic upstream stream failure.
func codeBuddyStreamError(code, message string) *CodeBuddyError {
	return &CodeBuddyError{Status: http.StatusBadGateway, Code: code, Message: message}
}

// AggregateCodeBuddyStream consumes the decoder into one OpenAI-compatible
// completion. Upstream id/model/created/finish/usage are preserved when
// supplied; a missing ID becomes an explicitly local completion ID. Retained
// content is bounded to 64 MiB.
func AggregateCodeBuddyStream(s *CodeBuddyStream) ([]byte, error) {
	var content, reasoning strings.Builder
	var finish, completionID string
	var created any
	var usage json.RawMessage
	type aggTool struct {
		id, name, typ string
		args          strings.Builder
	}
	tools := map[int]*aggTool{}
	order := []int{}
	retained := 0
	account := func(n int) error {
		retained += n
		if retained > int(codebuddy.AggregateLimit) {
			return codeBuddyStreamError("response_too_large", "codebuddy: upstream response exceeds size limit")
		}
		return nil
	}
	for {
		chunk, err := s.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		var parsed map[string]any
		decoder := json.NewDecoder(bytes.NewReader(chunk))
		decoder.UseNumber()
		if err := decoder.Decode(&parsed); err != nil {
			return nil, codeBuddyStreamError("upstream_error", "codebuddy: invalid stream chunk")
		}
		if id, ok := parsed["id"].(string); ok && id != "" && completionID == "" {
			completionID = id
		}
		if value, ok := parsed["created"]; ok && value != nil && created == nil {
			created = value
		}
		if rawUsage, ok := parsed["usage"]; ok && rawUsage != nil {
			if encoded, err := json.Marshal(rawUsage); err == nil && isCompleteCodeBuddyUsage(encoded) {
				usage = encoded
			}
		}
		choices, _ := parsed["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if choice == nil {
			continue
		}
		if reason := codeBuddyFinishReason(choice["finish_reason"]); reason != "" {
			finish = reason
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if text, ok := delta["content"].(string); ok && text != "" {
			if err := account(len(text)); err != nil {
				return nil, err
			}
			content.WriteString(text)
		}
		if text, ok := delta["reasoning_content"].(string); ok && text != "" {
			if err := account(len(text)); err != nil {
				return nil, err
			}
			reasoning.WriteString(text)
		}
		rawCalls, ok := delta["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, rawCall := range rawCalls {
			call, _ := rawCall.(map[string]any)
			if call == nil {
				continue
			}
			index, err := codeBuddyToolIndexValue(call["index"])
			if err != nil {
				return nil, codeBuddyStreamError("upstream_error", "codebuddy: aggregated tool call missing index")
			}
			state := tools[index]
			if state == nil {
				state = &aggTool{}
				tools[index] = state
				order = append(order, index)
			}
			if id, ok := call["id"].(string); ok && id != "" {
				state.id = id
			}
			if typ, ok := call["type"].(string); ok && typ != "" {
				state.typ = typ
			}
			function, _ := call["function"].(map[string]any)
			if function == nil {
				continue
			}
			if name, ok := function["name"].(string); ok && name != "" {
				state.name = name
			}
			if args, ok := function["arguments"].(string); ok && args != "" {
				if err := account(len(args)); err != nil {
					return nil, err
				}
				state.args.WriteString(args)
			}
		}
	}
	if completionID == "" {
		completionID = codeBuddyLocalCompletionID()
	}
	if created == nil {
		created = time.Now().Unix()
	}
	message := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(order) > 0 {
		// Emit calls in index order for determinism.
		sort.Ints(order)
		calls := make([]any, 0, len(order))
		for _, index := range order {
			state := tools[index]
			calls = append(calls, map[string]any{
				"id":       state.id,
				"type":     firstNonEmpty(state.typ, "function"),
				"function": map[string]any{"name": state.name, "arguments": state.args.String()},
			})
		}
		message["tool_calls"] = calls
	}
	completion := map[string]any{
		"id":      completionID,
		"object":  "chat.completion",
		"created": created,
		"model":   s.expectedModel,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
	}
	if len(usage) > 0 {
		var decoded any
		if err := json.Unmarshal(usage, &decoded); err == nil {
			completion["usage"] = decoded
		}
	}
	encoded, err := json.Marshal(completion)
	if err != nil {
		return nil, codeBuddyStreamError("upstream_error", "codebuddy: encode completion: "+err.Error())
	}
	return encoded, nil
}

// isCompleteCodeBuddyUsage reports whether a usage frame carries token counts.
func isCompleteCodeBuddyUsage(raw []byte) bool {
	var usage map[string]any
	if err := json.Unmarshal(raw, &usage); err != nil {
		return false
	}
	for _, key := range []string{"total_tokens", "prompt_tokens", "completion_tokens"} {
		if _, ok := usage[key]; ok {
			return true
		}
	}
	return false
}

// codeBuddyLocalCompletionID generates an explicitly local completion ID for
// aggregated responses that carried no upstream ID.
func codeBuddyLocalCompletionID() string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("codebuddy-local-%d", time.Now().UnixNano())
	}
	return "codebuddy-local-" + hex.EncodeToString(raw[:])
}
