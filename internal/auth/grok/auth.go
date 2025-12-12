package grok

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type GrokAuth struct {
	cfg        *config.Config
	httpClient *GrokHTTPClient
}

func NewGrokAuth(cfg *config.Config) *GrokAuth {
	return &GrokAuth{
		cfg:        cfg,
		httpClient: NewGrokHTTPClient(cfg, ""),
	}
}

// CheckRateLimits checks Grok account rate limits (stubbed for now).
func (g *GrokAuth) CheckRateLimits(ctx context.Context, storage *GrokTokenStorage, model string) (int, error) {
	if storage == nil {
		return -1, fmt.Errorf("grok auth: storage is nil")
	}

	modelCfg, ok := GetGrokModelConfig(model)
	if !ok {
		return -1, fmt.Errorf("grok auth: unknown model %q", model)
	}

	body, err := json.Marshal(map[string]any{
		"requestKind": "DEFAULT",
		"modelName":   modelCfg.RateLimitModel,
	})
	if err != nil {
		return -1, fmt.Errorf("grok auth: encode rate-limit body: %w", err)
	}

	headers := BuildHeaders(g.cfg, storage.SSOToken, storage.CFClearance, HeaderOptions{Path: "/rest/rate-limits"})
	resp, err := g.httpClient.Post(ctx, GrokRateLimitAPI, headers, body)
	if err != nil {
		return -1, err
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	data := resp.Bytes()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			storage.FailedCount++
			if storage.FailedCount >= MaxFailures {
				storage.Status = "expired"
			}
		}
		log.Warnf("grok auth: rate-limit check failed (status=%d token=%s model=%s): %s", resp.StatusCode, MaskToken(storage.SSOToken), model, summarizeBody(data))
		return -1, fmt.Errorf("grok auth: rate-limit check failed with status %d", resp.StatusCode)
	}

	remaining := -1
	if model == "grok-4-heavy" || strings.Contains(modelCfg.RateLimitModel, "heavy") {
		val := gjson.GetBytes(data, "remainingQueries")
		storage.HeavyRemainingQueries = -1
		if val.Exists() {
			storage.HeavyRemainingQueries = int(val.Int())
		}
		remaining = storage.HeavyRemainingQueries
	} else {
		val := gjson.GetBytes(data, "remainingTokens")
		storage.RemainingQueries = -1
		if val.Exists() {
			storage.RemainingQueries = int(val.Int())
		}
		remaining = storage.RemainingQueries
	}

	log.Debugf("grok auth: rate-limit refresh ok (model=%s token=%s remaining=%d heavy=%d)", model, MaskToken(storage.SSOToken), storage.RemainingQueries, storage.HeavyRemainingQueries)
	return remaining, nil
}

// MaskToken hides sensitive portions of tokens for logging.
func MaskToken(token string) string {
	if len(token) <= 8 {
		return token
	}

	return fmt.Sprintf("%s****%s", token[:4], token[len(token)-4:])
}

func summarizeBody(data []byte) string {
	body := strings.TrimSpace(string(data))
	if len(body) > 200 {
		return body[:200] + "..."
	}
	return body
}
