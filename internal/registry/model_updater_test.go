package registry

import "testing"

func TestDetectChangedProvidersIncludesQwenIFlowAndCursor(t *testing.T) {
	oldData := &staticModelsJSON{
		Qwen:   []*ModelInfo{{ID: "qwen-old"}},
		IFlow:  []*ModelInfo{{ID: "iflow-old"}},
		Cursor: []*ModelInfo{{ID: "cursor-old"}},
	}
	newData := &staticModelsJSON{
		Qwen:   []*ModelInfo{{ID: "qwen-new"}},
		IFlow:  []*ModelInfo{{ID: "iflow-new"}},
		Cursor: []*ModelInfo{{ID: "cursor-new"}},
	}

	changed := detectChangedProviders(oldData, newData)
	got := make(map[string]bool, len(changed))
	for _, provider := range changed {
		got[provider] = true
	}
	for _, provider := range []string{"qwen", "iflow", "cursor"} {
		if !got[provider] {
			t.Fatalf("changed providers = %v, missing %q", changed, provider)
		}
	}
}

func TestDetectChangedProviders_CodexConfigurationUpdate(t *testing.T) {
	oldData := &staticModelsJSON{
		CodexFree: []*ModelInfo{{ID: "gpt-6-luna"}},
	}
	newData := &staticModelsJSON{
		CodexFree: []*ModelInfo{{ID: "gpt-6-luna", SupportConfigurationUpdate: true}},
	}

	changed := detectChangedProviders(oldData, newData)
	if len(changed) != 1 || changed[0] != "codex" {
		t.Fatalf("configuration_update-only change: got providers %v, want [codex]", changed)
	}
}

func TestDetectChangedProviders_KimiAliases(t *testing.T) {
	oldData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}},
	}
	newData := &staticModelsJSON{
		Kimi: []*ModelInfo{{ID: "kimi-k2"}, {ID: "kimi-k3"}},
	}

	changed := detectChangedProviders(oldData, newData)
	expected := map[string]bool{
		"kimi":     false,
		"kimi-ai":  false,
		"kimi.ai":  false,
		"kimi.com": false,
	}

	for _, p := range changed {
		if _, ok := expected[p]; ok {
			expected[p] = true
		}
	}

	for p, found := range expected {
		if !found {
			t.Errorf("expected changed provider %q to be reported, got %v", p, changed)
		}
	}
}
