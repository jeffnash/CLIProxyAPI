// This file implements the Chutes executor that proxies requests to the Chutes API.
package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const (
	chutesDefaultBaseURL = "https://llm.chutes.ai/v1"
	chutesModelsEndpoint = "/models"
	chutesChatEndpoint   = "/chat/completions"

	// Default retry configuration for Chutes 429 errors.
	chutesDefaultMaxRetries = 4
)

// chutesBackoffSchedule defines the backoff durations for retry attempts.
// Index 0 = first retry backoff, index 1 = second retry backoff, etc.
var chutesBackoffSchedule = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

func waitForDuration(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// chutesRetryableStatusCodes defines HTTP status codes that trigger retry logic.
var chutesRetryableStatusCodes = map[int]bool{
	429: true, // Too Many Requests - primary target
	502: true, // Bad Gateway
	503: true, // Service Unavailable
	504: true, // Gateway Timeout
}

// Shared caches (survive executor recreation)
var (
	chutesModelCacheMu sync.Mutex
	chutesModelCache   *chutesModelCacheEntry

	// Maps registry model IDs to actual Chutes API model IDs
	chutesAliasMapMu sync.RWMutex
	chutesAliasMap   = make(map[string]string)

	// Track registered normalized keys to detect collisions
	chutesNormalizedKeysMu sync.Mutex
	chutesNormalizedKeys   = make(map[string]string) // normalized → root
)

type chutesModelCacheEntry struct {
	models    []*registry.ModelInfo
	fetchedAt time.Time
}

const chutesModelCacheTTL = 30 * time.Minute

// ChutesModel represents a model from the Chutes /v1/models response.
type ChutesModel struct {
	ID                  string   `json:"id"`
	Root                string   `json:"root"`
	Quantization        string   `json:"quantization"`
	ContextLength       int      `json:"context_length"`
	MaxOutputLength     int      `json:"max_output_length"`
	InputModalities     []string `json:"input_modalities"`
	OutputModalities    []string `json:"output_modalities"`
	SupportedFeatures   []string `json:"supported_features"`
	ConfidentialCompute bool     `json:"confidential_compute"`
}

type ChutesExecutor struct {
	cfg *config.Config
}

func NewChutesExecutor(cfg *config.Config) *ChutesExecutor {
	return &ChutesExecutor{cfg: cfg}
}

func (e *ChutesExecutor) Identifier() string { return "chutes" }

// Execute performs a non-streaming chat completion request.
func (e *ChutesExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	apiKey, baseURL := chutesCreds(auth, e.cfg)
	if apiKey == "" {
		return resp, fmt.Errorf("chutes executor: missing api key")
	}

	reporter := newUsageReporter(ctx, e.Identifier(), req.Model, auth)
	defer reporter.trackFailure(ctx, &err)

	// Resolve model ID for Chutes API (strip chutes- prefix & consult cache)
	apiModel := resolveChutesModel(req.Model)
	log.WithFields(log.Fields{
		"model":          req.Model,
		"upstream_model": apiModel,
		"endpoint":       "chat.completions",
		"stream":         false,
	}).Info("chutes: request")

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), false)
	body, _ = sjson.SetBytes(body, "model", apiModel)

	endpoint := strings.TrimSuffix(baseURL, "/") + chutesChatEndpoint

	maxRetries := chutesMaxRetries(e.cfg)
	if maxRetries < 0 {
		maxRetries = 0
	}
	attempts := maxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	var data []byte
	for attempt := 0; attempt < attempts; attempt++ {
		start := time.Now()
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if errReq != nil {
			return resp, errReq
		}
		applyChutesHeaders(httpReq, apiKey, false)
		logChutesRequestHeaders(httpReq)

		httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			log.WithFields(log.Fields{
				"attempt":   attempt + 1,
				"duration":  time.Since(start).String(),
				"model":     req.Model,
				"endpoint":  endpoint,
				"http_error": errDo.Error(),
			}).Warn("chutes: request error")
			// Network errors are treated as transient; retry if we still have attempts.
			if attempt < attempts-1 {
				if errWait := chutesWaitForRetry(ctx, e.cfg, attempt, req.Model, 0); errWait != nil {
					return resp, errWait
				}
				continue
			}
			return resp, errDo
		}
		// Always record upstream status/headers for request logging (including happy paths).
		recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

		func() {
			defer httpResp.Body.Close()
			data, _ = io.ReadAll(httpResp.Body)
			if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
				log.WithFields(log.Fields{
					"attempt":          attempt + 1,
					"status":           httpResp.StatusCode,
					"duration":         time.Since(start).String(),
					"endpoint":         endpoint,
					"response_headers": formatChutesResponseHeaders(httpResp.Header),
					"body":             sanitizeResponseBody(data),
				}).Info("chutes: upstream non-2xx")
				appendAPIResponseChunk(ctx, e.cfg, data)
			} else {
				log.WithFields(log.Fields{
					"attempt":  attempt + 1,
					"status":   httpResp.StatusCode,
					"duration": time.Since(start).String(),
					"endpoint": endpoint,
				}).Info("chutes: upstream ok")
			}
		}()

		// Success path (2xx).
		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			break
		}

		// Retryable status codes.
		if chutesIsRetryableStatus(httpResp.StatusCode) && attempt < attempts-1 {
			if errWait := chutesWaitForRetry(ctx, e.cfg, attempt, req.Model, httpResp.StatusCode); errWait != nil {
				return resp, errWait
			}
			continue
		}

		// Not retryable or out of retries.
		se := statusErr{code: httpResp.StatusCode, msg: string(data)}
		if httpResp.StatusCode == http.StatusTooManyRequests {
			se.retryAfter = chutesShortRetryAfter()
		}
		return resp, se
	}

	if len(data) == 0 {
		// Defensive: should only happen if upstream returned empty body with 2xx.
		data = []byte("{}")
	}
	reporter.publish(ctx, parseOpenAIUsage(data))
	reporter.ensurePublished(ctx)

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), body, data, &param)
	return cliproxyexecutor.Response{Payload: []byte(out)}, nil
}

// ExecuteStream performs a streaming chat completion request.
func (e *ChutesExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	apiKey, baseURL := chutesCreds(auth, e.cfg)
	if apiKey == "" {
		return nil, fmt.Errorf("chutes executor: missing api key")
	}

	reporter := newUsageReporter(ctx, e.Identifier(), req.Model, auth)
	defer reporter.trackFailure(ctx, &err)

	apiModel := resolveChutesModel(req.Model)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), true)
	body, _ = sjson.SetBytes(body, "model", apiModel)

	endpoint := strings.TrimSuffix(baseURL, "/") + chutesChatEndpoint

	maxRetries := chutesMaxRetries(e.cfg)
	if maxRetries < 0 {
		maxRetries = 0
	}
	attempts := maxRetries + 1
	if attempts < 1 {
		attempts = 1
	}

	var httpResp *http.Response
	for attempt := 0; attempt < attempts; attempt++ {
		start := time.Now()
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if errReq != nil {
			return nil, errReq
		}
		applyChutesHeaders(httpReq, apiKey, true)
		logChutesRequestHeaders(httpReq)

		httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
		resp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			log.WithFields(log.Fields{
				"attempt":   attempt + 1,
				"duration":  time.Since(start).String(),
				"model":     req.Model,
				"endpoint":  endpoint,
				"http_error": errDo.Error(),
			}).Warn("chutes: stream bootstrap error")
			if attempt < attempts-1 {
				if errWait := chutesWaitForRetry(ctx, e.cfg, attempt, req.Model, 0); errWait != nil {
					return nil, errWait
				}
				continue
			}
			return nil, errDo
		}
		// Always record upstream status/headers for request logging (including happy paths).
		recordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			httpResp = resp
			break
		}

		// Read body for logging / error reporting.
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		log.WithFields(log.Fields{
			"attempt":          attempt + 1,
			"status":           resp.StatusCode,
			"duration":         time.Since(start).String(),
			"endpoint":         endpoint,
			"response_headers": formatChutesResponseHeaders(resp.Header),
			"body":             sanitizeResponseBody(data),
		}).Info("chutes: stream bootstrap non-2xx")
		appendAPIResponseChunk(ctx, e.cfg, data)

		if chutesIsRetryableStatus(resp.StatusCode) && attempt < attempts-1 {
			// Optional: respect upstream Retry-After, but cap to a short cooldown to avoid
			// punishing other models when one is overloaded.
			if resp.StatusCode == http.StatusTooManyRequests {
				if ra := parseRetryAfterHeader(resp.Header); ra != nil {
					cap := 5 * time.Second
					wait := minPositiveDuration(*ra, cap)
					log.WithFields(log.Fields{
						"attempt":     attempt + 1,
						"backoff":     wait.String(),
						"model":       req.Model,
						"status_code": resp.StatusCode,
						"retry_after": ra.String(),
						"capped":      cap.String(),
					}).Info("chutes: retrying after Retry-After")
					if errWait := waitForDuration(ctx, wait); errWait != nil {
						return nil, errWait
					}
					continue
				}
			}
			if errWait := chutesWaitForRetry(ctx, e.cfg, attempt, req.Model, resp.StatusCode); errWait != nil {
				return nil, errWait
			}
			continue
		}

		se := statusErr{code: resp.StatusCode, msg: string(data)}
		if resp.StatusCode == http.StatusTooManyRequests {
			se.retryAfter = chutesShortRetryAfter()
		}
		return nil, se
	}
	if httpResp == nil {
		return nil, fmt.Errorf("chutes executor: missing response")
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer httpResp.Body.Close()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		loggedLines := 0
		for scanner.Scan() {
			line := scanner.Bytes()
			if loggedLines < 8 {
				loggedLines++
				appendAPIResponseChunk(ctx, e.cfg, line)
			}
			if detail, ok := parseOpenAIStreamUsage(line); ok {
				reporter.publish(ctx, detail)
			}
			chunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
		reporter.ensurePublished(ctx)
	}()

	return out, nil
}

// Refresh - Chutes uses static API keys, no refresh needed.
func (e *ChutesExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

// CountTokens uses tiktoken estimation.
func (e *ChutesExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), false)

	enc, err := tokenizerForModel(req.Model)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("chutes executor: tokenizer init failed: %w", err)
	}

	count, err := countOpenAIChatTokens(enc, body)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("chutes executor: token counting failed: %w", err)
	}

	usageJSON := buildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

// HttpRequest injects Chutes credentials and executes HTTP request.
func (e *ChutesExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("chutes executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	apiKey, _ := chutesCreds(auth, e.cfg)
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// FetchModels fetches and processes models from Chutes API.
// NOT part of ProviderExecutor interface, but follows Copilot/Antigravity convention.
func (e *ChutesExecutor) FetchModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	// 1. Check cache
	chutesModelCacheMu.Lock()
	if chutesModelCache != nil && time.Since(chutesModelCache.fetchedAt) < chutesModelCacheTTL {
		models := chutesModelCache.models
		chutesModelCacheMu.Unlock()
		return models
	}
	chutesModelCacheMu.Unlock()

	apiKey, baseURL := chutesCreds(auth, cfg)
	if apiKey == "" {
		log.Warn("chutes: no API key configured, using static fallback")
		return registry.GetChutesModels()
	}

	// 2. Fetch from /v1/models using proxy-aware client
	endpoint := strings.TrimSuffix(baseURL, "/") + chutesModelsEndpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		log.Warnf("chutes: failed to create models request: %v", err)
		return registry.GetChutesModels()
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	httpClient := newProxyAwareHTTPClient(ctx, cfg, auth, 15*time.Second)
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Warnf("chutes: failed to fetch models: %v", err)
		return registry.GetChutesModels()
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warnf("chutes: /models returned status %d", resp.StatusCode)
		return registry.GetChutesModels()
	}

	var listResp struct {
		Data []ChutesModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		log.Warnf("chutes: failed to decode models response: %v", err)
		return registry.GetChutesModels()
	}

	// 3. Process models
	teePref := strings.ToLower(strings.TrimSpace(cfg.Chutes.TEEPreference))
	if teePref == "" {
		teePref = "prefer"
	}

	models := processChutesModels(listResp.Data, teePref, cfg.Chutes.Models, cfg.Chutes.ModelsExclude)

	// 4. Generate aliases
	models = registry.GenerateChutesAliases(models)

	// 5. Cache
	chutesModelCacheMu.Lock()
	chutesModelCache = &chutesModelCacheEntry{
		models:    models,
		fetchedAt: time.Now(),
	}
	chutesModelCacheMu.Unlock()

	return models
}

// processChutesModels groups by root, selects best variant, applies filters.
func processChutesModels(raw []ChutesModel, teePref string, whitelist, blocklist []string) []*registry.ModelInfo {
	byRoot := make(map[string][]ChutesModel)
	for _, m := range raw {
		root := m.Root
		if root == "" {
			root = m.ID
		}
		byRoot[root] = append(byRoot[root], m)
	}

	whitelistSet := make(map[string]bool)
	for _, r := range whitelist {
		whitelistSet[strings.TrimSpace(r)] = true
	}
	blocklistSet := make(map[string]bool)
	for _, r := range blocklist {
		blocklistSet[strings.TrimSpace(r)] = true
	}

	chutesNormalizedKeysMu.Lock()
	chutesNormalizedKeys = make(map[string]string)
	chutesNormalizedKeysMu.Unlock()

	var models []*registry.ModelInfo
	chutesAliasMapMu.Lock()
	defer chutesAliasMapMu.Unlock()
	chutesAliasMap = make(map[string]string) // reset

	for root, variants := range byRoot {
		if len(whitelistSet) > 0 && !whitelistSet[root] {
			continue
		}
		if blocklistSet[root] {
			continue
		}

		selected := selectChutesVariants(variants, teePref)
		for _, sel := range selected {
			registryID := root
			isTEEVariant := false
			if sel.ConfidentialCompute && teePref == "both" {
				registryID = root + "-tee"
				isTEEVariant = true
			}

			normalizedKey := registry.NormalizeChutesModelKey(root)
			if isTEEVariant {
				normalizedKey = normalizedKey + "-tee"
			}

			chutesNormalizedKeysMu.Lock()
			if existingRoot, exists := chutesNormalizedKeys[normalizedKey]; exists && existingRoot != registryID {
				log.Warnf("chutes: normalized key collision: %q maps to both %q and %q, skipping %q", normalizedKey, existingRoot, registryID, registryID)
				chutesNormalizedKeysMu.Unlock()
				continue
			}
			chutesNormalizedKeys[normalizedKey] = registryID
			chutesNormalizedKeysMu.Unlock()

			chutesAliasMap[registryID] = sel.ID
			chutesAliasMap[registry.ChutesModelPrefix+registryID] = sel.ID

			baseNormalizedKey := registry.NormalizeChutesModelKey(root)
			if !isTEEVariant {
				chutesAliasMap[baseNormalizedKey] = sel.ID
			} else {
				chutesAliasMap[normalizedKey] = sel.ID
			}

			info := &registry.ModelInfo{
				ID:                  registryID,
				Object:              "model",
				Created:             time.Now().Unix(),
				OwnedBy:             "chutes",
				Type:                "chutes",
				DisplayName:         formatChutesDisplayName(root, sel.ConfidentialCompute),
				Description:         fmt.Sprintf("via Chutes API (%s)", sel.Quantization),
				ContextLength:       sel.ContextLength,
				MaxCompletionTokens: sel.MaxOutputLength,
				SupportedParameters: mapChutesFeatures(sel.SupportedFeatures),
			}
			models = append(models, info)
		}
	}

	return models
}

// selectChutesVariants picks the best variant(s) from a group sharing the same root.
func selectChutesVariants(variants []ChutesModel, teePref string) []ChutesModel {
	if len(variants) == 0 {
		return nil
	}

	var tee, nonTee []ChutesModel
	for _, v := range variants {
		if v.ConfidentialCompute {
			tee = append(tee, v)
		} else {
			nonTee = append(nonTee, v)
		}
	}

	pickBest := func(vs []ChutesModel) ChutesModel {
		if len(vs) == 0 {
			return ChutesModel{}
		}
		sort.Slice(vs, func(i, j int) bool {
			pi, pj := quantizationScore(vs[i].Quantization), quantizationScore(vs[j].Quantization)
			if pi != pj {
				return pi > pj
			}
			return vs[i].ContextLength > vs[j].ContextLength
		})
		return vs[0]
	}

	switch teePref {
	case "both":
		var result []ChutesModel
		if len(tee) > 0 {
			result = append(result, pickBest(tee))
		}
		if len(nonTee) > 0 {
			result = append(result, pickBest(nonTee))
		}
		return result
	case "avoid":
		if len(nonTee) > 0 {
			return []ChutesModel{pickBest(nonTee)}
		}
		return []ChutesModel{pickBest(tee)}
	default: // prefer
		if len(tee) > 0 {
			return []ChutesModel{pickBest(tee)}
		}
		return []ChutesModel{pickBest(nonTee)}
	}
}

func quantizationScore(q string) int {
	switch strings.ToLower(q) {
	case "bf16":
		return 100
	case "fp16":
		return 90
	case "fp8":
		return 80
	case "fp4":
		return 70
	case "int8":
		return 60
	case "int4":
		return 50
	default:
		return 75
	}
}

func formatChutesDisplayName(root string, isTEE bool) string {
	name := root
	if idx := strings.LastIndex(root, "/"); idx >= 0 {
		name = root[idx+1:]
	}
	if isTEE {
		return name + " (TEE)"
	}
	return name
}

func mapChutesFeatures(features []string) []string {
	params := []string{"temperature", "top_p", "max_tokens", "stream"}
	for _, f := range features {
		switch f {
		case "tools":
			params = append(params, "tools")
		case "json_mode":
			params = append(params, "response_format")
		case "reasoning":
			params = append(params, "reasoning_effort")
		}
	}
	return params
}

func resolveChutesModel(modelID string) string {
	stripped := strings.TrimPrefix(modelID, registry.ChutesModelPrefix)

	chutesAliasMapMu.RLock()
	if apiID, ok := chutesAliasMap[stripped]; ok {
		chutesAliasMapMu.RUnlock()
		return apiID
	}
	normalized := registry.NormalizeChutesModelKey(stripped)
	if apiID, ok := chutesAliasMap[normalized]; ok {
		chutesAliasMapMu.RUnlock()
		return apiID
	}
	chutesAliasMapMu.RUnlock()

	return stripped
}

func applyChutesHeaders(r *http.Request, apiKey string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+apiKey)
	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
}

// logChutesRequestHeaders logs request headers with sensitive values masked.
func logChutesRequestHeaders(r *http.Request) {
	fields := log.Fields{"endpoint": r.URL.String()}
	for k := range r.Header {
		fields[k] = util.MaskSensitiveHeaderValue(k, r.Header.Get(k))
	}
	log.WithFields(fields).Debug("chutes request headers")
}

// formatChutesResponseHeaders formats all response headers for logging non-2xx responses.
// Sensitive values (like API keys) are masked using util.MaskSensitiveHeaderValue.
func formatChutesResponseHeaders(h http.Header) map[string]string {
	if h == nil {
		return nil
	}
	headers := make(map[string]string)
	for key := range h {
		headers[key] = util.MaskSensitiveHeaderValue(key, h.Get(key))
	}
	return headers
}

// sensitiveBodyPatterns contains regex patterns for sensitive data in response bodies.
var sensitiveBodyPatterns = []*regexp.Regexp{
	// API keys: sk-..., pk-..., key-..., etc.
	regexp.MustCompile(`(?i)(["\']?(?:api[_-]?key|secret[_-]?key|access[_-]?key|auth[_-]?token|bearer)["\']?\s*[:=]\s*["\']?)([a-zA-Z0-9_-]{8,})(["\']?)`),
	// Bearer tokens in JSON
	regexp.MustCompile(`(?i)(["\']?bearer["\']?\s*[:=]\s*["\']?)([a-zA-Z0-9_.-]{20,})(["\']?)`),
	// Generic long tokens/keys that look like secrets
	regexp.MustCompile(`(?i)(["\']?(?:token|credential|password|secret)["\']?\s*[:=]\s*["\']?)([a-zA-Z0-9_+/=-]{16,})(["\']?)`),
}

// sanitizeResponseBody masks sensitive patterns in the response body while preserving structure.
func sanitizeResponseBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	result := string(body)
	for _, pattern := range sensitiveBodyPatterns {
		result = pattern.ReplaceAllStringFunc(result, func(match string) string {
			submatches := pattern.FindStringSubmatch(match)
			if len(submatches) >= 4 {
				// submatches[1] = prefix, submatches[2] = sensitive value, submatches[3] = suffix
				return submatches[1] + util.HideAPIKey(submatches[2]) + submatches[3]
			}
			return match
		})
	}
	return result
}

func chutesCreds(auth *cliproxyauth.Auth, cfg *config.Config) (apiKey, baseURL string) {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			apiKey = v
		}
		if v := strings.TrimSpace(auth.Attributes["base_url"]); v != "" {
			baseURL = v
		}
	}
	if apiKey == "" && cfg != nil {
		apiKey = cfg.Chutes.APIKey
	}
	if baseURL == "" && cfg != nil && cfg.Chutes.BaseURL != "" {
		baseURL = cfg.Chutes.BaseURL
	}
	if baseURL == "" {
		baseURL = chutesDefaultBaseURL
	}
	return apiKey, baseURL
}

// EvictChutesModelCache clears the model cache (for testing or auth removal).
func EvictChutesModelCache() {
	chutesModelCacheMu.Lock()
	chutesModelCache = nil
	chutesModelCacheMu.Unlock()
}

// chutesMaxRetries returns the configured max retries or the default.
func chutesMaxRetries(cfg *config.Config) int {
	if cfg == nil {
		return chutesDefaultMaxRetries
	}
	// Honor explicit 0 as "no retries".
	if cfg.Chutes.MaxRetries >= 0 {
		return cfg.Chutes.MaxRetries
	}
	// Negative values are treated as invalid and fall back to default.
	return chutesDefaultMaxRetries
}

// chutesBackoffDuration returns the backoff duration for a given attempt (0-indexed).
// Uses configured backoff schedule from cfg if available, otherwise uses default schedule.
func chutesBackoffDuration(cfg *config.Config, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}

	schedule := parseChutesBackoffSchedule(cfg)
	if attempt >= len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[attempt]
}

// parseChutesBackoffSchedule parses the configured backoff schedule or returns default.
// Format: comma-separated seconds like "5,15,30,60"
func parseChutesBackoffSchedule(cfg *config.Config) []time.Duration {
	if cfg == nil || cfg.Chutes.RetryBackoff == "" {
		return chutesBackoffSchedule
	}

	parts := strings.Split(cfg.Chutes.RetryBackoff, ",")
	if len(parts) == 0 {
		return chutesBackoffSchedule
	}

	var schedule []time.Duration
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		seconds, err := strconv.Atoi(trimmed)
		if err != nil || seconds < 0 {
			// Invalid value, fall back to default
			log.WithFields(log.Fields{
				"value":   trimmed,
				"section": "chutes.retry-backoff",
			}).Warn("invalid backoff value, using default schedule")
			return chutesBackoffSchedule
		}
		schedule = append(schedule, time.Duration(seconds)*time.Second)
	}

	if len(schedule) == 0 {
		return chutesBackoffSchedule
	}
	return schedule
}

// chutesIsRetryableStatus returns true if the status code should trigger retry logic.
func chutesIsRetryableStatus(statusCode int) bool {
	return chutesRetryableStatusCodes[statusCode]
}

// chutesWaitForRetry waits for the backoff duration or until context is cancelled.
func chutesWaitForRetry(ctx context.Context, cfg *config.Config, attempt int, model string, statusCode int) error {
	backoff := chutesBackoffDuration(cfg, attempt)
	log.WithFields(log.Fields{
		"attempt":     attempt + 1,
		"backoff":     backoff.String(),
		"model":       model,
		"status_code": statusCode,
	}).Info("chutes: retrying after backoff")

	timer := time.NewTimer(backoff)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func parseRetryAfterHeader(h http.Header) *time.Duration {
	if h == nil {
		return nil
	}
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	// Retry-After can be seconds or an HTTP-date, but we only support seconds here.
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			seconds = 0
		}
		d := time.Duration(seconds) * time.Second
		return &d
	}
	return nil
}

func minPositiveDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

// chutesShortRetryAfter returns a short retry-after duration for per-model cooldown.
// This ensures Chutes models have short cooldowns since one model being overloaded
// doesn't mean others are affected.
func chutesShortRetryAfter() *time.Duration {
	d := 5 * time.Second
	return &d
}
