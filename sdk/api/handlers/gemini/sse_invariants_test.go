package gemini

import (
	"net/http/httptest"
	"strings"
	"testing"
)

type sseEventBlock struct {
	lines []string
}

func parseSSEBlocks(payload string) []sseEventBlock {
	lines := strings.Split(payload, "\n")
	blocks := make([]sseEventBlock, 0)
	cur := make([]string, 0)
	for _, line := range lines {
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

func TestSSEInvariants_GeminiSSEWriter(t *testing.T) {
	rec := httptest.NewRecorder()

	_ = writeGeminiSSEData(rec, []byte(""))
	_ = writeGeminiSSEData(rec, []byte("   "))
	_ = writeGeminiSSEData(rec, []byte(`{"ok":true}`))
	_ = writeGeminiSSEData(rec, []byte("{\"a\":1}\n{\"b\":2}\n"))

	assertNoEmptySSEDataLines(t, rec.Body.String())
}

