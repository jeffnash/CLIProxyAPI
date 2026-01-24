package openai

import (
	"net/http/httptest"
	"testing"
)

func TestWriteOpenAISSEData_SkipsEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	if wrote := writeOpenAISSEData(rec, []byte("")); wrote {
		t.Fatalf("expected wrote=false")
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestWriteOpenAISSEData_WritesNonEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	if wrote := writeOpenAISSEData(rec, []byte(`{"ok":true}`)); !wrote {
		t.Fatalf("expected wrote=true")
	}
	if got := rec.Body.String(); got != "data: {\"ok\":true}\n\n" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestWriteOpenAISSEData_MultilineDataIsSplitIntoDataLines(t *testing.T) {
	rec := httptest.NewRecorder()
	if wrote := writeOpenAISSEData(rec, []byte("{\"a\":1}\n{\"b\":2}\n")); !wrote {
		t.Fatalf("expected wrote=true")
	}
	if got := rec.Body.String(); got != "data: {\"a\":1}\ndata: {\"b\":2}\n\n" {
		t.Fatalf("unexpected output: %q", got)
	}
}
