package executor

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestEncodeVarint(t *testing.T) {
	tests := []struct {
		input    uint64
		expected []byte
	}{
		{0, []byte{0}},
		{1, []byte{1}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
		{16384, []byte{0x80, 0x80, 0x01}},
	}
	for _, tt := range tests {
		result := encodeVarint(tt.input)
		if !bytes.Equal(result, tt.expected) {
			t.Errorf("encodeVarint(%d) = %v, want %v", tt.input, result, tt.expected)
		}
	}
}

func TestReadVarint(t *testing.T) {
	tests := []struct {
		data     []byte
		offset   int
		expected uint64
		newOff   int
	}{
		{[]byte{0}, 0, 0, 1},
		{[]byte{1}, 0, 1, 1},
		{[]byte{0x80, 0x01}, 0, 128, 2},
		{[]byte{0xac, 0x02}, 0, 300, 2},
		{[]byte{0xff, 0x80, 0x01}, 0, 16511, 3}, // 0x7f | 0x00<<7 | 0x01<<14 = 16511
	}
	for _, tt := range tests {
		val, off := readVarint(tt.data, tt.offset)
		if val != tt.expected || off != tt.newOff {
			t.Errorf("readVarint(%v, %d) = (%d, %d), want (%d, %d)", tt.data, tt.offset, val, off, tt.expected, tt.newOff)
		}
	}
}

func TestProtoStringField(t *testing.T) {
	// Field 1, wire type 2, value "hello"
	result := protoStringField(1, "hello")
	// tag = (1 << 3) | 2 = 10
	if result[0] != 10 {
		t.Errorf("expected tag byte 10, got %d", result[0])
	}
	// length = 5
	if result[1] != 5 {
		t.Errorf("expected length byte 5, got %d", result[1])
	}
	if string(result[2:]) != "hello" {
		t.Errorf("expected value 'hello', got '%s'", string(result[2:]))
	}
}

func TestProtoVarintField(t *testing.T) {
	// Field 2, wire type 0, value 1
	result := protoVarintField(2, 1)
	// tag = (2 << 3) | 0 = 16
	if result[0] != 16 {
		t.Errorf("expected tag byte 16, got %d", result[0])
	}
	if result[1] != 1 {
		t.Errorf("expected value byte 1, got %d", result[1])
	}
}

func TestProtoMessageConcatenation(t *testing.T) {
	a := []byte{1, 2, 3}
	b := []byte{4, 5}
	c := []byte{6}
	result := protoMessage(a, b, c)
	expected := []byte{1, 2, 3, 4, 5, 6}
	if !bytes.Equal(result, expected) {
		t.Errorf("protoMessage = %v, want %v", result, expected)
	}
}

func TestDecodeProtobufFields(t *testing.T) {
	// Encode a message with field 1 (string "test") and field 2 (varint 42)
	msg := protoMessage(
		protoStringField(1, "test"),
		protoVarintField(2, 42),
	)

	fields := decodeProtobufFields(msg)
	if len(fields) != 2 {
		t.Fatalf("expected 2 fields, got %d", len(fields))
	}

	if fields[0].Number != 1 || fields[0].WireType != 2 {
		t.Errorf("field 0: expected number=1 wireType=2, got number=%d wireType=%d", fields[0].Number, fields[0].WireType)
	}
	if string(fields[0].Value) != "test" {
		t.Errorf("field 0: expected value 'test', got '%s'", string(fields[0].Value))
	}

	if fields[1].Number != 2 || fields[1].WireType != 0 {
		t.Errorf("field 1: expected number=2 wireType=0, got number=%d wireType=%d", fields[1].Number, fields[1].WireType)
	}
}

func TestDecodeProtobufNestedMessage(t *testing.T) {
	// Build a message with field 1 containing a nested message
	inner := protoMessage(protoStringField(1, "nested"))
	outer := protoMessage(protoMessageField(1, inner))

	fields := decodeProtobufFields(outer)
	if len(fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(fields))
	}
	if fields[0].Number != 1 || fields[0].WireType != 2 {
		t.Errorf("expected number=1 wireType=2, got number=%d wireType=%d", fields[0].Number, fields[0].WireType)
	}

	innerFields := decodeProtobufFields(fields[0].Value)
	if len(innerFields) != 1 {
		t.Fatalf("expected 1 inner field, got %d", len(innerFields))
	}
	if string(innerFields[0].Value) != "nested" {
		t.Errorf("expected inner value 'nested', got '%s'", string(innerFields[0].Value))
	}
}

// --- Connect protocol framing tests ---

func TestEncodeConnectFrame(t *testing.T) {
	payload := []byte("hello")
	frame := encodeConnectFrame(payload)

	if len(frame) != 5+len(payload) {
		t.Fatalf("expected frame length %d, got %d", 5+len(payload), len(frame))
	}
	if frame[0] != 0 {
		t.Errorf("expected flags byte 0, got %d", frame[0])
	}
	length := binary.BigEndian.Uint32(frame[1:5])
	if length != 5 {
		t.Errorf("expected length 5, got %d", length)
	}
	if !bytes.Equal(frame[5:], payload) {
		t.Errorf("expected payload %v, got %v", payload, frame[5:])
	}
}

func TestEncodeConnectFrameEmptyPayload(t *testing.T) {
	frame := encodeConnectFrame([]byte{})
	if len(frame) != 5 {
		t.Fatalf("expected frame length 5, got %d", len(frame))
	}
	length := binary.BigEndian.Uint32(frame[1:5])
	if length != 0 {
		t.Errorf("expected length 0, got %d", length)
	}
}

// --- Cursor chat request encoding tests ---

func buildTestConnectFrame(payload []byte, flags byte) []byte {
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

// buildChatResponseText builds a protobuf frame containing text content.
// Matches the TypeScript chatResponseText helper: field 2 > field 1 = text
func buildChatResponseText(text string) []byte {
	return protoMessage(protoMessageField(2, protoMessage(protoStringField(1, text))))
}

// buildChatResponseThinking builds a protobuf frame containing thinking content.
// Matches the TypeScript chatResponseThinking helper: field 2 > field 25 > field 1 = text
func buildChatResponseThinking(text string) []byte {
	return protoMessage(protoMessageField(2, protoMessage(protoMessageField(25, protoMessage(protoStringField(1, text))))))
}

func TestCursorTextSanitizerStripsControlTokens(t *testing.T) {
	// Each case feeds chunks via Push and then calls Flush. The test asserts the
	// full concatenation matches `want`. Complete control-token markers are
	// stripped; partial-marker suffixes at EOF are preserved as real content
	// since they never completed into a real control token.
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"closing-think-in-one-chunk", []string{"hello</think>world"}, "helloworld"},
		{"final-marker", []string{"draft<|final|>real"}, "draftreal"},
		{"full-width-final", []string{"draft<｜final｜>real"}, "draftreal"},
		{"split-across-chunks", []string{"abc</thi", "nk>xyz"}, "abcxyz"},
		{"partial-suffix-at-eof", []string{"foo</th"}, "foo</th"}, // Flush preserves the partial
		{"no-marker", []string{"plain text"}, "plain text"},
		{"multiple-markers", []string{"a</think>b<|final|>c"}, "abc"},
		{"completing-marker-spans-flush", []string{"abc</thi", "nk>"}, "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &cursorTextSanitizer{}
			var got strings.Builder
			for _, c := range tc.chunks {
				got.WriteString(s.Push(c))
			}
			got.WriteString(s.Flush())
			if got.String() != tc.want {
				t.Errorf("got %q, want %q", got.String(), tc.want)
			}
		})
	}
}

func TestStreamCursorTextEventsDebug(t *testing.T) {
	var buf bytes.Buffer
	frame1 := buildTestConnectFrame(buildChatResponseText("Hello"), 0)
	frame2 := buildTestConnectFrame(buildChatResponseText(" world"), 0)
	frame3 := buildTestConnectFrame([]byte("{}"), 2)
	t.Logf("frame1 (%d bytes): %x", len(frame1), frame1)
	t.Logf("frame2 (%d bytes): %x", len(frame2), frame2)
	t.Logf("frame3 (%d bytes): %x", len(frame3), frame3)
	buf.Write(frame1)
	buf.Write(frame2)
	buf.Write(frame3)
	t.Logf("total buffer: %d bytes", buf.Len())

	// Read manually
	data := buf.Bytes()
	offset := 0
	frameNum := 0
	for offset < len(data) {
		if offset+5 > len(data) {
			t.Logf("incomplete header at offset %d", offset)
			break
		}
		flags := data[offset]
		length := binary.BigEndian.Uint32(data[offset+1 : offset+5])
		t.Logf("frame %d: offset=%d flags=%d length=%d", frameNum, offset, flags, length)
		offset += 5 + int(length)
		frameNum++
	}
}

func TestResolveCursorModelName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"", "composer-2.5"},
		{"composer-2.5", "composer-2.5"},
		{"composer-2-5", "composer-2.5"},
		{"composer-2.5-sdk", "composer-2.5"},
		{"composer-latest", "composer-2.5"},
		{"auto", "composer-2.5"},
		{"default", "composer-2.5"},
		{"composer-2.5-fast", "composer-2.5-fast"},
		{"composer-2-5-fast", "composer-2.5-fast"},
		{"composer-2", "composer-2"},
		{"gpt-5.2", "gpt-5.2"},
		{"COMPOSER-2.5", "composer-2.5"},
	}
	for _, tt := range tests {
		result := resolveCursorModelName(tt.input)
		if result != tt.expected {
			t.Errorf("resolveCursorModelName(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

// --- Prompt building tests ---

func TestBuildCursorPromptSimple(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "Hello"},
		},
	})
	prompt := buildCursorPrompt(makeTestRequest(payload))
	// Transcript format mirrors composer-api/openai.ts: directive header,
	// Conversation: marker, then ROLE: content lines.
	if !strings.Contains(prompt, "Conversation:") {
		t.Errorf("expected 'Conversation:' marker, got %q", prompt)
	}
	if !strings.Contains(prompt, "USER: Hello") {
		t.Errorf("expected 'USER: Hello' line, got %q", prompt)
	}
}

func TestBuildCursorPromptMultiMessage(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "system", "content": "Be terse."},
			{"role": "user", "content": "What is 2+2?"},
			{"role": "assistant", "content": "4"},
			{"role": "user", "content": "Why?"},
		},
	})
	prompt := buildCursorPrompt(makeTestRequest(payload))
	wantFragments := []string{
		"Conversation:",
		"SYSTEM: Be terse.",
		"USER: What is 2+2?",
		"ASSISTANT: 4",
		"USER: Why?",
	}
	for _, want := range wantFragments {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected prompt to contain %q, got %q", want, prompt)
		}
	}
}

func TestBuildCursorPromptContentArray(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": "What is this?"},
			}},
		},
	})
	prompt := buildCursorPrompt(makeTestRequest(payload))
	if !strings.Contains(prompt, "USER: What is this?") {
		t.Errorf("expected 'USER: What is this?', got %q", prompt)
	}
}

func TestBuildCursorPromptFallback(t *testing.T) {
	// Non-JSON payload: no `messages` array, so the transcript has no
	// USER lines. The system directive header should still be present.
	req := makeTestRequest([]byte("raw prompt text"))
	prompt := buildCursorPrompt(req)
	if !strings.Contains(prompt, "OpenAI-compatible API request") {
		t.Errorf("expected system directive in fallback prompt, got %q", prompt)
	}
}

func TestBuildCursorPromptWithTools(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "go"}},
	})
	tools := []cursorToolDefinition{
		{Name: "Read", Description: "Read a file", Parameters: `{"type":"object","properties":{"file_path":{"type":"string"}}}`},
	}
	prompt := buildCursorPromptWithTools(makeTestRequest(payload), tools, cursorToolChoice{})
	// Tool-aware transcript: TOOL_SYSTEM_DIRECTIVE header + inventory +
	// AGENT_MODE_PRIMER turns conditioning Composer to obey custom tools.
	wantFragments := []string{
		"This request is already in Agent mode because the client provided executable tools.",
		"CLIENT TOOL INVENTORY:",
		"Allowed tool names: Read",
		"call_proxy_switch_mode",                         // primer
		`"name":"switch_mode"`,                           // primer fake tool call
		"ASSISTANT: Great, I've switched to agent mode.", // primer close
		"USER: go",
	}
	for _, want := range wantFragments {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected %q in prompt, got %q", want, prompt)
		}
	}
}

// --- SHA256 and UUID tests ---

func TestSha256Hex(t *testing.T) {
	result := sha256Hex("hello")
	// SHA-256 of "hello" is well-known
	expected := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if result != expected {
		t.Errorf("sha256Hex('hello') = %s, want %s", result, expected)
	}
}

func TestSha256HexEmpty(t *testing.T) {
	result := sha256Hex("")
	expected := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if result != expected {
		t.Errorf("sha256Hex('') = %s, want %s", result, expected)
	}
}

func TestStableUUID(t *testing.T) {
	uuid1 := stableUUID("test-value")
	uuid2 := stableUUID("test-value")
	if uuid1 != uuid2 {
		t.Errorf("stableUUID not deterministic: %s != %s", uuid1, uuid2)
	}
	// UUID format: 8-4-4-4-12
	parts := strings.Split(uuid1, "-")
	if len(parts) != 5 {
		t.Errorf("expected 5 UUID parts, got %d: %s", len(parts), uuid1)
	}
	if len(parts[0]) != 8 || len(parts[1]) != 4 || len(parts[2]) != 4 || len(parts[3]) != 4 || len(parts[4]) != 12 {
		t.Errorf("UUID part lengths wrong: %s", uuid1)
	}
}

func TestStableUUIDDifferentInputs(t *testing.T) {
	uuid1 := stableUUID("input-a")
	uuid2 := stableUUID("input-b")
	if uuid1 == uuid2 {
		t.Error("different inputs should produce different UUIDs")
	}
}

func TestBase64URLEncode(t *testing.T) {
	result := base64URLEncode([]byte{0x00, 0x01, 0x02})
	if result == "" {
		t.Error("base64URLEncode returned empty string")
	}
	// Should not contain standard base64 padding
	if strings.Contains(result, "=") {
		t.Errorf("base64URLEncode should not contain padding: %s", result)
	}
}

// --- Token estimation tests ---

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		chars    int
		expected int
	}{
		{0, 1},    // minimum 1
		{4, 1},    // 4 chars = 1 token
		{5, 2},    // 5 chars = 2 tokens (ceil)
		{100, 25}, // 100 chars = 25 tokens
		{1, 1},    // 1 char = 1 token
	}
	for _, tt := range tests {
		result := estimateTokens(tt.chars)
		if result != tt.expected {
			t.Errorf("estimateTokens(%d) = %d, want %d", tt.chars, result, tt.expected)
		}
	}
}

func TestEstimateUsage(t *testing.T) {
	usage := estimateUsage(100, 200)
	if usage["prompt_tokens"] != 25 {
		t.Errorf("expected prompt_tokens=25, got %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != 50 {
		t.Errorf("expected completion_tokens=50, got %v", usage["completion_tokens"])
	}
	if usage["total_tokens"] != 75 {
		t.Errorf("expected total_tokens=75, got %v", usage["total_tokens"])
	}
}

// --- ExtractMessageContent tests ---

func TestExtractMessageContentString(t *testing.T) {
	result := extractMessageContent("hello")
	if result != "hello" {
		t.Errorf("expected 'hello', got '%s'", result)
	}
}

func TestExtractMessageContentNil(t *testing.T) {
	result := extractMessageContent(nil)
	if result != "" {
		t.Errorf("expected '', got '%s'", result)
	}
}

func TestExtractMessageContentArray(t *testing.T) {
	content := []any{
		map[string]any{"type": "text", "text": "part1"},
		map[string]any{"type": "text", "text": "part2"},
	}
	result := extractMessageContent(content)
	if !strings.Contains(result, "part1") || !strings.Contains(result, "part2") {
		t.Errorf("expected 'part1' and 'part2' in result, got '%s'", result)
	}
}

// --- Integration-level protobuf roundtrip test ---

func TestParseCursorChatPayloadModernAndLegacy(t *testing.T) {
	payloadBytes, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"role": "user", "content": "go"},
			{"role": "assistant", "content": "calling tool", "tool_calls": []map[string]any{
				{"id": "call_1", "type": "function", "function": map[string]any{"name": "read", "arguments": `{"p":"a"}`}},
			}},
			{"role": "tool", "tool_call_id": "call_1", "name": "read", "content": "result-text"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "modern_tool", "description": "modern", "parameters": map[string]any{"type": "object"}}},
		},
		"functions": []map[string]any{
			{"name": "legacy_tool", "description": "legacy", "parameters": map[string]any{"type": "object"}},
		},
	})

	parsed := parseCursorChatPayload(makeTestRequest(payloadBytes))
	if len(parsed.Tools) != 2 {
		t.Fatalf("expected 2 tools (modern + legacy), got %d", len(parsed.Tools))
	}
	if parsed.Tools[0].Name != "modern_tool" || parsed.Tools[1].Name != "legacy_tool" {
		t.Errorf("tool names mismatch: %+v", parsed.Tools)
	}
	if !parsed.HasAssistantCall || len(parsed.AssistantCalls) != 1 {
		t.Errorf("assistant tool_calls not preserved: %+v", parsed.AssistantCalls)
	}
	if parsed.AssistantCalls[0].ID != "call_1" || parsed.AssistantCalls[0].Name != "read" {
		t.Errorf("assistant call lost: %+v", parsed.AssistantCalls[0])
	}
	if !parsed.HasToolMessages || len(parsed.ToolResults) != 1 {
		t.Errorf("tool results not preserved: %+v", parsed.ToolResults)
	}
	if parsed.ToolResults[0].Content != "result-text" {
		t.Errorf("tool result content lost: %+v", parsed.ToolResults[0])
	}
}

// --- Native Cursor tool-call frame decoding tests ---

// buildClientSideToolV2CallFrame builds an outer-field-1 frame containing
// aiserver.v1.ClientSideToolV2Call with the given fields. Field 3=tool_call_id,
// 9=name, 10=raw_args, 15=is_last_message, 48=tool_index.
func buildClientSideToolV2CallFrame(name, callID, args string) []byte {
	parts := [][]byte{}
	if callID != "" {
		parts = append(parts, protoStringField(3, callID))
	}
	if name != "" {
		parts = append(parts, protoStringField(9, name))
	}
	if args != "" {
		parts = append(parts, protoStringField(10, args))
	}
	inner := protoMessage(parts...)
	return protoMessage(protoMessageField(1, inner))
}

// buildPartialToolCallFrame builds a StreamUnifiedChatResponse frame (outer
// field 2) containing partial_tool_call (inner field 15): 2=id, 3=name.
func buildPartialToolCallFrame(name, callID string) []byte {
	inner := protoMessage(
		protoStringField(2, callID),
		protoStringField(3, name),
	)
	resp := protoMessage(protoMessageField(15, inner))
	return protoMessage(protoMessageField(2, resp))
}

// buildStreamedBackToolCallFrame builds a StreamUnifiedChatResponse frame
// (outer field 2) containing tool_call (inner field 13): 2=id, 8=name, 9=raw_args.
func buildStreamedBackToolCallFrame(name, callID, args string) []byte {
	parts := [][]byte{}
	if callID != "" {
		parts = append(parts, protoStringField(2, callID))
	}
	if name != "" {
		parts = append(parts, protoStringField(8, name))
	}
	if args != "" {
		parts = append(parts, protoStringField(9, args))
	}
	inner := protoMessage(parts...)
	resp := protoMessage(protoMessageField(13, inner))
	return protoMessage(protoMessageField(2, resp))
}

func TestBuildCursorOpenAIChunkEscapesDynamicFields(t *testing.T) {
	hostile := `model"with\back\and"quotes`
	delta := map[string]any{
		"tool_calls": []map[string]any{
			{
				"index": 0,
				"id":    `call"with"quotes`,
				"type":  "function",
				"function": map[string]any{
					"name":      `tool\name"weird`,
					"arguments": `{"k":"v\"with\\"}`,
				},
			},
		},
	}
	chunk := buildCursorOpenAIChunk("resp\"\\id", hostile, delta, "tool_calls")
	if !bytes.HasPrefix(chunk, []byte("data: ")) {
		t.Fatalf("expected SSE data prefix, got %q", chunk)
	}
	jsonPart := bytes.TrimPrefix(chunk, []byte("data: "))
	var decoded map[string]any
	if err := json.Unmarshal(jsonPart, &decoded); err != nil {
		t.Fatalf("emitted chunk is not valid JSON: %v\n%s", err, jsonPart)
	}
	if decoded["model"] != hostile {
		t.Errorf("model field lost dynamic value: got %v", decoded["model"])
	}
}

func TestBuildCursorOpenAIUsageChunkMarshalsCleanly(t *testing.T) {
	chunk := buildCursorOpenAIUsageChunk("id\"x", "m\\y", map[string]any{"prompt_tokens": 7})
	jsonPart := bytes.TrimPrefix(chunk, []byte("data: "))
	var decoded map[string]any
	if err := json.Unmarshal(jsonPart, &decoded); err != nil {
		t.Fatalf("usage chunk is not valid JSON: %v\n%s", err, jsonPart)
	}
}

// --- Chat-side CURSOR_CLIENT_VERSION header tests ---

type sharedStateTracker struct {
	calls   int
	gotSame bool
	first   any
}

// streamTranslatorAcrossChunksClosure mirrors the closure-based emit pattern
// used in ExecuteStream. The fix moves the param variable outside the closure
// so the state pointer remains stable across calls.
func streamTranslatorAcrossChunksClosure(emit func(state *any)) {
	var state any
	wrapped := func() { emit(&state) }
	wrapped()
	wrapped()
}

func TestStreamingTranslatorStatePersists(t *testing.T) {
	tracker := &sharedStateTracker{}
	streamTranslatorAcrossChunksClosure(func(state *any) {
		if tracker.calls == 0 {
			*state = "primed"
			tracker.first = state
		} else {
			// If state were reset per-call, the previous "primed" assignment
			// would be lost on this second call.
			if *state != "primed" {
				t.Errorf("expected state to persist across calls, got %v", *state)
			}
			tracker.gotSame = state == tracker.first
		}
		tracker.calls++
	})
	if !tracker.gotSame {
		t.Errorf("expected emit closures to share the same translator state pointer")
	}
}

// --- Helpers ---

// makeTestRequest creates a minimal Request for testing.
func makeTestRequest(payload []byte) cliproxyexecutor.Request {
	return cliproxyexecutor.Request{Payload: payload}
}

// --- Image pipeline tests ---

// onePixelPNG is a 1x1 transparent PNG. Smallest valid PNG header that
// image.DecodeConfig will accept.
var onePixelPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func TestDecodeCursorImage_ValidPNG(t *testing.T) {
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(onePixelPNG)
	img, err := decodeCursorImage(dataURI)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.Width != 1 || img.Height != 1 {
		t.Errorf("expected 1x1, got %dx%d", img.Width, img.Height)
	}
	if len(img.Data) != len(onePixelPNG) {
		t.Errorf("expected %d bytes, got %d", len(onePixelPNG), len(img.Data))
	}
	if img.UUID == "" {
		t.Error("expected non-empty UUID")
	}
}

func TestDecodeCursorImage_DeterministicUUID(t *testing.T) {
	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(onePixelPNG)
	a, _ := decodeCursorImage(dataURI)
	b, _ := decodeCursorImage(dataURI)
	if a.UUID != b.UUID {
		t.Errorf("identical images should produce identical UUIDs; got %s vs %s", a.UUID, b.UUID)
	}
}

func TestDecodeCursorImage_RejectsScheme(t *testing.T) {
	cases := []string{
		"http://example.com/x.png",
		"https://example.com/x.png",
		"file:///etc/passwd",
		"blob:foo",
	}
	for _, uri := range cases {
		if _, err := decodeCursorImage(uri); err == nil {
			t.Errorf("expected rejection for %q", uri)
		}
	}
}

func TestDecodeCursorImage_RejectsSVG(t *testing.T) {
	if _, err := decodeCursorImage("data:image/svg+xml;base64,PHN2Zy8+"); err == nil {
		t.Error("expected SVG to be rejected")
	}
}

func TestDecodeCursorImage_RejectsBadBase64(t *testing.T) {
	if _, err := decodeCursorImage("data:image/png;base64,!!!"); err == nil {
		t.Error("expected base64 decode error")
	}
}

func TestParseCursorChatPayload_AttachesImages(t *testing.T) {
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(onePixelPNG)
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "what is this?"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": uri}},
				},
			},
		},
	}
	raw, _ := json.Marshal(body)
	payload := parseCursorChatPayload(cliproxyexecutor.Request{Payload: raw})
	if len(payload.Images) != 1 {
		t.Fatalf("expected 1 image, got %d (errors: %v)", len(payload.Images), payload.ImageErrors)
	}
	if payload.Images[0].Width != 1 {
		t.Errorf("expected 1x1, got %dx%d", payload.Images[0].Width, payload.Images[0].Height)
	}
}

func TestParseCursorChatPayload_RejectsHTTPImage(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/foo.png"}},
				},
			},
		},
	}
	raw, _ := json.Marshal(body)
	payload := parseCursorChatPayload(cliproxyexecutor.Request{Payload: raw})
	if len(payload.ImageErrors) == 0 {
		t.Fatal("expected an image error for http URL")
	}
	if !strings.Contains(payload.ImageErrors[0], "only data: URIs are supported") {
		t.Errorf("expected data-URI restriction error, got: %s", payload.ImageErrors[0])
	}
}

func TestEncodeImageProto_RoundTrip(t *testing.T) {
	img := cursorImage{Data: onePixelPNG, Width: 1, Height: 1, UUID: "test-uuid"}
	encoded := encodeImageProto(img)
	fields := decodeProtobufFields(encoded)
	m := make(map[int]protoField)
	for _, f := range fields {
		m[f.Number] = f
	}
	if !bytes.Equal(m[1].Value, onePixelPNG) {
		t.Error("field 1 (data) mismatch")
	}
	dimFields := decodeProtobufFields(m[2].Value)
	var w, h int64
	for _, f := range dimFields {
		if f.Number == 1 {
			w = varintFromBytes(f.Value)
		}
		if f.Number == 2 {
			h = varintFromBytes(f.Value)
		}
	}
	if w != 1 || h != 1 {
		t.Errorf("dimension: expected 1x1, got %dx%d", w, h)
	}
	if string(m[3].Value) != "test-uuid" {
		t.Errorf("field 3 (uuid): expected test-uuid, got %s", string(m[3].Value))
	}
}

func TestCursorToolCallEmitter_PartialThenArgumentChunks(t *testing.T) {
	// Simulate the streaming pattern Cursor actually emits: one tool_call_partial
	// (name+id only) followed by multiple tool_call frames each carrying a chunk
	// of arguments under the same CallID. All downstream OpenAI deltas should
	// reuse the same index. The name/id appears only on the first delta.
	em := newCursorToolCallEmitter("req-stableidx")

	events := []cursorEvent{
		{Type: "tool_call_partial", CallID: "call_abc", Name: "write_file"},
		{Type: "tool_call", CallID: "call_abc", Name: "write_file", Arguments: `{"path":"`},
		{Type: "tool_call", CallID: "call_abc", Name: "write_file", Arguments: `/tmp/x"`},
		{Type: "tool_call", CallID: "call_abc", Name: "write_file", Arguments: `,"data":"y"}`},
	}

	var deltas []map[string]any
	for _, ev := range events {
		delta, ok := em.OnEvent(ev)
		if !ok {
			continue
		}
		deltas = append(deltas, delta)
	}

	if len(deltas) != 4 {
		t.Fatalf("expected 4 deltas (one per event), got %d", len(deltas))
	}

	// Every delta should reference index=0 (one logical call).
	for i, d := range deltas {
		tcs := d["tool_calls"].([]map[string]any)
		if len(tcs) != 1 {
			t.Fatalf("delta %d: expected exactly 1 tool_calls entry, got %d", i, len(tcs))
		}
		if tcs[0]["index"] != 0 {
			t.Errorf("delta %d: expected index=0, got %v", i, tcs[0]["index"])
		}
	}

	// Only the first delta should carry id+name; subsequent deltas must omit them
	// (per OpenAI streaming protocol — they only carry argument deltas).
	if deltas[0]["tool_calls"].([]map[string]any)[0]["id"] != "call_abc" {
		t.Errorf("first delta should carry id=call_abc, got %v", deltas[0]["tool_calls"])
	}
	if deltas[0]["tool_calls"].([]map[string]any)[0]["function"].(map[string]any)["name"] != "write_file" {
		t.Errorf("first delta should carry function.name=write_file")
	}
	for i := 1; i < len(deltas); i++ {
		tc := deltas[i]["tool_calls"].([]map[string]any)[0]
		if _, hasID := tc["id"]; hasID {
			t.Errorf("delta %d should not repeat the id; got %v", i, tc["id"])
		}
		fn := tc["function"].(map[string]any)
		if _, hasName := fn["name"]; hasName {
			t.Errorf("delta %d should not repeat function.name", i)
		}
		if _, hasArgs := fn["arguments"]; !hasArgs {
			t.Errorf("delta %d should carry function.arguments delta", i)
		}
	}

	// Concatenating the args of deltas 1..3 should reconstruct the full payload.
	got := ""
	for i := 1; i < len(deltas); i++ {
		got += deltas[i]["tool_calls"].([]map[string]any)[0]["function"].(map[string]any)["arguments"].(string)
	}
	want := `{"path":"/tmp/x","data":"y"}`
	if got != want {
		t.Errorf("reassembled args: got %q, want %q", got, want)
	}
}

func TestCursorToolCallEmitter_TwoIndependentCalls(t *testing.T) {
	// Two distinct CallIDs should get distinct indices (0 and 1).
	em := newCursorToolCallEmitter("req-twoindep")
	d1, _ := em.OnEvent(cursorEvent{Type: "tool_call", CallID: "call_a", Name: "a", Arguments: `{}`})
	d2, _ := em.OnEvent(cursorEvent{Type: "tool_call", CallID: "call_b", Name: "b", Arguments: `{}`})
	if d1["tool_calls"].([]map[string]any)[0]["index"] != 0 {
		t.Errorf("first call: expected index=0")
	}
	if d2["tool_calls"].([]map[string]any)[0]["index"] != 1 {
		t.Errorf("second call: expected index=1")
	}
}

func TestCursorToolCallEmitter_PartialDuplicateIsNoop(t *testing.T) {
	// A second tool_call_partial for the same call_id is a no-op (slot already reserved).
	em := newCursorToolCallEmitter("req-dup")
	_, ok1 := em.OnEvent(cursorEvent{Type: "tool_call_partial", CallID: "call_x", Name: "x"})
	if !ok1 {
		t.Fatal("first partial should emit")
	}
	_, ok2 := em.OnEvent(cursorEvent{Type: "tool_call_partial", CallID: "call_x", Name: "x"})
	if ok2 {
		t.Errorf("duplicate partial should be a no-op")
	}
}

func TestCursorToolCallEmitter_ZeroIndexedNoCallID(t *testing.T) {
	// A multi-chunk tool call with no CallID and ToolIndex=0 must reuse the
	// same OpenAI slot index across all chunks. The presence flag is what
	// distinguishes "server sent index 0" from "no index field at all".
	em := newCursorToolCallEmitter("req-zeroidx")

	events := []cursorEvent{
		{Type: "tool_call_partial", ToolIndex: 0, HasToolIndex: true, Name: "search"},
		{Type: "tool_call", ToolIndex: 0, HasToolIndex: true, Name: "search", Arguments: `{"q":"`},
		{Type: "tool_call", ToolIndex: 0, HasToolIndex: true, Name: "search", Arguments: `hello"}`},
	}
	var deltas []map[string]any
	for _, ev := range events {
		d, ok := em.OnEvent(ev)
		if !ok {
			continue
		}
		deltas = append(deltas, d)
	}
	if len(deltas) != 3 {
		t.Fatalf("expected 3 deltas, got %d", len(deltas))
	}
	for i, d := range deltas {
		tc := d["tool_calls"].([]map[string]any)[0]
		if tc["index"] != 0 {
			t.Errorf("delta %d: expected index=0, got %v", i, tc["index"])
		}
	}
	// Reassembled arguments should match.
	got := ""
	for i := 1; i < len(deltas); i++ {
		got += deltas[i]["tool_calls"].([]map[string]any)[0]["function"].(map[string]any)["arguments"].(string)
	}
	if got != `{"q":"hello"}` {
		t.Errorf("reassembled args: got %q", got)
	}
}

func TestCursorToolCallEmitter_LateNameAfterEmptyPartial(t *testing.T) {
	// A partial frame with an empty Name must NOT lock out the name field —
	// a subsequent tool_call frame should backfill function.name on its delta.
	em := newCursorToolCallEmitter("req-latename")

	d1, ok1 := em.OnEvent(cursorEvent{Type: "tool_call_partial", CallID: "call_abc", Name: ""})
	if !ok1 {
		t.Fatal("first partial should emit")
	}
	// Partial delta carries an empty name on .function.name.
	if d1["tool_calls"].([]map[string]any)[0]["function"].(map[string]any)["name"] != "" {
		t.Errorf("partial delta should carry empty name, got %v",
			d1["tool_calls"].([]map[string]any)[0]["function"])
	}

	d2, ok2 := em.OnEvent(cursorEvent{Type: "tool_call", CallID: "call_abc", Name: "write_file", Arguments: `{"path":"`})
	if !ok2 {
		t.Fatal("tool_call after empty partial should emit")
	}
	tc2 := d2["tool_calls"].([]map[string]any)[0]
	if tc2["index"] != 0 {
		t.Errorf("expected index=0, got %v", tc2["index"])
	}
	fn2 := tc2["function"].(map[string]any)
	if fn2["name"] != "write_file" {
		t.Errorf("tool_call delta should backfill name=write_file, got %v", fn2["name"])
	}
	if fn2["arguments"] != `{"path":"` {
		t.Errorf("tool_call delta should carry args, got %v", fn2["arguments"])
	}

	// Next tool_call frame must NOT repeat the name (it's already sent).
	d3, _ := em.OnEvent(cursorEvent{Type: "tool_call", CallID: "call_abc", Name: "write_file", Arguments: `/tmp/x"}`})
	tc3 := d3["tool_calls"].([]map[string]any)[0]
	fn3 := tc3["function"].(map[string]any)
	if _, hasName := fn3["name"]; hasName {
		t.Errorf("subsequent tool_call delta should NOT repeat function.name, got %v", fn3)
	}
}

// --- Usage frame decoding tests (C2) ---

func TestExtractCursorEndStreamUsage(t *testing.T) {
	cases := []struct {
		name  string
		input map[string]any
		want  *cursorEvent
	}{
		{
			name: "openai-shape usage",
			input: map[string]any{
				"usage": map[string]any{"prompt_tokens": float64(100), "completion_tokens": float64(200)},
			},
			want: &cursorEvent{Type: "usage", PromptTokens: 100, CompletionTokens: 200},
		},
		{
			name: "anthropic-shape usage",
			input: map[string]any{
				"usage": map[string]any{"input_tokens": float64(7), "output_tokens": float64(11)},
			},
			want: &cursorEvent{Type: "usage", PromptTokens: 7, CompletionTokens: 11},
		},
		{
			name: "nested metadata.usage",
			input: map[string]any{
				"metadata": map[string]any{
					"usage": map[string]any{"prompt_tokens": float64(5), "completion_tokens": float64(6)},
				},
			},
			want: &cursorEvent{Type: "usage", PromptTokens: 5, CompletionTokens: 6},
		},
		{name: "no usage", input: map[string]any{}, want: nil},
		{
			name:  "zero usage ignored",
			input: map[string]any{"usage": map[string]any{"prompt_tokens": float64(0), "completion_tokens": float64(0)}},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCursorEndStreamUsage(tc.input)
			if tc.want == nil {
				if got != nil {
					t.Errorf("expected nil, got %+v", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected %+v, got nil", *tc.want)
			}
			if got.PromptTokens != tc.want.PromptTokens || got.CompletionTokens != tc.want.CompletionTokens {
				t.Errorf("got %+v, want %+v", *got, *tc.want)
			}
		})
	}
}

func TestParseCursorChatPayload_BuildsTurns(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "Be helpful."},
			map[string]any{"role": "user", "content": "What's the weather?"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "get_weather", "arguments": `{"city":"SF"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "62F, sunny"},
			map[string]any{"role": "user", "content": "Thanks. How about LA?"},
		},
	}
	raw, _ := json.Marshal(body)
	payload := parseCursorChatPayload(cliproxyexecutor.Request{Payload: raw})

	if len(payload.Turns) != 3 {
		t.Fatalf("expected 3 turns (2 user + 1 assistant), got %d", len(payload.Turns))
	}
	if payload.Turns[0].Role != "user" || payload.Turns[0].Text != "What's the weather?" {
		t.Errorf("turn 0: %+v", payload.Turns[0])
	}
	if payload.Turns[1].Role != "assistant" || len(payload.Turns[1].Calls) != 1 {
		t.Errorf("turn 1: %+v", payload.Turns[1])
	}
	if payload.Turns[2].Role != "user" || payload.Turns[2].Text != "Thanks. How about LA?" {
		t.Errorf("turn 2: %+v", payload.Turns[2])
	}
	// SystemPrompt now includes the provider framing prelude (directive +
	// inventory + primer) prepended in front of the OpenAI system content.
	// Verify the OpenAI-provided portion is preserved as a substring.
	if !strings.Contains(payload.SystemPrompt, "Be helpful.") {
		t.Errorf("system prompt missing OpenAI portion %q: got %q", "Be helpful.", payload.SystemPrompt)
	}

	// lookupToolResult should resolve call_1 to the matching tool message.
	if payload.lookupToolResult == nil {
		t.Fatal("lookupToolResult is nil")
	}
	r, ok := payload.lookupToolResult("call_1")
	if !ok || r.Content != "62F, sunny" {
		t.Errorf("lookupToolResult(call_1): ok=%v content=%q", ok, r.Content)
	}
}

func TestResolveToolSpec_AliasesNativeNames(t *testing.T) {
	clientTools := []cursorToolDefinition{
		{Name: "Read", Parameters: `{}`},
		{Name: "write", Parameters: `{}`},
		{Name: "bash", Parameters: `{}`},
		{Name: "shell", Parameters: `{}`},
		{Name: "edit", Parameters: `{}`},
		{Name: "glob", Parameters: `{}`},
	}
	cases := []struct {
		emitted string
		want    string
	}{
		{"read_file", "Read"},
		{"openFile", "Read"},
		{"write_file", "write"},
		{"createFile", "write"},
		{"run_terminal_cmd", "bash"}, // first candidate "bash" wins
		{"terminal", "bash"},
		{"editFile", "edit"},
		{"search_replace", "edit"},
		{"replaceFile", "edit"},
		{"file_search", "glob"},
		{"findFiles", "glob"},
	}
	for _, c := range cases {
		spec := resolveToolSpec(c.emitted, clientTools, nil)
		if spec == nil {
			t.Errorf("%q: expected to resolve via alias to %q, got nil", c.emitted, c.want)
			continue
		}
		if spec.Name != c.want {
			t.Errorf("%q: expected %q, got %q", c.emitted, c.want, spec.Name)
		}
	}
}

func TestResolveToolSpec_UnknownReturnsNil(t *testing.T) {
	clientTools := []cursorToolDefinition{{Name: "Read"}}
	if spec := resolveToolSpec("totally_made_up_tool", clientTools, nil); spec != nil {
		t.Errorf("expected nil for unknown tool, got %+v", spec)
	}
}

// --- Review comment 4: argument normalizer covers nested + defaults ---

func TestNormalizeToolArguments_NestedArgumentsExpansion(t *testing.T) {
	tool := &cursorToolDefinition{Name: "Read", Parameters: `{"type":"object","properties":{"file_path":{"type":"string"}}}`}
	// Composer wraps the real args under a "arguments" key.
	raw := map[string]any{"arguments": map[string]any{"file_path": "x.txt"}}
	got := normalizeToolArguments(raw, tool)
	if got["file_path"] != "x.txt" {
		t.Errorf("expected nested arguments flattened to file_path=x.txt, got %+v", got)
	}
}

func TestNormalizeToolArguments_NestedArgumentsStringJSON(t *testing.T) {
	tool := &cursorToolDefinition{Name: "Read", Parameters: `{"type":"object","properties":{"file_path":{"type":"string"}}}`}
	raw := map[string]any{"args": `{"file_path":"y.txt"}`}
	got := normalizeToolArguments(raw, tool)
	if got["file_path"] != "y.txt" {
		t.Errorf("expected nested args (string JSON) to flatten; got %+v", got)
	}
}

func TestNormalizeToolArguments_ShellDefaults(t *testing.T) {
	// description required by schema, model omitted it. command also required.
	tool := &cursorToolDefinition{
		Name:       "bash",
		Parameters: `{"type":"object","properties":{"command":{"type":"string"},"description":{"type":"string"}},"required":["command","description"]}`,
	}
	got := normalizeToolArguments(map[string]any{"cmd": "ls -la"}, tool)
	if got["command"] != "ls -la" {
		t.Errorf("expected command remapped from cmd, got %+v", got)
	}
	if desc, _ := got["description"].(string); desc == "" || desc == "Runs shell command" {
		t.Errorf("expected description derived from command, got %q", desc)
	}
}

func TestNormalizeToolArguments_GlobDefault(t *testing.T) {
	tool := &cursorToolDefinition{
		Name:       "glob",
		Parameters: `{"type":"object","properties":{"pattern":{"type":"string"}},"required":["pattern"]}`,
	}
	// Composer omitted pattern entirely.
	got := normalizeToolArguments(map[string]any{}, tool)
	if got["pattern"] != "*" {
		t.Errorf("expected pattern defaulted to '*', got %+v", got)
	}
}

func TestNormalizeToolArguments_PriorityResolution(t *testing.T) {
	// Two aliases compete for the same schema target. The higher-priority
	// COMMON-table rule wins. We deliberately use a tool name with no
	// tool-specific override branch in cursor_composer_aliases.go so only
	// commonArgumentAliases applies — otherwise both keys collapse to the
	// tool-specific priority (95) and the test races on Go's non-
	// deterministic map iteration order (~20% flake rate). With only the
	// common table active, filePath=90 and targeting=45 stay distinct.
	tool := &cursorToolDefinition{
		Name:       "custom_unaliased_tool",
		Parameters: `{"type":"object","properties":{"path":{"type":"string"}}}`,
	}
	raw := map[string]any{"targeting": "from_targeting", "filePath": "from_filePath"}
	got := normalizeToolArguments(raw, tool)
	if got["path"] != "from_filePath" {
		t.Errorf("higher-priority alias filePath should win over targeting, got %+v", got)
	}
}

// --- Review comment 5: JSON-schema output constraint includes schema text ---

func TestCursorOutputConstraints_JSONSchemaIncludesSchema(t *testing.T) {
	body := map[string]any{
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "MyShape",
				"schema": map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "number"}}},
			},
		},
	}
	got := cursorOutputConstraints(body)
	if len(got) != 1 {
		t.Fatalf("expected 1 constraint, got %d: %v", len(got), got)
	}
	want := `Return JSON that matches this schema: {"properties":{"x":{"type":"number"}},"type":"object"}`
	if got[0] != want {
		t.Errorf("got %q\nwant %q", got[0], want)
	}
}

func TestCursorOutputConstraints_JSONObject(t *testing.T) {
	body := map[string]any{"response_format": map[string]any{"type": "json_object"}}
	got := cursorOutputConstraints(body)
	if len(got) != 1 || got[0] != "Return a single valid JSON object and no surrounding prose." {
		t.Errorf("got %v", got)
	}
}

func TestCursorOutputConstraints_StopAndMaxTokens(t *testing.T) {
	body := map[string]any{
		"max_tokens": float64(500),
		"stop":       []any{"END", "DONE"},
	}
	got := cursorOutputConstraints(body)
	if len(got) != 2 {
		t.Fatalf("expected 2 constraints, got %d: %v", len(got), got)
	}
	if got[0] != "Keep the answer within about 500 output tokens." {
		t.Errorf("max_tokens line: %q", got[0])
	}
	if got[1] != "Stop before any of these sequences: END, DONE" {
		t.Errorf("stop array line: %q", got[1])
	}
}

func TestCursorOutputConstraints_StopString(t *testing.T) {
	body := map[string]any{"stop": "STOPHERE"}
	got := cursorOutputConstraints(body)
	if len(got) != 1 || got[0] != "Do not include text after this stop sequence: STOPHERE" {
		t.Errorf("got %v", got)
	}
}

// --- Review comment 6: whitespace-padded markers in text sanitizer ---

func TestCursorTextSanitizer_WhitespacePaddedFinalMarker(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"unpadded ASCII", []string{"a<|final|>b"}, "ab"},
		{"space-padded ASCII", []string{"a< |final|>b"}, "ab"},
		{"fully-padded ASCII", []string{"a< | final | >b"}, "ab"},
		{"full-width", []string{"a<｜final｜>b"}, "ab"},
		{"fully-padded full-width", []string{"a< ｜ final ｜ >b"}, "ab"},
		{"chunk-split padded", []string{"a< | fi", "nal | >b"}, "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &cursorTextSanitizer{}
			var got strings.Builder
			for _, c := range tc.chunks {
				got.WriteString(s.Push(c))
			}
			got.WriteString(s.Flush())
			if got.String() != tc.want {
				t.Errorf("got %q, want %q", got.String(), tc.want)
			}
		})
	}
}

// --- Tool-result history truncation (size mitigation for the flat path) ---

func TestTruncateCursorToolResultForHistory(t *testing.T) {
	// Under both caps → unchanged.
	short := "line1\nline2\nline3"
	if got := truncateCursorToolResultForHistory(short); got != short {
		t.Errorf("short content should be unchanged, got %q", got)
	}
	// Empty → unchanged.
	if got := truncateCursorToolResultForHistory(""); got != "" {
		t.Errorf("empty should stay empty, got %q", got)
	}
	// Over the rune cap → truncated + marker.
	big := strings.Repeat("x", cursorMaxHistoryToolResultRunes+5000)
	got := truncateCursorToolResultForHistory(big)
	if len([]rune(got)) >= len([]rune(big)) {
		t.Errorf("over-cap content should shrink: in=%d out=%d", len([]rune(big)), len([]rune(got)))
	}
	if !strings.Contains(got, "cursor proxy truncated tool result") {
		t.Errorf("expected truncation marker, got tail %q", got[max(0, len(got)-120):])
	}
	// Over the line cap → truncated even when rune count is small.
	manyLines := strings.Repeat("a\n", cursorMaxHistoryToolResultLines+100)
	gotLines := truncateCursorToolResultForHistory(manyLines)
	if !strings.Contains(gotLines, "cursor proxy truncated tool result") {
		t.Errorf("line-cap should trigger truncation")
	}
	keptLineCount := strings.Count(gotLines, "\n")
	if keptLineCount > cursorMaxHistoryToolResultLines+3 { // +marker lines
		t.Errorf("kept too many lines: %d", keptLineCount)
	}
}
