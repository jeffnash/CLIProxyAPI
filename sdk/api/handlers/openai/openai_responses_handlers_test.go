package openai

import (
	"net/http/httptest"
	"testing"
)

func TestResponsesSSEWriteState_NoLeadingDelimiterBeforeFirstData(t *testing.T) {
	rec := httptest.NewRecorder()
	st := &responsesSSEWriteState{}

	// Leading empty chunks should be ignored until a non-empty data payload is written.
	st.writeChunk(rec, []byte(""))
	st.writeChunk(rec, []byte("event: response.created"))
	st.writeDone(rec)

	if got := rec.Body.String(); got != "event: response.created\n" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestResponsesSSEWriteState_EventNewlineOnlyAfterNonEmptyData(t *testing.T) {
	rec := httptest.NewRecorder()
	st := &responsesSSEWriteState{}

	st.writeChunk(rec, []byte(`data: {"type":"response.created"}`))
	st.writeChunk(rec, []byte("event: response.output_text.delta"))

	// After writing a non-empty data payload, the next event line should be preceded by exactly one
	// injected newline, resulting in a blank line between the data line and event line.
	want := "data: {\"type\":\"response.created\"}\n\nevent: response.output_text.delta\n"
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
