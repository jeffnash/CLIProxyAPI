package registry

import "testing"

func TestWithXAIBuiltinsIncludesVideoPreviewModel(t *testing.T) {
	models := WithXAIBuiltins(nil)

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15PreviewModelID {
			return
		}
	}

	t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15PreviewModelID)
}

func TestWithXAIBuiltinsIncludesComposerReasoningAliases(t *testing.T) {
	models := WithXAIBuiltins(nil)
	found := map[string]*ModelInfo{}
	for _, model := range models {
		if model == nil {
			continue
		}
		found[model.ID] = model
	}

	for _, level := range []string{"low", "medium", "high"} {
		id := "grok-composer-2.5-fast-" + level
		model := found[id]
		if model == nil {
			t.Fatalf("expected xAI builtin model %s", id)
		}
		if model.Type != "xai" || model.OwnedBy != "xai" {
			t.Fatalf("%s ownership/type = %s/%s, want xai/xai", id, model.OwnedBy, model.Type)
		}
		if model.Thinking == nil {
			t.Fatalf("%s missing thinking support", id)
		}
		if got := len(model.Thinking.Levels); got != 3 {
			t.Fatalf("%s thinking levels len = %d, want 3", id, got)
		}
	}
}

func TestAntigravityWebSearchModelForRequiresRequestedModelCapability(t *testing.T) {
	registryRef := GetGlobalRegistry()
	registryRef.RegisterClient("test-antigravity-websearch-route", "antigravity", []*ModelInfo{
		{ID: "gemini-route-test"},
		{ID: "gemini-web-search-test", SupportsWebSearch: true},
	})
	registryRef.RegisterClient("test-gemini-websearch-route", "gemini", []*ModelInfo{
		{ID: "gemini-cross-provider-route"},
		{ID: "gemini-cross-provider-search", SupportsWebSearch: true},
	})
	t.Cleanup(func() {
		registryRef.UnregisterClient("test-antigravity-websearch-route")
		registryRef.UnregisterClient("test-gemini-websearch-route")
	})

	if got := AntigravityWebSearchModelFor("gemini-route-test"); got != "" {
		t.Fatalf("route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-route-test(high)"); got != "" {
		t.Fatalf("suffix route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-web-search-test"); got != "gemini-web-search-test" {
		t.Fatalf("AntigravityWebSearchModelFor capable model = %q, want itself", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-cross-provider-route"); got != "" {
		t.Fatalf("cross-provider model should not get Antigravity web search model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("unknown-model"); got != "" {
		t.Fatalf("unknown model should not get Antigravity web search model, got %q", got)
	}
}
