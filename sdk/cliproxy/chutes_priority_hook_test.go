package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v6/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestApplyChutesModelPriority_RetainsPrefixedAliases(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient("other-client", "other", []*registry.ModelInfo{{ID: "m1"}})
	reg.RegisterClient("chutes-client", "chutes", []*registry.ModelInfo{{ID: "m1"}, {ID: registry.ChutesModelPrefix + "m1"}})

	store := sdkAuth.GetTokenStore()
	mgr := coreauth.NewManager(store, &coreauth.RoundRobinSelector{}, nil)
	_, err := mgr.Register(context.Background(), &coreauth.Auth{ID: "chutes-client", Provider: "chutes", Attributes: map[string]string{"priority": "fallback"}})
	if err != nil {
		t.Fatalf("failed to register auth: %v", err)
	}

	svc := &Service{coreManager: mgr}
	svc.applyChutesModelPriority()

	models := reg.GetModelsForClient("chutes-client")
	if len(models) != 1 {
		t.Fatalf("expected 1 model after filtering, got %d", len(models))
	}
	if models[0].ID != registry.ChutesModelPrefix+"m1" {
		t.Fatalf("expected retained model %q, got %q", registry.ChutesModelPrefix+"m1", models[0].ID)
	}
}
