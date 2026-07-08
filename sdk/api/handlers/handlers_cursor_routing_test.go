package handlers

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

// TestGetRequestDetails_CursorPrefixRouting locks the cursor- force-prefix path
// (same pattern as copilot-/codex-). Used to disambiguate Cursor Grok 4.5 from xAI
// grok-4.5: client requests "cursor-grok-4.5" → provider cursor, model "grok-4.5".
// Grok 4.3 has no Cursor variant — bare "grok-4.3" only routes via xAI registration.
func TestGetRequestDetails_CursorPrefixRouting(t *testing.T) {
	if registry.CursorModelPrefix != "cursor-" {
		t.Fatalf("CursorModelPrefix=%q, want cursor-", registry.CursorModelPrefix)
	}

	handler := &BaseAPIHandler{}
	tests := []struct {
		name                    string
		modelName               string
		expectedNormalizedModel string
	}{
		{name: "cursor-grok-4.5", modelName: "cursor-grok-4.5", expectedNormalizedModel: "grok-4.5"},
		{name: "cursor-grok-4.5-fast", modelName: "cursor-grok-4.5-fast", expectedNormalizedModel: "grok-4.5-fast"},
		{name: "cursor-grok-4.5-xhigh", modelName: "cursor-grok-4.5-xhigh", expectedNormalizedModel: "grok-4.5-xhigh"},
		{name: "cursor-composer-2.5", modelName: "cursor-composer-2.5", expectedNormalizedModel: "composer-2.5"},
		{name: "cursor-composer-2.5-fast", modelName: "cursor-composer-2.5-fast", expectedNormalizedModel: "composer-2.5-fast"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers, normalizedModel, metadata, err := handler.getRequestDetailsWithOptions(tt.modelName, false)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(providers) != 1 || providers[0] != "cursor" {
				t.Fatalf("providers=%v, want [cursor]", providers)
			}
			if normalizedModel != tt.expectedNormalizedModel {
				t.Fatalf("normalizedModel=%q, want %q", normalizedModel, tt.expectedNormalizedModel)
			}
			forced, _ := metadata["forced_provider"].(bool)
			if !forced {
				t.Fatalf("forced_provider=%v, want true", metadata["forced_provider"])
			}
		})
	}
}
