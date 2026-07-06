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
