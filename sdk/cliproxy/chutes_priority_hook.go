package cliproxy

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	log "github.com/sirupsen/logrus"
)

// chutesPriorityHook implements ModelRegistryHook to apply Chutes priority filtering
// when non-Chutes models are registered.
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
	// Ignore Chutes registrations - they don't affect priority decisions
	if strings.ToLower(provider) == "chutes" {
		return
	}

	// Non-Chutes provider registered - schedule priority re-evaluation
	h.scheduleReeval()
}

func (h *chutesPriorityHook) OnModelsUnregistered(ctx context.Context, provider, clientID string) {
	// When non-Chutes provider is removed, Chutes models may become visible again
	if strings.ToLower(provider) == "chutes" {
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

		h.service.applyChutesModelPriority()
	})

	log.Debug("chutes priority: scheduled priority re-evaluation")
}
