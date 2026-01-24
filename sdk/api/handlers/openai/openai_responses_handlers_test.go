package openai

import (
	"net/http/httptest"
	"testing"
)

func TestResponsesSSEWriteState_NoLeadingDelimiterBeforeFirstData(t *testing.T) {
	rec := httptest.NewRecorder()
	st := &responsesSSEWriteState{}

	// Leading empty chunks should be ignored until a non-empty data payload is written.
	// Event-only lines are buffered and suppressed if no data follows.
	st.writeChunk(rec, []byte(""))
	st.writeChunk(rec, []byte("event: response.created"))
	st.writeDone(rec)

	// Event line with no data should be suppressed entirely to prevent
	// downstream SSE decoders from emitting events with data="" which fails JSON parsing.
	if got := rec.Body.String(); got != "" {
		t.Fatalf("expected empty output for event-only block, got: %q", got)
	}
}

func TestResponsesSSEWriteState_EventNewlineOnlyAfterNonEmptyData(t *testing.T) {
	rec := httptest.NewRecorder()
	st := &responsesSSEWriteState{}

	st.writeChunk(rec, []byte(`data: {"type":"response.created"}`))
	st.writeChunk(rec, []byte("event: response.output_text.delta"))
	st.writeChunk(rec, []byte(`data: {"delta":"hi"}`))
	st.writeDone(rec)

	// After writing a non-empty data payload, the next event line should be preceded by exactly one
	// injected newline, resulting in a blank line between the data line and event line.
	// Event is buffered until data arrives.
	want := "data: {\"type\":\"response.created\"}\n\nevent: response.output_text.delta\ndata: {\"delta\":\"hi\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", got, want)
	}
}

func TestResponsesSSEWriteState_MultilineEmitEventChunk(t *testing.T) {
	rec := httptest.NewRecorder()
	st := &responsesSSEWriteState{}

	// Some translators emit a single chunk containing both event and data lines.
	st.writeChunk(rec, []byte("event: response.created\ndata: {\"type\":\"response.created\"}"))
	st.writeChunk(rec, []byte(""))
	st.writeDone(rec)

	got := rec.Body.String()
	// Should be exactly one well-formed block with both event and non-empty data.
	want := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	if got != want {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", got, want)
	}
}

func TestResponsesSSEWriteState_EmptyDataPayloadFiltered(t *testing.T) {
	t.Run("filters data: with empty payload", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}
		st.writeChunk(rec, []byte("data: "))
		st.writeDone(rec)
		if got := rec.Body.String(); got != "" {
			t.Fatalf("expected empty output for empty data payload, got: %q", got)
		}
	})

	t.Run("filters data: with whitespace-only payload", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}
		st.writeChunk(rec, []byte("data:   "))
		st.writeDone(rec)
		if got := rec.Body.String(); got != "" {
			t.Fatalf("expected empty output for whitespace data payload, got: %q", got)
		}
	})

	t.Run("passes through data: with valid payload", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}
		st.writeChunk(rec, []byte(`data: {"ok":true}`))
		st.writeDone(rec)
		want := "data: {\"ok\":true}\n\n"
		if got := rec.Body.String(); got != want {
			t.Fatalf("unexpected output:\n got: %q\nwant: %q", got, want)
		}
	})

	t.Run("filters empty data but passes valid data in sequence", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}
		st.writeChunk(rec, []byte("event: test"))
		st.writeChunk(rec, []byte("data: ")) // empty - should be filtered
		st.writeChunk(rec, []byte(""))       // delimiter - event block ends with no data, event suppressed
		st.writeChunk(rec, []byte(`data: {"valid":true}`))
		st.writeChunk(rec, []byte(""))
		st.writeDone(rec)
		// First event block (event: test + empty data) is suppressed entirely.
		// Second event block has only data, no event line.
		want := "data: {\"valid\":true}\n\n"
		if got := rec.Body.String(); got != want {
			t.Fatalf("unexpected output:\n got: %q\nwant: %q", got, want)
		}
	})
}

func TestResponsesSSEWriteState_WriteDoneGated(t *testing.T) {
	t.Run("does not write delimiter without non-empty data", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}
		st.writeDone(rec)
		if got := rec.Body.String(); got != "" {
			t.Fatalf("unexpected output: %q", got)
		}
	})

	t.Run("writes delimiter after non-empty data", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}
		st.writeChunk(rec, []byte(`data: {"ok":true}`))
		st.writeDone(rec)
		if got := rec.Body.String(); got != "data: {\"ok\":true}\n\n" {
			t.Fatalf("unexpected output: %q", got)
		}
	})

	t.Run("does not add extra delimiter if already at boundary", func(t *testing.T) {
		rec := httptest.NewRecorder()
		st := &responsesSSEWriteState{}
		st.writeChunk(rec, []byte(`data: {"ok":true}`))
		st.writeChunk(rec, []byte("")) // delimiter emitted by upstream/translator
		st.writeDone(rec)
		if got := rec.Body.String(); got != "data: {\"ok\":true}\n\n" {
			t.Fatalf("unexpected output: %q", got)
		}
	})
}

func TestResponsesSSEWriteState_EventOnlyBlockAfterValidEvent(t *testing.T) {
	// Regression test: after a valid event+data block, an event line followed by
	// empty data and delimiter should NOT emit an event-only block (which would
	// cause JSON parse errors downstream).
	rec := httptest.NewRecorder()
	st := &responsesSSEWriteState{}

	// First: valid event with data
	st.writeChunk(rec, []byte("event: response.created"))
	st.writeChunk(rec, []byte(`data: {"type":"response.created"}`))
	st.writeChunk(rec, []byte("")) // delimiter

	// Second: event line with empty data - should be completely suppressed
	st.writeChunk(rec, []byte("event: response.output_text.delta"))
	st.writeChunk(rec, []byte("data: ")) // empty data - filtered
	st.writeChunk(rec, []byte(""))       // delimiter - should be suppressed (no data in this event)

	st.writeDone(rec)

	// Expected: only the first valid event block, second event-only block is completely suppressed
	want := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", got, want)
	}
}

func TestResponsesSSEWriteState_ConsecutiveEventsWithEmptyData(t *testing.T) {
	// Regression test: event line, empty data line, then another event line
	// should NOT produce any output since neither event has data.
	rec := httptest.NewRecorder()
	st := &responsesSSEWriteState{}

	st.writeChunk(rec, []byte("event: first"))
	st.writeChunk(rec, []byte("data: ")) // empty - filtered
	st.writeChunk(rec, []byte("event: second"))
	st.writeChunk(rec, []byte("data: ")) // empty - filtered
	st.writeDone(rec)

	// Both events have no data, so both are suppressed entirely.
	want := ""
	if got := rec.Body.String(); got != want {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", got, want)
	}
}

func TestResponsesSSEWriteState_TrailingNewlineDoesNotDropEvent(t *testing.T) {
	// Regression test: a chunk ending with a single newline should not cause
	// the buffered event line to be dropped. The trailing empty segment from
	// bytes.Split should not be treated as a delimiter.
	rec := httptest.NewRecorder()
	st := &responsesSSEWriteState{}

	// Chunk with trailing newline (common from translators)
	st.writeChunk(rec, []byte("event: response.created\n"))
	st.writeChunk(rec, []byte(`data: {"type":"response.created"}`))
	st.writeDone(rec)

	// Event line should be preserved and emitted with data.
	want := "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("unexpected output:\n got: %q\nwant: %q", got, want)
	}
}
