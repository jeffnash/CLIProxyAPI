package thinking_test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/thinking/provider/claude"
	"github.com/tidwall/gjson"
)

func TestApplyThinkingUnknownSuffixFallsBackToBodyConfig(t *testing.T) {
	body := []byte(`{"max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":2048}}`)

	out, err := thinking.ApplyThinking(body, "claude-sonnet-4-5-20250929(unknown)", "claude", "claude", "claude")
	if err != nil {
		t.Fatalf("ApplyThinking() error = %v", err)
	}

	if got := gjson.GetBytes(out, "thinking.budget_tokens").Int(); got != 2047 {
		t.Fatalf("thinking.budget_tokens = %d, want body config normalized to 2047; body=%s", got, out)
	}
}
