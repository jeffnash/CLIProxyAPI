package registry

import (
	"testing"
	"time"
)

func TestModelRegistry_ConvertModelToMap_IncludesContextWindow(t *testing.T) {
	reg := GetGlobalRegistry()

	// Use a unique client ID to avoid conflicts with other tests
	clientID := "test-client-ctx-window"

	// Register a model with context window and max completion tokens
	models := []*ModelInfo{{
		ID:                  "test-model-ctx",
		Object:              "model",
		Created:             time.Now().Unix(),
		OwnedBy:             "test-provider",
		Type:                "openai",
		DisplayName:         "Test Model",
		ContextLength:       256000,
		MaxCompletionTokens: 64000,
	}}

	reg.RegisterClient(clientID, "openai", models)
	defer reg.UnregisterClient(clientID)

	// Get available models in OpenAI format
	availableModels := reg.GetAvailableModels("openai")

	var model map[string]any
	for _, m := range availableModels {
		if m["id"] == "test-model-ctx" {
			model = m
			break
		}
	}

	if model == nil {
		t.Fatal("expected to find test-model-ctx in available models")
	}

	// Verify context_length is included
	if v, ok := model["context_length"]; !ok {
		t.Error("expected context_length in model response")
	} else if v != 256000 {
		t.Errorf("expected context_length 256000, got %v", v)
	}

	// Verify context_window alias is included
	if v, ok := model["context_window"]; !ok {
		t.Error("expected context_window alias in model response")
	} else if v != 256000 {
		t.Errorf("expected context_window 256000, got %v", v)
	}

	// Verify max_completion_tokens is included
	if v, ok := model["max_completion_tokens"]; !ok {
		t.Error("expected max_completion_tokens in model response")
	} else if v != 64000 {
		t.Errorf("expected max_completion_tokens 64000, got %v", v)
	}

	// Verify max_tokens alias is included
	if v, ok := model["max_tokens"]; !ok {
		t.Error("expected max_tokens alias in model response")
	} else if v != 64000 {
		t.Errorf("expected max_tokens 64000, got %v", v)
	}
}

func TestModelRegistry_ConvertModelToMap_OmitsZeroContextWindow(t *testing.T) {
	reg := GetGlobalRegistry()

	clientID := "test-client-no-ctx"

	// Register a model WITHOUT context window
	models := []*ModelInfo{{
		ID:      "test-model-no-ctx",
		Object:  "model",
		OwnedBy: "test-provider",
		// ContextLength and MaxCompletionTokens are zero
	}}

	reg.RegisterClient(clientID, "openai", models)
	defer reg.UnregisterClient(clientID)

	// Get available models in OpenAI format
	availableModels := reg.GetAvailableModels("openai")

	var model map[string]any
	for _, m := range availableModels {
		if m["id"] == "test-model-no-ctx" {
			model = m
			break
		}
	}

	if model == nil {
		t.Fatal("expected to find test-model-no-ctx")
	}

	// Verify context_length is NOT included when zero
	if _, ok := model["context_length"]; ok {
		t.Error("expected context_length to be omitted when zero")
	}

	// Verify context_window is NOT included when zero
	if _, ok := model["context_window"]; ok {
		t.Error("expected context_window to be omitted when zero")
	}
}

func TestModelRegistry_PassthruModelRegistration(t *testing.T) {
	reg := GetGlobalRegistry()

	clientID := "passthru-auth-id-test"

	// Simulate passthru model registration (as done in service.go)
	models := []*ModelInfo{{
		ID:                  "zai-test-model",
		Object:              "model",
		Created:             time.Now().Unix(),
		OwnedBy:             "passthru",
		Type:                "claude",
		DisplayName:         "zai-test-model",
		ContextLength:       128000,
		MaxCompletionTokens: 32000,
		UserDefined:         true,
	}}

	reg.RegisterClient(clientID, "claude", models)
	defer reg.UnregisterClient(clientID)

	// Verify model is registered
	providers := reg.GetModelProviders("zai-test-model")
	if len(providers) == 0 {
		t.Fatal("expected passthru model to be registered")
	}
	if providers[0] != "claude" {
		t.Errorf("expected provider 'claude', got %q", providers[0])
	}

	// Verify model appears in available models
	availableModels := reg.GetAvailableModels("openai")
	var found bool
	for _, m := range availableModels {
		if m["id"] == "zai-test-model" {
			found = true
			if m["owned_by"] != "passthru" {
				t.Errorf("expected owned_by 'passthru', got %v", m["owned_by"])
			}
			if m["context_window"] != 128000 {
				t.Errorf("expected context_window 128000, got %v", m["context_window"])
			}
			break
		}
	}
	if !found {
		t.Error("expected zai-test-model in available models")
	}
}

func TestModelRegistry_PassthruModelWithThinking(t *testing.T) {
	reg := GetGlobalRegistry()

	clientID := "passthru-thinking-test"

	// Register a passthru model with UserDefined=true and Thinking set
	models := []*ModelInfo{{
		ID:                  "zai-glm-test",
		Object:              "model",
		Created:             time.Now().Unix(),
		OwnedBy:             "passthru",
		Type:                "claude",
		DisplayName:         "zai-glm-test",
		ContextLength:       204800,
		MaxCompletionTokens: 64000,
		UserDefined:         true,
		Thinking:            &ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: true},
	}}

	reg.RegisterClient(clientID, "claude", models)
	defer reg.UnregisterClient(clientID)

	// Verify model is registered and has thinking support
	info := reg.GetModelInfo("zai-glm-test", "claude")
	if info == nil {
		t.Fatal("expected model info to be non-nil")
	}
	if !info.UserDefined {
		t.Error("expected UserDefined to be true")
	}
	if info.Thinking == nil {
		t.Fatal("expected Thinking to be non-nil")
	}
	if info.Thinking.Min != 1024 {
		t.Errorf("expected Thinking.Min 1024, got %d", info.Thinking.Min)
	}
	if info.Thinking.Max != 128000 {
		t.Errorf("expected Thinking.Max 128000, got %d", info.Thinking.Max)
	}
	if !info.Thinking.ZeroAllowed {
		t.Error("expected Thinking.ZeroAllowed to be true")
	}
	if !info.Thinking.DynamicAllowed {
		t.Error("expected Thinking.DynamicAllowed to be true")
	}
}
