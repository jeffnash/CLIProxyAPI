package executor

import (
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestEvictExpiredManagedProviderModelCache(t *testing.T) {
	managedProviderModelCacheMu.Lock()
	snapshot := managedProviderModelCache
	managedProviderModelCache = make(map[string]*managedProviderModelCacheEntry)
	managedProviderModelCacheMu.Unlock()
	t.Cleanup(func() {
		managedProviderModelCacheMu.Lock()
		managedProviderModelCache = snapshot
		managedProviderModelCacheMu.Unlock()
	})

	fresh := &managedProviderModelCacheEntry{
		models:    []*registry.ModelInfo{{ID: "fresh-model"}},
		fetchedAt: time.Now(),
		ttl:       time.Hour,
	}
	stale := &managedProviderModelCacheEntry{
		models:    []*registry.ModelInfo{{ID: "stale-model"}},
		fetchedAt: time.Now().Add(-time.Hour),
		ttl:       time.Minute,
	}
	managedProviderModelCacheMu.Lock()
	managedProviderModelCache["test-fresh"] = fresh
	managedProviderModelCache["test-stale"] = stale
	managedProviderModelCacheMu.Unlock()

	expired := EvictExpiredManagedProviderModelCache()
	if !slices.Contains(expired, "test-stale") {
		t.Fatalf("expired providers %v, want test-stale", expired)
	}
	if slices.Contains(expired, "test-fresh") {
		t.Fatalf("fresh entry evicted, expired=%v", expired)
	}

	managedProviderModelCacheMu.Lock()
	_, freshKept := managedProviderModelCache["test-fresh"]
	_, staleKept := managedProviderModelCache["test-stale"]
	managedProviderModelCacheMu.Unlock()
	if !freshKept {
		t.Fatal("fresh cache entry was evicted, want kept")
	}
	if staleKept {
		t.Fatal("stale cache entry was kept, want evicted")
	}
}

func TestEvictAllAndExpiredCopilotModelCaches(t *testing.T) {
	sharedModelCacheMu.Lock()
	snapshot := sharedModelCache
	sharedModelCache = make(map[string]*sharedModelCacheEntry)
	sharedModelCacheMu.Unlock()
	t.Cleanup(func() {
		sharedModelCacheMu.Lock()
		sharedModelCache = snapshot
		sharedModelCacheMu.Unlock()
	})

	sharedModelCacheMu.Lock()
	sharedModelCache["auth-fresh"] = &sharedModelCacheEntry{fetchedAt: time.Now()}
	sharedModelCache["auth-stale"] = &sharedModelCacheEntry{fetchedAt: time.Now().Add(-time.Hour)}
	sharedModelCacheMu.Unlock()

	expired := EvictExpiredCopilotModelCaches()
	if !slices.Contains(expired, "auth-stale") || slices.Contains(expired, "auth-fresh") {
		t.Fatalf("expired auths %v, want only auth-stale", expired)
	}

	EvictAllCopilotModelCaches()
	sharedModelCacheMu.Lock()
	remaining := len(sharedModelCache)
	sharedModelCacheMu.Unlock()
	if remaining != 0 {
		t.Fatalf("EvictAllCopilotModelCaches left %d entries, want 0", remaining)
	}
}

func TestEvictExpiredChutesModelCache(t *testing.T) {
	chutesModelCacheMu.Lock()
	snapshot := chutesModelCache
	chutesModelCacheMu.Unlock()
	t.Cleanup(func() {
		chutesModelCacheMu.Lock()
		chutesModelCache = snapshot
		chutesModelCacheMu.Unlock()
	})

	chutesModelCacheMu.Lock()
	chutesModelCache = nil
	chutesModelCacheMu.Unlock()
	if EvictExpiredChutesModelCache() {
		t.Fatal("empty cache reported eviction, want false")
	}

	chutesModelCacheMu.Lock()
	chutesModelCache = &chutesModelCacheEntry{fetchedAt: time.Now()}
	chutesModelCacheMu.Unlock()
	if EvictExpiredChutesModelCache() {
		t.Fatal("fresh cache entry was evicted, want kept")
	}

	chutesModelCacheMu.Lock()
	chutesModelCache = &chutesModelCacheEntry{fetchedAt: time.Now().Add(-time.Hour)}
	chutesModelCacheMu.Unlock()
	if !EvictExpiredChutesModelCache() {
		t.Fatal("stale cache entry was kept, want evicted")
	}
}
