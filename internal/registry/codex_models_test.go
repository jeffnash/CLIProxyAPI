package registry

import (
	"strings"
	"testing"
)

func TestGenerateCodexAliases(t *testing.T) {
	baseModels := []*ModelInfo{
		{ID: "gpt-5", DisplayName: "GPT-5", Description: "Test model"},
		{ID: "gpt-5.2-xhigh", DisplayName: "GPT-5.2 XHigh", Description: "Reasoning model"},
		{ID: "gpt-5.3-codex", DisplayName: "GPT-5.3 Codex", Description: "Codex model"},
	}

	result := GenerateCodexAliases(baseModels)

	if len(result) != len(baseModels)*2 {
		t.Errorf("expected %d models, got %d", len(baseModels)*2, len(result))
	}

	aliasIDs := make(map[string]bool)
	for _, m := range result {
		aliasIDs[m.ID] = true
	}

	expectedIDs := []string{
		"gpt-5",
		"codex-gpt-5",
		"gpt-5.2-xhigh",
		"codex-gpt-5.2-xhigh",
		"gpt-5.3-codex",
		"codex-gpt-5.3-codex",
	}

	for _, expected := range expectedIDs {
		if !aliasIDs[expected] {
			t.Errorf("expected alias %q to exist in result", expected)
		}
	}
}

func TestGenerateCodexAliases_DisplayNameAndDescription(t *testing.T) {
	baseModels := []*ModelInfo{
		{ID: "gpt-5.2-xhigh", DisplayName: "GPT-5.2 XHigh", Description: "Base description"},
	}

	result := GenerateCodexAliases(baseModels)

	var alias *ModelInfo
	for _, m := range result {
		if m.ID == "codex-gpt-5.2-xhigh" {
			alias = m
			break
		}
	}

	if alias == nil {
		t.Fatal("codex-gpt-5.2-xhigh alias not found")
	}

	if !strings.Contains(alias.DisplayName, "(Codex)") {
		t.Errorf("expected DisplayName to contain '(Codex)', got %s", alias.DisplayName)
	}

	if !strings.Contains(alias.Description, "routing") && !strings.Contains(alias.Description, "alias") {
		t.Errorf("expected Description to mention routing or alias, got %s", alias.Description)
	}
}

// TestGenerateCodexAliases_AllGPTModelFamilies verifies that codex- prefixed
// aliases are produced for all known GPT model families from GetOpenAIModels().
func TestGenerateCodexAliases_AllGPTModelFamilies(t *testing.T) {
	models := GetOpenAIModels()
	result := GenerateCodexAliases(models)

	aliasIDs := make(map[string]bool, len(result))
	for _, m := range result {
		aliasIDs[m.ID] = true
	}

	// Every base model must have both the bare ID and the codex- prefixed alias.
	for _, base := range models {
		if !aliasIDs[base.ID] {
			t.Errorf("base model %q missing from result", base.ID)
		}
		codexAlias := CodexModelPrefix + base.ID
		if !aliasIDs[codexAlias] {
			t.Errorf("codex alias %q missing for base model %q", codexAlias, base.ID)
		}
	}

	// Sanity: result count must be exactly 2x base.
	if len(result) != len(models)*2 {
		t.Errorf("expected %d models, got %d", len(models)*2, len(result))
	}
}

// TestGenerateCodexAliases_SpecificModelFamilies checks that important model
// families all get codex- aliases, including effort-level variants and codex
// sub-families across gpt-5, gpt-5.1, gpt-5.2, and gpt-5.3.
func TestGenerateCodexAliases_SpecificModelFamilies(t *testing.T) {
	// Exhaustive list of model IDs we expect to exist as codex- aliases.
	// These mirror the IDs in model_definitions_static_data.go GetOpenAIModels().
	expectedAliases := []string{
		// gpt-5 base + effort + codex sub-family
		"codex-gpt-5",
		"codex-gpt-5-minimal",
		"codex-gpt-5-low",
		"codex-gpt-5-medium",
		"codex-gpt-5-high",
		"codex-gpt-5-codex",
		"codex-gpt-5-codex-low",
		"codex-gpt-5-codex-medium",
		"codex-gpt-5-codex-high",
		"codex-gpt-5-codex-mini",
		"codex-gpt-5-codex-mini-medium",
		"codex-gpt-5-codex-mini-high",

		// gpt-5.1 base + effort + codex sub-family + codex-max sub-family
		"codex-gpt-5.1",
		"codex-gpt-5.1-none",
		"codex-gpt-5.1-low",
		"codex-gpt-5.1-medium",
		"codex-gpt-5.1-high",
		"codex-gpt-5.1-codex",
		"codex-gpt-5.1-codex-low",
		"codex-gpt-5.1-codex-medium",
		"codex-gpt-5.1-codex-high",
		"codex-gpt-5.1-codex-mini",
		"codex-gpt-5.1-codex-mini-medium",
		"codex-gpt-5.1-codex-mini-high",
		"codex-gpt-5.1-codex-max",
		"codex-gpt-5.1-codex-max-low",
		"codex-gpt-5.1-codex-max-medium",
		"codex-gpt-5.1-codex-max-high",
		"codex-gpt-5.1-codex-max-xhigh",

		// gpt-5.2 base + effort + codex sub-family
		"codex-gpt-5.2",
		"codex-gpt-5.2-none",
		"codex-gpt-5.2-low",
		"codex-gpt-5.2-medium",
		"codex-gpt-5.2-high",
		"codex-gpt-5.2-xhigh",
		"codex-gpt-5.2-codex",
		"codex-gpt-5.2-codex-low",
		"codex-gpt-5.2-codex-medium",
		"codex-gpt-5.2-codex-high",
		"codex-gpt-5.2-codex-xhigh",

		// gpt-5.3-codex family
		"codex-gpt-5.3-codex",
		"codex-gpt-5.3-codex-low",
		"codex-gpt-5.3-codex-medium",
		"codex-gpt-5.3-codex-high",
		"codex-gpt-5.3-codex-xhigh",
	}

	models := GetOpenAIModels()
	result := GenerateCodexAliases(models)

	aliasIDs := make(map[string]bool, len(result))
	for _, m := range result {
		aliasIDs[m.ID] = true
	}

	for _, expected := range expectedAliases {
		if !aliasIDs[expected] {
			t.Errorf("expected codex alias %q to exist", expected)
		}
	}
}

// TestGenerateCodexAliases_PreservesOriginalModelUnchanged ensures that the
// original (non-aliased) ModelInfo is not mutated by alias generation.
func TestGenerateCodexAliases_PreservesOriginalModelUnchanged(t *testing.T) {
	original := &ModelInfo{
		ID:                  "gpt-5.2-xhigh",
		DisplayName:         "GPT-5.2 XHigh",
		Description:         "Original desc",
		ContextLength:       400000,
		MaxCompletionTokens: 128000,
	}

	result := GenerateCodexAliases([]*ModelInfo{original})

	// Original must not be mutated.
	if original.ID != "gpt-5.2-xhigh" {
		t.Fatalf("original model ID was mutated to %q", original.ID)
	}
	if original.DisplayName != "GPT-5.2 XHigh" {
		t.Fatalf("original DisplayName was mutated to %q", original.DisplayName)
	}

	// Alias must have its own independent metadata.
	var alias *ModelInfo
	for _, m := range result {
		if m.ID == "codex-gpt-5.2-xhigh" {
			alias = m
			break
		}
	}
	if alias == nil {
		t.Fatal("alias not found")
	}
	if alias.ContextLength != original.ContextLength {
		t.Errorf("alias ContextLength=%d, want %d", alias.ContextLength, original.ContextLength)
	}
	if alias.MaxCompletionTokens != original.MaxCompletionTokens {
		t.Errorf("alias MaxCompletionTokens=%d, want %d", alias.MaxCompletionTokens, original.MaxCompletionTokens)
	}
}

// TestGenerateCodexAliases_EmptyInput returns empty slice for nil/empty input.
func TestGenerateCodexAliases_EmptyInput(t *testing.T) {
	result := GenerateCodexAliases(nil)
	if len(result) != 0 {
		t.Errorf("expected 0 models for nil input, got %d", len(result))
	}

	result = GenerateCodexAliases([]*ModelInfo{})
	if len(result) != 0 {
		t.Errorf("expected 0 models for empty input, got %d", len(result))
	}
}

// TestGenerateCodexAliases_NoDuplicateIDs ensures no duplicate model IDs in output.
func TestGenerateCodexAliases_NoDuplicateIDs(t *testing.T) {
	models := GetOpenAIModels()
	result := GenerateCodexAliases(models)

	seen := make(map[string]int, len(result))
	for _, m := range result {
		seen[m.ID]++
	}

	for id, count := range seen {
		if count > 1 {
			t.Errorf("duplicate model ID %q appeared %d times", id, count)
		}
	}
}

// TestCodexModelPrefix_Value ensures the constant matches the expected prefix.
func TestCodexModelPrefix_Value(t *testing.T) {
	if CodexModelPrefix != "codex-" {
		t.Errorf("CodexModelPrefix=%q, want %q", CodexModelPrefix, "codex-")
	}
}
