package cliproxy

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestRefreshFetchedModelsAllAndFiltered(t *testing.T) {
	reg := GlobalModelRegistry()
	authIDs := []string{"refresh-test-qwen", "refresh-test-devin"}
	for _, id := range authIDs {
		reg.UnregisterClient(id)
	}
	t.Cleanup(func() {
		for _, id := range authIDs {
			reg.UnregisterClient(id)
		}
	})

	manager := coreauth.NewManager(nil, nil, nil)
	auths := []*coreauth.Auth{
		{ID: authIDs[0], Provider: "qwen", Status: coreauth.StatusActive},
		{ID: authIDs[1], Provider: "devin", Status: coreauth.StatusActive},
	}
	for _, auth := range auths {
		if _, err := manager.Register(context.Background(), auth); err != nil {
			t.Fatalf("register auth %s: %v", auth.ID, err)
		}
	}
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	ctx := context.Background()

	if got := service.RefreshFetchedModels(ctx, nil); got != 2 {
		t.Fatalf("RefreshFetchedModels(nil) refreshed %d auths, want 2", got)
	}
	if got := service.RefreshFetchedModels(ctx, []string{"QWEN"}); got != 1 {
		t.Fatalf("RefreshFetchedModels([QWEN]) refreshed %d auths, want 1", got)
	}
	if got := service.RefreshFetchedModels(ctx, []string{"no-such-provider"}); got != 0 {
		t.Fatalf("RefreshFetchedModels([unknown]) refreshed %d auths, want 0", got)
	}

	if models := reg.GetModelsForClient(authIDs[0]); len(models) == 0 {
		t.Fatal("expected qwen models registered after refresh, got none")
	}
	if models := reg.GetModelsForClient(authIDs[1]); len(models) == 0 {
		t.Fatal("expected devin models registered after refresh, got none")
	}
}

func TestRefreshFetchedModelsNilService(t *testing.T) {
	var service *Service
	if got := service.RefreshFetchedModels(context.Background(), nil); got != 0 {
		t.Fatalf("nil service refreshed %d auths, want 0", got)
	}
	service = &Service{}
	if got := service.RefreshFetchedModels(context.Background(), nil); got != 0 {
		t.Fatalf("service without manager refreshed %d auths, want 0", got)
	}
}

func TestNormalizeProviderFilter(t *testing.T) {
	if filter := normalizeProviderFilter(nil); len(filter) != 0 {
		t.Fatalf("nil providers produced filter %v, want empty", filter)
	}
	filter := normalizeProviderFilter([]string{" A6 ", "", "a6", "Meta"})
	if len(filter) != 2 || !filter["a6"] || !filter["meta"] {
		t.Fatalf("unexpected filter %v, want [a6 meta]", filter)
	}
}
