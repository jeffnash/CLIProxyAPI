package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestApplyModeAutoLegacyClaudeOmitsInvalidThinkingControls(t *testing.T) {
	applier := NewApplier()
	body := []byte(`{"max_tokens":4096,"thinking":{"type":"enabled"},"output_config":{"effort":"high"}}`)
	modelInfo := &registry.ModelInfo{
		ID: "legacy-claude",
		Thinking: &registry.ThinkingSupport{
			Min:            1024,
			Max:            128000,
			DynamicAllowed: true,
		},
	}

	out, err := applier.Apply(body, thinking.ThinkingConfig{Mode: thinking.ModeAuto, Budget: -1}, modelInfo)
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking = %s, want omitted for legacy auto mode; body=%s", gjson.GetBytes(out, "thinking").Raw, out)
	}
	if gjson.GetBytes(out, "output_config").Exists() {
		t.Fatalf("output_config = %s, want omitted after effort cleanup; body=%s", gjson.GetBytes(out, "output_config").Raw, out)
	}
}
