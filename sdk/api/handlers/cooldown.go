package handlers

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ClearCooldown clears cooldown/suspension for a provider, or one provider×model pair.
// Auth: Bearer API key via AuthMiddleware (any valid api-keys entry).
// Body: {"provider":"vsllm","model":"kimi-k3"}  // model optional; if omitted → provider-wide.
// Provider match is case-insensitive against Auth.Provider.
func (h *BaseAPIHandler) ClearCooldown(c *gin.Context) {
	if h == nil || h.AuthManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	provider := strings.TrimSpace(req.Provider)
	model := strings.TrimSpace(req.Model)
	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
		return
	}
	authIDs, clearedModels, err := h.AuthManager.ClearCooldown(c.Request.Context(), provider, model)
	if err != nil {
		msg := strings.ToLower(err.Error())
		if strings.Contains(msg, "no auth found") {
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		if strings.Contains(msg, "provider is required") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"provider":       provider,
		"model":          model,
		"auth_ids":       authIDs,
		"cleared_models": clearedModels,
	})
}
