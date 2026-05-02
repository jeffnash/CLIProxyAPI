package executor

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

func clearChutesAliasesForTest(t *testing.T) func() {
	t.Helper()
	chutesAliasMapMu.Lock()
	previous := make(map[string]string, len(chutesAliasMap))
	for key, value := range chutesAliasMap {
		previous[key] = value
	}
	chutesAliasMap = make(map[string]string)
	chutesAliasMapMu.Unlock()
	return func() {
		chutesAliasMapMu.Lock()
		chutesAliasMap = previous
		chutesAliasMapMu.Unlock()
	}
}

func TestResolveChutesModelUsesRegistryUpstreamIDWhenAliasMapMissing(t *testing.T) {
	const (
		clientID   = "test-chutes-upstream-id"
		publicID   = "nvidia/Gemma-4-31B-IT-NVFP4"
		upstreamID = "google/gemma-4-31B-turbo-TEE"
	)

	restoreAliases := clearChutesAliasesForTest(t)
	defer restoreAliases()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(clientID, "chutes", []*registry.ModelInfo{
		{
			ID:         publicID,
			Object:     "model",
			OwnedBy:    "chutes",
			Type:       "chutes",
			UpstreamID: upstreamID,
		},
	})
	defer reg.UnregisterClient(clientID)

	if got := resolveChutesModel(registry.ChutesModelPrefix + publicID); got != upstreamID {
		t.Fatalf("resolveChutesModel() = %q, want %q", got, upstreamID)
	}
}

func TestChutesModelCacheRestoresAliasMap(t *testing.T) {
	const (
		publicID   = "nvidia/Gemma-4-31B-IT-NVFP4"
		upstreamID = "google/gemma-4-31B-turbo-TEE"
	)

	chutesModelCacheMu.Lock()
	previousCache := chutesModelCache
	chutesModelCache = &chutesModelCacheEntry{
		models: []*registry.ModelInfo{
			{
				ID:         publicID,
				Object:     "model",
				OwnedBy:    "chutes",
				Type:       "chutes",
				UpstreamID: upstreamID,
			},
		},
		aliases: map[string]string{
			publicID: upstreamID,
		},
		fetchedAt: time.Now(),
	}
	chutesModelCacheMu.Unlock()
	defer func() {
		chutesModelCacheMu.Lock()
		chutesModelCache = previousCache
		chutesModelCacheMu.Unlock()
	}()

	restoreAliases := clearChutesAliasesForTest(t)
	defer restoreAliases()
	models := NewChutesExecutor(nil).FetchModels(t.Context(), nil, nil)
	if len(models) != 1 {
		t.Fatalf("FetchModels() returned %d models, want 1", len(models))
	}
	if got := resolveChutesModel(publicID); got != upstreamID {
		t.Fatalf("resolveChutesModel() = %q, want %q", got, upstreamID)
	}
}
