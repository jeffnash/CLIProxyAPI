package executor

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

// TestMergeEssentialCopilotModels tests that essential models are correctly
// merged into the dynamic model list.
func TestMergeEssentialCopilotModels(t *testing.T) {
	now := time.Now().Unix()

	tests := []struct {
		name           string
		existingModels []*registry.ModelInfo
		expectAdded    []string
		expectNotAdded []string
	}{
		{
			name:           "adds gemini-3-flash-preview when missing",
			existingModels: []*registry.ModelInfo{},
			expectAdded:    []string{"gemini-3-flash-preview"},
		},
		{
			name: "does not duplicate existing gemini-3-flash-preview",
			existingModels: []*registry.ModelInfo{
				{ID: "gemini-3-flash-preview", OwnedBy: "copilot"},
			},
			expectNotAdded: []string{"gemini-3-flash-preview"},
		},
		{
			name: "case insensitive duplicate check",
			existingModels: []*registry.ModelInfo{
				{ID: "GEMINI-3-FLASH-PREVIEW", OwnedBy: "copilot"},
			},
			expectNotAdded: []string{"gemini-3-flash-preview"},
		},
		{
			name: "adds essential model alongside other models",
			existingModels: []*registry.ModelInfo{
				{ID: "gpt-5", OwnedBy: "copilot"},
				{ID: "claude-sonnet-4", OwnedBy: "copilot"},
				{ID: "gemini-2.5-pro", OwnedBy: "copilot"},
			},
			expectAdded: []string{"gemini-3-flash-preview"},
		},
		{
			name: "preserves existing models when adding essential",
			existingModels: []*registry.ModelInfo{
				{ID: "gpt-5", OwnedBy: "copilot"},
				{ID: "gpt-4o", OwnedBy: "copilot"},
			},
			expectAdded: []string{"gemini-3-flash-preview", "gpt-5", "gpt-4o"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := mergeEssentialCopilotModels(tt.existingModels, now)

			// Build set of result model IDs
			resultIDs := make(map[string]bool)
			for _, m := range result {
				resultIDs[strings.ToLower(m.ID)] = true
			}

			// Check expected additions are present
			for _, expected := range tt.expectAdded {
				if !resultIDs[strings.ToLower(expected)] {
					t.Errorf("expected model %s to be in result, but it wasn't", expected)
				}
			}

			// Check that models that should NOT be added (duplicates) don't appear twice
			for _, notExpected := range tt.expectNotAdded {
				count := 0
				for _, m := range result {
					if strings.EqualFold(m.ID, notExpected) {
						count++
					}
				}
				if count > 1 {
					t.Errorf("model %s appears %d times (should be 1)", notExpected, count)
				}
			}
		})
	}
}

// TestMergeEssentialCopilotModels_ModelAttributes tests that added essential
// models have correct attributes.
func TestMergeEssentialCopilotModels_ModelAttributes(t *testing.T) {
	now := time.Now().Unix()
	result := mergeEssentialCopilotModels([]*registry.ModelInfo{}, now)

	// Find gemini-3-flash-preview in result
	var geminiFlash *registry.ModelInfo
	for _, m := range result {
		if m.ID == "gemini-3-flash-preview" {
			geminiFlash = m
			break
		}
	}

	if geminiFlash == nil {
		t.Fatal("gemini-3-flash-preview not found in result")
	}

	// Check attributes
	if geminiFlash.OwnedBy != "copilot" {
		t.Errorf("expected OwnedBy=copilot, got %s", geminiFlash.OwnedBy)
	}
	if geminiFlash.Type != "copilot" {
		t.Errorf("expected Type=copilot, got %s", geminiFlash.Type)
	}
	if geminiFlash.Object != "model" {
		t.Errorf("expected Object=model, got %s", geminiFlash.Object)
	}
	if geminiFlash.Created != now {
		t.Errorf("expected Created=%d, got %d", now, geminiFlash.Created)
	}
	if geminiFlash.ContextLength <= 0 {
		t.Errorf("expected positive ContextLength, got %d", geminiFlash.ContextLength)
	}
	if geminiFlash.MaxCompletionTokens <= 0 {
		t.Errorf("expected positive MaxCompletionTokens, got %d", geminiFlash.MaxCompletionTokens)
	}
	if len(geminiFlash.SupportedParameters) == 0 {
		t.Error("expected SupportedParameters to be populated")
	}

	// Check that tools parameter is included
	hasTools := false
	for _, p := range geminiFlash.SupportedParameters {
		if p == "tools" {
			hasTools = true
			break
		}
	}
	if !hasTools {
		t.Error("expected SupportedParameters to include 'tools'")
	}
}

// TestEssentialCopilotModels_ContainsRequiredModels tests that the essential
// models list contains all required models.
func TestEssentialCopilotModels_ContainsRequiredModels(t *testing.T) {
	requiredModels := []string{
		"gemini-3-flash-preview",
	}

	essentialIDs := make(map[string]bool)
	for _, m := range essentialCopilotModels {
		essentialIDs[strings.ToLower(m.ID)] = true
	}

	for _, required := range requiredModels {
		if !essentialIDs[strings.ToLower(required)] {
			t.Errorf("essential models list missing required model: %s", required)
		}
	}
}

// TestGenerateCopilotAliases tests that copilot- prefixed aliases are correctly
// generated for all models.
func TestGenerateCopilotAliases(t *testing.T) {
	baseModels := []*registry.ModelInfo{
		{ID: "gpt-5", DisplayName: "GPT-5", Description: "Test model"},
		{ID: "gemini-3-flash-preview", DisplayName: "Gemini 3 Flash", Description: "Another test"},
	}

	result := registry.GenerateCopilotAliases(baseModels)

	// Should have 2x the models (original + aliased)
	if len(result) != len(baseModels)*2 {
		t.Errorf("expected %d models, got %d", len(baseModels)*2, len(result))
	}

	// Check that aliases exist
	aliasIDs := make(map[string]bool)
	for _, m := range result {
		aliasIDs[m.ID] = true
	}

	expectedAliases := []string{
		"gpt-5",
		"copilot-gpt-5",
		"gemini-3-flash-preview",
		"copilot-gemini-3-flash-preview",
	}

	for _, expected := range expectedAliases {
		if !aliasIDs[expected] {
			t.Errorf("expected alias %s to exist", expected)
		}
	}
}

// TestGenerateCopilotAliases_DisplayNameAndDescription tests that aliases have
// correct display name and description modifications.
func TestGenerateCopilotAliases_DisplayNameAndDescription(t *testing.T) {
	baseModels := []*registry.ModelInfo{
		{ID: "test-model", DisplayName: "Test Model", Description: "Base description"},
	}

	result := registry.GenerateCopilotAliases(baseModels)

	// Find the aliased model
	var alias *registry.ModelInfo
	for _, m := range result {
		if m.ID == "copilot-test-model" {
			alias = m
			break
		}
	}

	if alias == nil {
		t.Fatal("copilot-test-model alias not found")
	}

	// Check display name has (Copilot) suffix
	if !strings.Contains(alias.DisplayName, "(Copilot)") {
		t.Errorf("expected DisplayName to contain '(Copilot)', got %s", alias.DisplayName)
	}

	// Check description mentions routing
	if !strings.Contains(alias.Description, "routing") && !strings.Contains(alias.Description, "alias") {
		t.Errorf("expected Description to mention routing or alias, got %s", alias.Description)
	}
}
