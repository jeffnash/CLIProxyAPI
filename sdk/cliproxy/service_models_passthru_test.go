package cliproxy

import (
	"context"
	"testing"

	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// TestRegisterModelsForAuthPassthru guards the restored passthru branch: a
// passthru route auth must register its routing name as a model ID.
func TestRegisterModelsForAuthPassthru(t *testing.T) {
	authID := "passthru-restore-test"
	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{}}
	auth := &coreauth.Auth{
		ID:       authID,
		Provider: "claude",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":               "k",
			"passthru":              "true",
			"passthru_model":        "muse-spark-1.3-contributor",
			"passthru_routing_name": "muse-spark-1.3-contributor",
			"context_window":        "1048576",
			"max_tokens":            "128000",
		},
	}

	service.registerModelsForAuth(context.Background(), auth)

	got := modelRegistry.GetModelsForClient(authID)
	if len(got) != 1 {
		t.Fatalf("registered models = %d, want 1", len(got))
	}
	if got[0].ID != "muse-spark-1.3-contributor" {
		t.Fatalf("model ID = %q, want muse-spark-1.3-contributor", got[0].ID)
	}
	if got[0].ContextLength != 1048576 || got[0].MaxCompletionTokens != 128000 {
		t.Fatalf("context/max = %d/%d, want 1048576/128000", got[0].ContextLength, got[0].MaxCompletionTokens)
	}
	if !got[0].UserDefined {
		t.Fatal("passthru model must be marked UserDefined")
	}
}

// TestRegisterModelsForAuthRestoredProviders guards the provider cases
// restored after the upstream merge dropped them from registration.
func TestRegisterModelsForAuthRestoredProviders(t *testing.T) {
	tests := []struct {
		provider string
		wantIDs  map[string]struct{}
	}{
		{"qwen", codexModelIDSet(internalregistry.GetQwenModels())},
		{"kiro", codexModelIDSet(internalregistry.GetKiroModels())},
		{"cursor", codexModelIDSet(internalregistry.GetCursorModels())},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if len(tt.wantIDs) == 0 {
				t.Skipf("no static %s models", tt.provider)
			}
			authID := "restored-provider-" + tt.provider
			modelRegistry := internalregistry.GetGlobalRegistry()
			modelRegistry.UnregisterClient(authID)
			t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

			service := &Service{cfg: &config.Config{}}
			auth := &coreauth.Auth{
				ID:         authID,
				Provider:   tt.provider,
				Status:     coreauth.StatusActive,
				Attributes: map[string]string{"api_key": "k"},
			}
			service.registerModelsForAuth(context.Background(), auth)

			got := codexModelIDSet(modelRegistry.GetModelsForClient(authID))
			if len(got) != len(tt.wantIDs) {
				t.Fatalf("registered %s model IDs = %#v, want %#v", tt.provider, got, tt.wantIDs)
			}
		})
	}
}

// TestRegisterModelsForAuthGeminiVirtualPrimary ensures virtual-primary
// gemini auths stay out of the registry.
func TestRegisterModelsForAuthGeminiVirtualPrimary(t *testing.T) {
	authID := "gemini-virtual-primary-test"
	modelRegistry := internalregistry.GetGlobalRegistry()
	modelRegistry.UnregisterClient(authID)
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })

	service := &Service{cfg: &config.Config{}}
	service.registerModelsForAuth(context.Background(), &coreauth.Auth{
		ID:         authID,
		Provider:   "gemini",
		Status:     coreauth.StatusActive,
		Attributes: map[string]string{"gemini_virtual_primary": "true"},
	})

	if got := modelRegistry.GetModelsForClient(authID); len(got) != 0 {
		t.Fatalf("virtual primary registered %d models, want 0", len(got))
	}
}
