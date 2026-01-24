package gemini

import (
	"bytes"
	"context"
	"fmt"
)

// PassthroughGeminiResponseStream forwards Gemini responses unchanged.
func PassthroughGeminiResponseStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []string {
	// Normalize CRLF SSE lines from bufio.Scanner (it splits on '\n' but may leave a trailing '\r').
	rawJSON = bytes.TrimSuffix(rawJSON, []byte("\r"))

	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	// Skip empty data payloads and [DONE] markers
	if len(rawJSON) == 0 || bytes.Equal(rawJSON, []byte("[DONE]")) {
		return []string{}
	}

	return []string{string(rawJSON)}
}

// PassthroughGeminiResponseNonStream forwards Gemini responses unchanged.
func PassthroughGeminiResponseNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) string {
	return string(rawJSON)
}

func GeminiTokenCount(ctx context.Context, count int64) string {
	return fmt.Sprintf(`{"totalTokens":%d,"promptTokensDetails":[{"modality":"TEXT","tokenCount":%d}]}`, count, count)
}
