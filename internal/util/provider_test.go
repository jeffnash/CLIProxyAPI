package util

import (
	"strings"
	"testing"
)

func TestSummarizeSensitiveBodyRedactsJSONSecrets(t *testing.T) {
	body := []byte(`{"access_token":"abcdef1234567890","nested":{"apiKey":"sk-secret-value"},"message":"bad"}`)
	got := SummarizeSensitiveBody(body, 512)
	if strings.Contains(got, "abcdef1234567890") || strings.Contains(got, "sk-secret-value") {
		t.Fatalf("summary leaked secret: %s", got)
	}
	if !strings.Contains(got, "message") {
		t.Fatalf("summary lost non-sensitive fields: %s", got)
	}
}

func TestSummarizeSensitiveBodyCapsPlainText(t *testing.T) {
	got := SummarizeSensitiveBody([]byte(strings.Repeat("x", 20)), 8)
	if len(got) != 8 {
		t.Fatalf("summary len = %d, want 8: %q", len(got), got)
	}
}
