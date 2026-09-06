package test

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestScratchThinkingProbe(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("scratch-probe", "test", []*registry.ModelInfo{
		{ID: "antigravity-budget-model", Object: "model", Type: "antigravity", Thinking: &registry.ThinkingSupport{Min: 128, Max: 20000, ZeroAllowed: true, DynamicAllowed: true}},
	})
	defer reg.UnregisterClient("scratch-probe")
	in := []byte(`{"model":"antigravity-budget-model(medium)","contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	body := sdktranslator.TranslateRequest(
		sdktranslator.FromString("gemini"),
		sdktranslator.FromString("antigravity"),
		"antigravity-budget-model(medium)",
		in,
		true,
	)
	t.Logf("TRANSLATED: %s", string(body))
	out, err := thinking.ApplyThinking(body, "antigravity-budget-model(medium)", "gemini", "antigravity", "antigravity")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Logf("APPLIED: %s", string(out))
	t.Logf("IT: %s", gjson.GetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts").Raw)
}
