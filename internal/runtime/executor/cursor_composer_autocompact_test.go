package executor

import (
	"bytes"
	"testing"

	"github.com/tidwall/gjson"
)

// sseData extracts the JSON payload from an `event: x\ndata: {json}\n\n` SSE event.
func sseData(chunk []byte) []byte {
	i := bytes.Index(chunk, []byte("data: "))
	if i < 0 {
		return nil
	}
	start := i + len("data: ")
	rel := bytes.IndexByte(chunk[start:], '\n')
	if rel < 0 {
		return chunk[start:]
	}
	return chunk[start : start+rel]
}

// TestComposerSetMessageStartInputTokens pins the auto-compact fix: CC reads message.usage.input_tokens to decide
// when to auto-compact, and the openai->claude translator hard-codes it to 0. We patch message_start so a composer
// session gets a real, growing input_tokens and CC compacts it like any Claude model.
func TestComposerSetMessageStartInputTokens(t *testing.T) {
	// The exact framing the openai->claude translator emits (common.AppendSSEEventBytes with trailingNewlines=2),
	// with the hard-coded usage:{input_tokens:0,output_tokens:0}.
	msgStart := []byte(`{"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"composer-2.5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`)
	chunk := append([]byte("event: message_start\ndata: "), msgStart...)
	chunk = append(chunk, '\n', '\n')

	patched := composerSetMessageStartInputTokens(chunk, 152345)
	got := sseData(patched)
	if v := gjson.GetBytes(got, "message.usage.input_tokens").Int(); v != 152345 {
		t.Fatalf("input_tokens = %d, want 152345 (the prompt estimate CC's auto-compact reads)", v)
	}
	if v := gjson.GetBytes(got, "message.usage.output_tokens").Int(); v != 0 {
		t.Fatalf("output_tokens = %d, want 0 (must be untouched)", v)
	}
	// The SSE framing must be preserved (event line + trailing newlines), only the payload mutated.
	if !bytes.HasPrefix(patched, []byte("event: message_start\ndata: ")) || !bytes.HasSuffix(patched, []byte("\n\n")) {
		t.Fatalf("SSE framing was corrupted: %q", patched)
	}

	// No estimate -> exact no-op (never report a fake 0/garbage).
	if !bytes.Equal(composerSetMessageStartInputTokens(chunk, 0), chunk) {
		t.Fatal("inputTokens<=0 must be an exact no-op")
	}
	// A non-message_start chunk -> untouched (we must not rewrite content/delta events).
	delta := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n")
	if !bytes.Equal(composerSetMessageStartInputTokens(delta, 999), delta) {
		t.Fatal("a non-message_start chunk must be returned unchanged")
	}
	// A message_delta (which legitimately carries usage) must NOT be treated as message_start.
	mdelta := []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":0,\"output_tokens\":5}}\n\n")
	if !bytes.Equal(composerSetMessageStartInputTokens(mdelta, 999), mdelta) {
		t.Fatal("message_delta must be returned unchanged")
	}
}
