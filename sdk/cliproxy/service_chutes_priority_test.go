package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestService_applyChutesModelPriority_PreservesChutesAliases(t *testing.T) {
	t.Parallel()

	mgr := coreauth.NewManager(nil, nil, nil)
	chutesAuth := &coreauth.Auth{
		ID:       "chutes-auth-1",
		Provider: "chutes",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"priority": "fallback",
		},
	}
	if _, err := mgr.Register(context.Background(), chutesAuth); err != nil {
		t.Fatalf("mgr.Register(chutes): %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(chutesAuth.ID, "chutes", []*registry.ModelInfo{
		{ID: "gpt-4o"},
		{ID: "chutes-gpt-4o"},
		{ID: "only-chutes-model"},
		{ID: "chutes-only-chutes-model"},
	})

	// Add a different provider advertising gpt-4o, so fallback priority would normally hide it.
	reg.RegisterClient("openai-auth-1", "openai", []*registry.ModelInfo{
		{ID: "gpt-4o"},
	})

	t.Cleanup(func() {
		reg.UnregisterClient(chutesAuth.ID)
		reg.UnregisterClient("openai-auth-1")
	})

	s := &Service{coreManager: mgr}
	s.applyChutesModelPriority()

	after := reg.GetModelsForClient(chutesAuth.ID)
	if len(after) == 0 {
		t.Fatalf("expected models for chutes auth after filtering")
	}
	has := func(id string) bool {
		for _, m := range after {
			if m != nil && m.ID == id {
				return true
			}
		}
		return false
	}

	// Non-prefixed model that exists in another provider should be hidden in fallback mode.
	if has("gpt-4o") {
		t.Fatalf("expected gpt-4o to be filtered out for chutes fallback priority")
	}
	// Prefixed alias should always be preserved.
	if !has("chutes-gpt-4o") {
		t.Fatalf("expected chutes-gpt-4o alias to be preserved")
	}
	// Models unique to chutes should remain (both base and alias).
	if !has("only-chutes-model") {
		t.Fatalf("expected only-chutes-model to be preserved")
	}
	if !has("chutes-only-chutes-model") {
		t.Fatalf("expected chutes-only-chutes-model alias to be preserved")
	}
}

func TestService_applyManagedProviderModelPriorities_PreservesManagedAliases(t *testing.T) {
	t.Parallel()

	mgr := coreauth.NewManager(nil, nil, nil)
	managedAuth := &coreauth.Auth{
		ID:       "managed-auth-1",
		Provider: "example-provider",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"priority": "fallback",
		},
	}
	if _, err := mgr.Register(context.Background(), managedAuth); err != nil {
		t.Fatalf("mgr.Register(example-provider): %v", err)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(managedAuth.ID, "example-provider", []*registry.ModelInfo{
		{ID: "glm-5.2"},
		{ID: "example-glm-5.2"},
		{ID: "only-managed-model"},
		{ID: "example-only-managed-model"},
	})
	reg.RegisterClient("other-auth-1", "other", []*registry.ModelInfo{
		{ID: "glm-5.2"},
	})

	t.Cleanup(func() {
		reg.UnregisterClient(managedAuth.ID)
		reg.UnregisterClient("other-auth-1")
	})

	s := &Service{
		coreManager: mgr,
		cfg: &config.Config{
			SDKConfig: config.SDKConfig{
				ManagedProviders: []config.ManagedProviderConfig{{
					Name:   "example-provider",
					Prefix: "example-",
				}},
			},
		},
	}
	s.applyManagedProviderModelPriorities()

	after := reg.GetModelsForClient(managedAuth.ID)
	if len(after) == 0 {
		t.Fatalf("expected models for managed auth after filtering")
	}
	has := func(id string) bool {
		for _, m := range after {
			if m != nil && m.ID == id {
				return true
			}
		}
		return false
	}

	if has("glm-5.2") {
		t.Fatalf("expected glm-5.2 to be filtered out for managed fallback priority")
	}
	if !has("example-glm-5.2") {
		t.Fatalf("expected example-glm-5.2 alias to be preserved")
	}
	if !has("only-managed-model") {
		t.Fatalf("expected only-managed-model to be preserved")
	}
	if !has("example-only-managed-model") {
		t.Fatalf("expected example-only-managed-model alias to be preserved")
	}
}
