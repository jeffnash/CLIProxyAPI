package openai

import (
	"net/http/httptest"
	"strings"
	"testing"
)

type sseEventBlock struct {
	lines []string
}

func parseSSEBlocks(payload string) []sseEventBlock {
	// Our handlers write one line per chunk, always ending with "\n".
	// A blank line (i.e. an empty line between "\n\n") terminates an SSE event block.
	lines := strings.Split(payload, "\n")
	blocks := make([]sseEventBlock, 0)
	cur := make([]string, 0)
	for _, line := range lines {
		// strings.Split keeps a final empty string if payload ends with "\n".
		// Treat each empty line as a delimiter boundary.
		if line == "" {
			if len(cur) > 0 {
				blocks = append(blocks, sseEventBlock{lines: cur})
				cur = make([]string, 0)
			}
			continue
		}
		cur = append(cur, line)
	}
	if len(cur) > 0 {
		blocks = append(blocks, sseEventBlock{lines: cur})
	}
	return blocks
}

func assertNoEmptySSEEvents(t *testing.T, payload string) {
	t.Helper()

	blocks := parseSSEBlocks(payload)
	for i, b := range blocks {
		hasEventLine := false
		hasDataLine := false
		for _, line := range b.lines {
			if strings.HasPrefix(line, "event:") {
				hasEventLine = true
				continue
			}
			if strings.HasPrefix(line, "data:") {
				hasDataLine = true
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "" {
					t.Fatalf("block[%d] contains empty data payload: %v", i, b.lines)
				}
			}
		}

		// Guard against dispatching an event-only block which some downstream clients treat as an
		// empty SSE event and may attempt to JSON-decode "".
		if hasEventLine && !hasDataLine {
			t.Fatalf("block[%d] contains event line(s) but no data line(s): %v", i, b.lines)
		}
	}
}

func assertNoEmptySSEDataLines(t *testing.T, payload string) {
	t.Helper()

	blocks := parseSSEBlocks(payload)
	for i, b := range blocks {
		for _, line := range b.lines {
			if strings.HasPrefix(line, "data:") {
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "" {
					t.Fatalf("block[%d] contains empty data payload: %v", i, b.lines)
				}
			}
		}
	}
}

func TestSSEInvariants_OpenAIResponsesStreamWriter(t *testing.T) {
	t.Run("leading empty chunks are ignored and do not create empty events", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}

		st.writeChunk(rec, []byte(""))
		st.writeChunk(rec, []byte(""))
		st.writeChunk(rec, []byte("event: response.created"))
		st.writeChunk(rec, []byte(`data: {"type":"response.created"}`))
		st.writeChunk(rec, []byte("")) // delimiter
		st.writeDone(rec)

		assertNoEmptySSEEvents(t, rec.Body.String())
	})

	t.Run("does not emit event-only blocks when translator buffers event lines", func(t *testing.T) {
		// Simulate chunks after translation: the translator emits event+data together and only emits
		// delimiters after non-empty data.
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}

		chunks := [][]byte{
			[]byte("event: response.created"),
			[]byte(`data: {"type":"response.created","response":{"id":"resp_123"}}`),
			[]byte(""),
			[]byte("event: response.output_text.delta"),
			[]byte(`data: {"type":"response.output_text.delta","delta":"Hello"}`),
			[]byte(""),
			[]byte("event: response.completed"),
			[]byte(`data: {"type":"response.completed","response":{"id":"resp_123"}}`),
			[]byte(""),
			[]byte("data: [DONE]"),
		}

		for _, c := range chunks {
			st.writeChunk(rec, c)
		}
		st.writeDone(rec)

		assertNoEmptySSEEvents(t, rec.Body.String())
	})

	t.Run("does not emit empty events for multiline emitEvent chunks", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}

		st.writeChunk(rec, []byte("event: response.created\ndata: {\"type\":\"response.created\"}"))
		st.writeChunk(rec, []byte(""))
		st.writeChunk(rec, []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}"))
		st.writeChunk(rec, []byte(""))
		st.writeDone(rec)

		assertNoEmptySSEEvents(t, rec.Body.String())
	})

	t.Run("error event formatting does not introduce empty events", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}

		st.writeChunk(rec, []byte(`data: {"type":"response.created"}`))
		st.writeChunk(rec, []byte(""))
		st.writeChunk(rec, []byte("event: error\ndata: {\"error\":true}"))
		st.writeChunk(rec, []byte(""))

		assertNoEmptySSEEvents(t, rec.Body.String())
	})
}

func TestSSEInvariants_OpenAIChatCompletionsSSEWriter(t *testing.T) {
	rec := httptest.NewRecorder()

	// Simulate a typical stream writer sequence and ensure we never create `data: \n\n`.
	_ = writeOpenAISSEData(rec, []byte(""))
	_ = writeOpenAISSEData(rec, []byte("   "))
	_ = writeOpenAISSEData(rec, []byte(`{"id":"1"}`))
	_ = writeOpenAISSEData(rec, []byte("{\"a\":1}\n{\"b\":2}\n"))

	assertNoEmptySSEDataLines(t, rec.Body.String())
}
