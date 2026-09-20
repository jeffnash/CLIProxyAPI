package cliproxy

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	log "github.com/sirupsen/logrus"
)

// dynamicModelRefreshInterval controls how often the service checks fetched
// (dynamic) model caches for TTL expiry. The check itself is cheap and performs
// no network I/O unless an entry has actually expired.
const dynamicModelRefreshInterval = time.Minute

// RefreshFetchedModels evicts fetched-model caches and re-registers models for
// every provider, or only the named providers when providers is non-empty.
// Provider names are case-insensitive; unknown names match nothing. Static
// providers re-register from the current catalog snapshot while dynamic
// providers (managed, copilot, chutes) refetch from their upstream /models
// endpoints. It returns the number of refreshed auths.
func (s *Service) RefreshFetchedModels(ctx context.Context, providers []string) int {
	if s == nil || s.coreManager == nil {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	filter := normalizeProviderFilter(providers)
	s.evictFetchedModelCaches(filter)
	return s.refreshModelRegistrations(ctx, filter)
}

// refreshModelRegistrations re-runs model registration for auths matching
// filter (a nil or empty filter matches all providers). It is shared by the
// static catalog refresh callback, the management refresh endpoint, and the
// TTL expiry loop so every path rebuilds per-auth model availability the
// same way.
func (s *Service) refreshModelRegistrations(ctx context.Context, filter map[string]bool) int {
	if s == nil || s.coreManager == nil {
		return 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	auths := s.coreManager.List()
	refreshed := 0
	var refreshedMu sync.Mutex
	tasks := make([]modelRegistrationTask, 0, len(auths))
	for _, item := range auths {
		if item == nil || item.ID == "" {
			continue
		}
		auth, ok := s.coreManager.GetByID(item.ID)
		if !ok || auth == nil || auth.Disabled {
			continue
		}
		if len(filter) > 0 && !filter[strings.ToLower(strings.TrimSpace(auth.Provider))] {
			continue
		}
		authForRefresh := auth
		tasks = append(tasks, modelRegistrationTask{
			phase:    modelRegistrationPhase(authForRefresh),
			category: modelRegistrationCategory(authForRefresh),
			run: func(compatCache *openAICompatibilityRegistrationCache) {
				if s.refreshModelRegistrationForAuthWithContext(ctx, authForRefresh, compatCache) {
					refreshedMu.Lock()
					refreshed++
					refreshedMu.Unlock()
				}
			},
		})
	}
	s.runModelRegistrationTasks(ctx, tasks)
	return refreshed
}

// evictFetchedModelCaches clears dynamic model caches for the filtered
// providers (or all providers when filter is empty) so the next registration
// refetches from upstream instead of serving stale snapshots.
func (s *Service) evictFetchedModelCaches(filter map[string]bool) {
	wants := func(provider string) bool {
		return len(filter) == 0 || filter[provider]
	}
	if wants("copilot") {
		executor.EvictAllCopilotModelCaches()
	}
	if wants("chutes") {
		executor.EvictChutesModelCache()
	}
	if len(filter) == 0 {
		executor.EvictManagedProviderModelCache("")
		return
	}
	for provider := range filter {
		if provider == "copilot" || provider == "chutes" {
			continue
		}
		if s.isManagedProvider(provider) {
			executor.EvictManagedProviderModelCache(provider)
		}
	}
}

// startDynamicModelRefreshLoop periodically evicts expired fetched-model cache
// entries and re-registers the affected auths, honoring the configured cache
// TTLs (30 minutes by default) without requiring a restart.
func (s *Service) startDynamicModelRefreshLoop(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		ticker := time.NewTicker(dynamicModelRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refreshExpiredFetchedModels(ctx)
			}
		}
	}()
	log.Infof("dynamic model refresh started (interval=%s)", dynamicModelRefreshInterval)
}

// refreshExpiredFetchedModels evicts only TTL-expired cache entries and
// re-registers auths belonging to the affected providers. It performs no
// network I/O when nothing has expired.
func (s *Service) refreshExpiredFetchedModels(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	expired := make(map[string]bool)
	for _, provider := range executor.EvictExpiredManagedProviderModelCache() {
		if name := strings.ToLower(strings.TrimSpace(provider)); name != "" {
			expired[name] = true
		}
	}
	for _, authID := range executor.EvictExpiredCopilotModelCaches() {
		if auth, ok := s.coreManager.GetByID(authID); ok && auth != nil {
			if name := strings.ToLower(strings.TrimSpace(auth.Provider)); name != "" {
				expired[name] = true
			}
		}
	}
	if executor.EvictExpiredChutesModelCache() {
		expired["chutes"] = true
	}
	if len(expired) == 0 {
		return
	}
	if refreshed := s.refreshModelRegistrations(ctx, expired); refreshed > 0 {
		log.Infof("re-registered models for %d auth(s) due to expired fetched-model cache", refreshed)
	}
}

func normalizeProviderFilter(providers []string) map[string]bool {
	var filter map[string]bool
	for _, provider := range providers {
		name := strings.ToLower(strings.TrimSpace(provider))
		if name == "" {
			continue
		}
		if filter == nil {
			filter = make(map[string]bool)
		}
		filter[name] = true
	}
	return filter
}
