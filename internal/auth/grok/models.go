package grok

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
)

type GrokModelConfig struct {
	GrokModel      string
	ModelMode      string
	RequiresSuper  bool
	ContextWindow  int
	MaxOutput      int
	RateLimitModel string
	IsVideoModel   bool
}

var GrokModels = map[string]GrokModelConfig{
	"grok-3-fast": {
		GrokModel:      "grok-3",
		ModelMode:      "MODEL_MODE_FAST",
		RequiresSuper:  false,
		ContextWindow:  131072,
		MaxOutput:      8192,
		RateLimitModel: "grok-3",
	},
	"grok-4-fast": {
		GrokModel:      "grok-4-mini-thinking-tahoe",
		ModelMode:      "MODEL_MODE_GROK_4_MINI_THINKING",
		RequiresSuper:  false,
		ContextWindow:  131072,
		MaxOutput:      8192,
		RateLimitModel: "grok-4-mini-thinking-tahoe",
	},
	"grok-4-fast-expert": {
		GrokModel:      "grok-4-mini-thinking-tahoe",
		ModelMode:      "MODEL_MODE_EXPERT",
		RequiresSuper:  false,
		ContextWindow:  131072,
		MaxOutput:      32768,
		RateLimitModel: "grok-4-mini-thinking-tahoe",
	},
	"grok-4-expert": {
		GrokModel:      "grok-4",
		ModelMode:      "MODEL_MODE_EXPERT",
		RequiresSuper:  false,
		ContextWindow:  131072,
		MaxOutput:      32768,
		RateLimitModel: "grok-4",
	},
	"grok-4-heavy": {
		GrokModel:      "grok-4-heavy",
		ModelMode:      "MODEL_MODE_HEAVY",
		RequiresSuper:  true,
		ContextWindow:  131072,
		MaxOutput:      65536,
		RateLimitModel: "grok-4-heavy",
	},
	"grok-4.1": {
		GrokModel:      "grok-4-1-non-thinking-w-tool",
		ModelMode:      "MODEL_MODE_GROK_4_1",
		RequiresSuper:  false,
		ContextWindow:  131072,
		MaxOutput:      8192,
		RateLimitModel: "grok-4-1-non-thinking-w-tool",
	},
	"grok-4.1-thinking": {
		GrokModel:      "grok-4-1-thinking-1108b",
		ModelMode:      "MODEL_MODE_AUTO",
		RequiresSuper:  false,
		ContextWindow:  131072,
		MaxOutput:      32768,
		RateLimitModel: "grok-4-1-thinking-1108b",
	},
	"grok-imagine-0.9": {
		GrokModel:      "grok-3",
		ModelMode:      "MODEL_MODE_FAST",
		RequiresSuper:  false,
		ContextWindow:  131072,
		MaxOutput:      8192,
		RateLimitModel: "grok-3",
		IsVideoModel:   true,
	},
}

// GetGrokModels returns registry-compatible model metadata for Grok provider.
func GetGrokModels() []*registry.ModelInfo {
	now := time.Now().Unix()
	supportedParams := []string{"temperature", "top_p", "max_tokens", "stream"}

	models := make([]*registry.ModelInfo, 0, len(GrokModels))
	for id, cfg := range GrokModels {
		models = append(models, &registry.ModelInfo{
			ID:                  id,
			Object:              "model",
			Created:             now,
			OwnedBy:             "xai",
			Type:                "grok",
			ContextLength:       cfg.ContextWindow,
			MaxCompletionTokens: cfg.MaxOutput,
			SupportedParameters: supportedParams,
		})
	}

	return models
}

func GetGrokModelConfig(model string) (GrokModelConfig, bool) {
	cfg, ok := GrokModels[model]
	if !ok {
		return GrokModelConfig{}, false
	}
	if cfg.RateLimitModel == "" {
		cfg.RateLimitModel = cfg.GrokModel
	}
	return cfg, true
}

func IsVideoModel(model string) bool {
	if cfg, ok := GetGrokModelConfig(model); ok {
		return cfg.IsVideoModel
	}
	return false
}
