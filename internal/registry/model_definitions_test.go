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

func TestGetCursorModelsIncludesGrok45FastAndNonFast(t *testing.T) {
	// Bare SDK ids (grok-4.5, grok-4.5-fast, …) plus GenerateCursorAliases force-routing forms
	// (cursor-grok-4.5, …) — same pattern as copilot-/codex-. Grok 4.3 has no Cursor variant.
	models := GetCursorModels()
	found := map[string]*ModelInfo{}
	for _, model := range models {
		if model == nil {
			continue
		}
		found[model.ID] = model
	}

	for _, id := range []string{
		// bare (SDK ids)
		"grok-4.5", "grok-4.5-fast", "grok-4.5-xhigh", "grok-4.5-fast-medium",
		// explicit force aliases (disambiguate from xAI grok-4.5)
		"cursor-grok-4.5", "cursor-grok-4.5-fast", "cursor-grok-4.5-xhigh", "cursor-grok-4.5-fast-medium",
		// composer force aliases too
		"cursor-composer-2.5", "cursor-composer-2.5-fast",
	} {
		m := found[id]
		if m == nil {
			t.Fatalf("expected cursor model %s", id)
		}
		if m.Type != "cursor" || m.OwnedBy != "cursor" {
			t.Fatalf("%s ownership/type = %s/%s, want cursor/cursor", id, m.OwnedBy, m.Type)
		}
	}
	if found["composer-2.5"] == nil || found["composer-2.5-fast"] == nil {
		t.Fatal("expected composer-2.5 and composer-2.5-fast to remain in cursor builtins")
	}
}

func TestGenerateCursorAliases(t *testing.T) {
	base := []*ModelInfo{{ID: "grok-4.5", DisplayName: "Grok 4.5", Description: "base", Type: "cursor", OwnedBy: "cursor"}}
	out := GenerateCursorAliases(base)
	if len(out) != 2 {
		t.Fatalf("len=%d, want 2 (base + alias)", len(out))
	}
	if out[0].ID != "grok-4.5" {
		t.Fatalf("base id = %q", out[0].ID)
	}
	if out[1].ID != CursorModelPrefix+"grok-4.5" {
		t.Fatalf("alias id = %q, want %q", out[1].ID, CursorModelPrefix+"grok-4.5")
	}
	if CursorModelPrefix != "cursor-" {
		t.Fatalf("CursorModelPrefix = %q, want cursor-", CursorModelPrefix)
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
