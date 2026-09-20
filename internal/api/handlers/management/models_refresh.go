package management

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// RefreshModels evicts fetched-model caches and re-registers models for every
// provider, or only the requested providers. Accepts repeatable or
// comma-separated query parameters (?provider=a6&provider=meta) or a JSON body
// ({"providers": ["a6"], "provider": "a6"}). An empty filter refreshes all
// providers.
func (h *Handler) RefreshModels(c *gin.Context) {
	h.mu.Lock()
	hook := h.modelRefreshHook
	h.mu.Unlock()
	if hook == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "model refresh unavailable"})
		return
	}

	var req struct {
		Provider  string   `json:"provider"`
		Providers []string `json:"providers"`
	}
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if errBind := c.ShouldBindJSON(&req); errBind != nil && !errors.Is(errBind, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + errBind.Error()})
			return
		}
	}
	providers := append([]string{req.Provider}, req.Providers...)
	providers = append(providers, c.QueryArray("provider")...)
	providers = append(providers, c.QueryArray("providers")...)
	normalized := make([]string, 0, len(providers))
	seen := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		for _, part := range strings.Split(provider, ",") {
			name := strings.ToLower(strings.TrimSpace(part))
			if name == "" {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			normalized = append(normalized, name)
		}
	}

	refreshed := hook(c.Request.Context(), normalized)
	c.JSON(http.StatusOK, gin.H{
		"ok":        true,
		"refreshed": refreshed,
		"providers": normalized,
	})
}
