package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
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

