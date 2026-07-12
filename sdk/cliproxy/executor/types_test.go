package executor

import (
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestResponseFormatOrSourceUsesExplicitResponseFormat(t *testing.T) {
	opts := Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatClaude,
	}

	if got := ResponseFormatOrSource(opts); got != sdktranslator.FormatClaude {
		t.Fatalf("ResponseFormatOrSource() = %q, want %q", got, sdktranslator.FormatClaude)
	}
}

func TestResponseFormatOrSourceFallsBackToSourceFormat(t *testing.T) {
	opts := Options{SourceFormat: sdktranslator.FormatGemini}

	if got := ResponseFormatOrSource(opts); got != sdktranslator.FormatGemini {
		t.Fatalf("ResponseFormatOrSource() = %q, want %q", got, sdktranslator.FormatGemini)
	}
}

func TestIsSSECommentOnlyPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "empty", payload: "", want: true},
		{name: "keepalive", payload: ": keepalive\n\n", want: true},
		{name: "multiple comments", payload: ": one\r\n:two\n\n", want: true},
		{name: "data", payload: "data: {}\n\n", want: false},
		{name: "event", payload: "event: ping\n\n", want: false},
		{name: "leading space is data", payload: " : comment\n", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSSECommentOnlyPayload([]byte(tc.payload)); got != tc.want {
				t.Fatalf("IsSSECommentOnlyPayload(%q) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}
