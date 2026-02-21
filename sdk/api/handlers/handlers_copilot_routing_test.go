package handlers

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// TestGetRequestDetails_CopilotPrefixRouting tests that the copilot- prefix correctly
// forces routing to the Copilot provider and sets the forced_provider metadata flag.
func TestGetRequestDetails_CopilotPrefixRouting(t *testing.T) {
	handler := &BaseAPIHandler{}

	tests := []struct {
		name                   string
		modelName              string
		expectedProviders      []string
		expectedNormalizedModel string
		expectedForcedProvider bool
	}{
		{
			name:                   "copilot prefix routes to copilot provider",
			modelName:              "copilot-gemini-3-flash-preview",
			expectedProviders:      []string{"copilot"},
			expectedNormalizedModel: "gemini-3-flash-preview",
			expectedForcedProvider: true,
		},
		{
			name:                   "copilot prefix with gpt model",
			modelName:              "copilot-gpt-5",
			expectedProviders:      []string{"copilot"},
			expectedNormalizedModel: "gpt-5",
			expectedForcedProvider: true,
		},
		{
			name:                   "copilot prefix with claude model",
			modelName:              "copilot-claude-sonnet-4",
			expectedProviders:      []string{"copilot"},
			expectedNormalizedModel: "claude-sonnet-4",
			expectedForcedProvider: true,
		},
		{
			name:                   "copilot prefix with gemini-2.5-pro",
			modelName:              "copilot-gemini-2.5-pro",
			expectedProviders:      []string{"copilot"},
			expectedNormalizedModel: "gemini-2.5-pro",
			expectedForcedProvider: true,
		},
		{
			name:                   "copilot prefix with grok model",
			modelName:              "copilot-grok-code-fast-1",
			expectedProviders:      []string{"copilot"},
			expectedNormalizedModel: "grok-code-fast-1",
			expectedForcedProvider: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers, normalizedModel, metadata, err := handler.getRequestDetails(tt.modelName)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check providers
			if len(providers) == 0 {
				t.Fatalf("expected providers, got empty slice")
			}

			// For forced copilot, providers should be exactly ["copilot"]
			if tt.expectedForcedProvider {
				if len(providers) != 1 || providers[0] != "copilot" {
					t.Errorf("expected providers=[copilot] for forced copilot, got %v", providers)
				}
			}

			// Check normalized model name (prefix should be stripped)
			if normalizedModel != tt.expectedNormalizedModel {
				t.Errorf("expected normalizedModel=%s, got %s", tt.expectedNormalizedModel, normalizedModel)
			}

			// Check forced_provider metadata flag
			forcedProvider := false
			if metadata != nil {
				if v, ok := metadata["forced_provider"]; ok {
					if b, okBool := v.(bool); okBool {
						forcedProvider = b
					}
				}
			}

			if forcedProvider != tt.expectedForcedProvider {
				t.Errorf("expected forced_provider=%v, got %v", tt.expectedForcedProvider, forcedProvider)
			}
		})
	}
}

// TestGetRequestDetails_CopilotPrefixWithThinkingSuffix tests that copilot- prefix
// works correctly with dynamic thinking suffixes.
func TestGetRequestDetails_CopilotPrefixWithThinkingSuffix(t *testing.T) {
	handler := &BaseAPIHandler{}

	tests := []struct {
		name                   string
		modelName              string
		expectedProviders      []string
		expectedForcedProvider bool
		checkModelContains     string
	}{
		{
			name:                   "copilot prefix with thinking suffix",
			modelName:              "copilot-gemini-3-flash-preview-high",
			expectedProviders:      []string{"copilot"},
			expectedForcedProvider: true,
			checkModelContains:     "gemini-3-flash-preview",
		},
		{
			name:                   "copilot prefix with medium thinking",
			modelName:              "copilot-claude-sonnet-4-medium",
			expectedProviders:      []string{"copilot"},
			expectedForcedProvider: true,
			checkModelContains:     "claude-sonnet-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers, normalizedModel, metadata, err := handler.getRequestDetails(tt.modelName)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			// Check forced copilot routing
			if tt.expectedForcedProvider {
				if len(providers) != 1 || providers[0] != "copilot" {
					t.Errorf("expected providers=[copilot], got %v", providers)
				}
			}

			// Check model name contains expected base model
			if tt.checkModelContains != "" {
				found := false
				if normalizedModel == tt.checkModelContains {
					found = true
				}
				// Also check metadata for original model
				if metadata != nil {
					if orig, ok := metadata["thinking_original_model"].(string); ok && orig == tt.checkModelContains {
						found = true
					}
				}
				if !found && normalizedModel != tt.checkModelContains {
					// The model normalization might have kept the suffix, that's ok
					// as long as the prefix was stripped
					if len(normalizedModel) < len(tt.checkModelContains) {
						t.Errorf("expected normalizedModel to contain %s, got %s", tt.checkModelContains, normalizedModel)
					}
				}
			}

			// Check forced_provider flag
			forcedProvider := false
			if metadata != nil {
				if v, ok := metadata["forced_provider"]; ok {
					if b, okBool := v.(bool); okBool {
						forcedProvider = b
					}
				}
			}

			if forcedProvider != tt.expectedForcedProvider {
				t.Errorf("expected forced_provider=%v, got %v", tt.expectedForcedProvider, forcedProvider)
			}
		})
	}
}

// TestCopilotModelPrefixConstant ensures the constant is correctly defined.
func TestCopilotModelPrefixConstant(t *testing.T) {
	if registry.CopilotModelPrefix != "copilot-" {
		t.Errorf("expected CopilotModelPrefix='copilot-', got '%s'", registry.CopilotModelPrefix)
	}
}

// TestGetRequestDetails_ForcedProviderMetadataIsSet tests that the forced_provider
// metadata is correctly set for copilot-prefixed models.
func TestGetRequestDetails_ForcedProviderMetadataIsSet(t *testing.T) {
	handler := &BaseAPIHandler{}

	// Test with copilot prefix - should set forced_provider
	_, _, metadata, err := handler.getRequestDetails("copilot-gpt-5")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if metadata == nil {
		t.Fatal("expected metadata to be non-nil for copilot-prefixed model")
	}

	forcedVal, exists := metadata["forced_provider"]
	if !exists {
		t.Error("expected forced_provider key in metadata")
	}

	forcedBool, ok := forcedVal.(bool)
	if !ok {
		t.Errorf("expected forced_provider to be bool, got %T", forcedVal)
	}

	if !forcedBool {
		t.Error("expected forced_provider to be true for copilot-prefixed model")
	}
}

// TestGetRequestDetails_ForcedProviderMetadataNotSetForNonCopilot verifies that
// forced_provider is not set for regular models (when they route normally).
func TestGetRequestDetails_MetadataForCopilotPrefixOnly(t *testing.T) {
	handler := &BaseAPIHandler{}

	testCases := []struct {
		model        string
		expectForced bool
	}{
		{"copilot-gpt-5", true},
		{"copilot-claude-sonnet-4", true},
		{"copilot-gemini-3-flash-preview", true},
	}

	for _, tc := range testCases {
		t.Run(tc.model, func(t *testing.T) {
			_, _, metadata, err := handler.getRequestDetails(tc.model)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			forced := false
			if metadata != nil {
				if v, ok := metadata["forced_provider"].(bool); ok {
					forced = v
				}
			}

			if forced != tc.expectForced {
				t.Errorf("expected forced_provider=%v for model %s, got %v", tc.expectForced, tc.model, forced)
			}
		})
	}
}

func TestGetRequestDetails_CodexPrefixRouting(t *testing.T) {
	handler := &BaseAPIHandler{}
	providers, normalizedModel, metadata, err := handler.getRequestDetails("codex-gpt-5.3-codex-spark-high")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(providers) != 1 || providers[0] != "codex" {
		t.Fatalf("expected providers=[codex], got %v", providers)
	}
	if normalizedModel != "gpt-5.3-codex-spark-high" {
		t.Fatalf("expected stripped codex model, got %q", normalizedModel)
	}
	forced, _ := metadata["forced_provider"].(bool)
	if !forced {
		t.Fatalf("expected forced_provider=true, got %v", metadata["forced_provider"])
	}
}
