package registry

import "testing"

func TestGenerateManagedProviderAliases(t *testing.T) {
	models := GenerateManagedProviderAliases([]*ModelInfo{{ID: "glm-5.2", DisplayName: "GLM 5.2", Description: "base"}}, "example-", "example-provider")
	if !hasManagedProviderModelID(models, "glm-5.2") {
		t.Fatal("missing base model")
	}
	if !hasManagedProviderModelID(models, "example-glm-5.2") {
		t.Fatal("missing explicit alias")
	}
}

func TestGenerateManagedProviderProtocolAliases(t *testing.T) {
	models := GenerateManagedProviderAliases([]*ModelInfo{{ID: "glm-5.2", DisplayName: "GLM 5.2", Description: "base"}}, "example-", "example-provider")
	models = GenerateManagedProviderProtocolAliases(models, "example-", "example-provider", ManagedProviderAnthropicProtocolPrefix, ManagedProviderOpenAIProtocolPrefix)
	for _, id := range []string{"anthropic-example-glm-5.2", "openai-example-glm-5.2"} {
		if !hasManagedProviderModelID(models, id) {
			t.Fatalf("missing protocol alias %q", id)
		}
	}
	if hasManagedProviderModelID(models, "anthropic-glm-5.2") {
		t.Fatal("generated raw protocol alias without provider prefix")
	}
}

func TestGetManagedProviderFallbackModelsIncludesRequestedFallbacksAndAliases(t *testing.T) {
	models := GetManagedProviderFallbackModels("example-provider", "example-", "Example Provider", []string{"glm-5.2", "qwen3.7-max"})
	models = GenerateManagedProviderProtocolAliases(models, "example-", "Example Provider", ManagedProviderAnthropicProtocolPrefix, ManagedProviderOpenAIProtocolPrefix)
	for _, id := range []string{"glm-5.2", "qwen3.7-max"} {
		if !hasManagedProviderModelID(models, id) {
			t.Fatalf("missing fallback model %q", id)
		}
		if !hasManagedProviderModelID(models, "example-"+id) {
			t.Fatalf("missing fallback alias %q", "example-"+id)
		}
		for _, modelID := range []string{id, "example-" + id, "anthropic-example-" + id, "openai-example-" + id} {
			model := findManagedProviderModel(models, modelID)
			if model == nil {
				t.Fatalf("missing fallback model %q", modelID)
			}
			if !model.UserDefined {
				t.Fatalf("fallback model %q must be UserDefined so managed-provider thinking controls pass through", modelID)
			}
		}
	}
}

func hasManagedProviderModelID(models []*ModelInfo, id string) bool {
	return findManagedProviderModel(models, id) != nil
}

func findManagedProviderModel(models []*ModelInfo, id string) *ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}
