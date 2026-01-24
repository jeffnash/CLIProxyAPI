package gemini

import (
	"net/http/httptest"
	"testing"
)

func TestWriteGeminiCLISSEChunk_SkipsEmptyAndDone(t *testing.T) {
	rec := httptest.NewRecorder()
	if wrote := writeGeminiCLISSEChunk(rec, []byte("")); wrote {
		t.Fatalf("expected wrote=false")
	}
	if wrote := writeGeminiCLISSEChunk(rec, []byte("   ")); wrote {
		t.Fatalf("expected wrote=false")
	}
	if wrote := writeGeminiCLISSEChunk(rec, []byte("[DONE]")); wrote {
		t.Fatalf("expected wrote=false")
	}
	if wrote := writeGeminiCLISSEChunk(rec, []byte("data: [DONE]")); wrote {
		t.Fatalf("expected wrote=false")
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("unexpected output: %q", got)
	}
}

func TestWriteGeminiCLISSEChunk_WritesWrappedOrRawJSON(t *testing.T) {
	t.Run("raw json", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if wrote := writeGeminiCLISSEChunk(rec, []byte(`{"ok":true}`)); !wrote {
			t.Fatalf("expected wrote=true")
		}
		if got := rec.Body.String(); got != "data: {\"ok\":true}\n\n" {
			t.Fatalf("unexpected output: %q", got)
		}
	})

	t.Run("data-prefixed json", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if wrote := writeGeminiCLISSEChunk(rec, []byte(`data: {"ok":true}`)); !wrote {
			t.Fatalf("expected wrote=true")
		}
		if got := rec.Body.String(); got != "data: {\"ok\":true}\n\n" {
			t.Fatalf("unexpected output: %q", got)
		}
	})
}

func TestWriteGeminiCLISSEChunk_MultilineSplitsIntoDataLines(t *testing.T) {
	rec := httptest.NewRecorder()
	if wrote := writeGeminiCLISSEChunk(rec, []byte("data: {\"a\":1}\n{\"b\":2}\n")); !wrote {
		t.Fatalf("expected wrote=true")
	}
	if got := rec.Body.String(); got != "data: {\"a\":1}\ndata: {\"b\":2}\n\n" {
		t.Fatalf("unexpected output: %q", got)
	}
}

