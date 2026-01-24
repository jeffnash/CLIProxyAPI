package gemini

import (
	"net/http/httptest"
	"testing"
)

func TestWriteGeminiSSEData_SkipsEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	if wrote := writeGeminiSSEData(rec, []byte("")); wrote {
		t.Fatalf("expected wrote=false")
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestWriteGeminiSSEData_WritesNonEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	if wrote := writeGeminiSSEData(rec, []byte(`{"ok":true}`)); !wrote {
		t.Fatalf("expected wrote=true")
	}
	if got := rec.Body.String(); got != "data: {\"ok\":true}\n\n" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestWriteGeminiSSEData_MultilineDataIsSplitIntoDataLines(t *testing.T) {
	rec := httptest.NewRecorder()
	if wrote := writeGeminiSSEData(rec, []byte("{\"a\":1}\n{\"b\":2}\n")); !wrote {
		t.Fatalf("expected wrote=true")
	}
	if got := rec.Body.String(); got != "data: {\"a\":1}\ndata: {\"b\":2}\n\n" {
		t.Fatalf("unexpected output: %q", got)
	}
}
