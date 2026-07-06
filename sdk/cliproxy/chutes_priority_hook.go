package cliproxy

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	log "github.com/sirupsen/logrus"
)

// chutesPriorityHook implements ModelRegistryHook to apply fallback-priority
// filtering for explicit-routing providers when other models change.
type chutesPriorityHook struct {
	service      *Service
	debounceTime time.Duration

	mu      sync.Mutex
	timer   *time.Timer
	pending bool
}

func newChutesPriorityHook(s *Service, debounce time.Duration) *chutesPriorityHook {
	return &chutesPriorityHook{
		service:      s,
		debounceTime: debounce,
	}
}

func (h *chutesPriorityHook) OnModelsRegistered(ctx context.Context, provider, clientID string, models []*registry.ModelInfo) {
	// Ignore managed provider registrations to avoid re-evaluation loops.
	if h.isManagedFallbackPriorityProvider(provider) {
		return
	}

	// Other provider registrations may hide fallback-priority base IDs.
	h.scheduleReeval()
}

func (h *chutesPriorityHook) OnModelsUnregistered(ctx context.Context, provider, clientID string) {
	// Managed provider unregisters do not affect their own explicit aliases.
	if h.isManagedFallbackPriorityProvider(provider) {
		return
	}
	h.scheduleReeval()
}

func (h *chutesPriorityHook) scheduleReeval() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.timer != nil {
		h.timer.Stop()
	}

	h.pending = true
	h.timer = time.AfterFunc(h.debounceTime, func() {
		h.mu.Lock()
		h.pending = false
		h.mu.Unlock()

		h.service.applyManagedProviderModelPriorities()
	})

	log.Debug("provider priority: scheduled priority re-evaluation")
}

func (h *chutesPriorityHook) isManagedFallbackPriorityProvider(provider string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), "chutes") {
		return true
	}
	return h != nil && h.service != nil && h.service.isManagedProvider(provider)
}
