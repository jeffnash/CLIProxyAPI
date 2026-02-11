package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	copilotauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CopilotExecutor handles requests to GitHub Copilot API.
// It manages token refresh and proper header injection for Copilot requests.
type CopilotExecutor struct {
	cfg            *config.Config
	tokenMu        sync.RWMutex
	mu             sync.Mutex
	tokenCache     map[string]*cachedToken
	modelMu        sync.Mutex
	initiatorCount map[string]uint64
}

// cachedToken stores the Copilot token and its expiration time.
type cachedToken struct {
	token     string
	expiresAt time.Time
}

// modelCacheEntry stores cached models.
// Shared model cache across executor instances (survives executor recreation).
var (
	sharedModelCacheMu sync.Mutex
	sharedModelCache   = make(map[string]*sharedModelCacheEntry)
)

type sharedModelCacheEntry struct {
	models    []*registry.ModelInfo
	fetchedAt time.Time
}

const sharedModelCacheTTL = 30 * time.Minute

// NewCopilotExecutor creates a new CopilotExecutor instance.

func NewCopilotExecutor(cfg *config.Config) *CopilotExecutor {
	return &CopilotExecutor{
		cfg:            cfg,
		tokenCache:     make(map[string]*cachedToken),
		initiatorCount: make(map[string]uint64),
	}
}

func (e *CopilotExecutor) Identifier() string { return "copilot" }

func (e *CopilotExecutor) logOutboundProxyDecision(httpReq *http.Request, auth *cliproxyauth.Auth, transport string) {
	// Request-scoped log (no dedupe): user wants to see proxy usage for every outbound Copilot request.
	host := ""
	if httpReq != nil && httpReq.URL != nil {
		host = strings.TrimSpace(httpReq.URL.Hostname())
	}

	// Match newProxyAwareHTTPClient + electron decision order:
	// 1) auth.ProxyURL (always allowed)
	// 2) global cfg.ProxyURL (only if enabled for "copilot")
	// then apply NO_PROXY.
	proxyURL := ""
	proxySource := ""
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
		if proxyURL != "" {
			proxySource = "auth"
		}
	}

	disabledByServices := false
	if proxyURL == "" && e != nil && e.cfg != nil {
		if e.cfg.SDKConfig.ProxyEnabledFor("copilot") {
			proxyURL = strings.TrimSpace(e.cfg.ProxyURL)
			if proxyURL != "" {
				proxySource = "global"
			}
		} else if strings.TrimSpace(e.cfg.ProxyURL) != "" {
			disabledByServices = true
		}
	}

	noProxyRaw := ""
	noProxyList := []string(nil)
	if proxyURL != "" {
		noProxyRaw = noProxyEnvRaw()
		noProxyList = parseNoProxyList(noProxyRaw)
	}

	used := false
	reason := ""
	if proxyURL == "" {
		if disabledByServices {
			reason = "disabled_by_OUTBOUND_PROXY_SERVICES"
		} else {
			reason = "not_configured"
		}
	} else if host != "" && shouldBypassProxy(host, noProxyList) {
		reason = "NO_PROXY"
	} else {
		used = true
		reason = "enabled"
	}

	if used {
		log.Infof("copilot outbound: proxy_used=true proxy=%s source=%s host=%s no_proxy=%q transport=%s",
			maskProxyURL(proxyURL),
			proxySource,
			host,
			noProxyRaw,
			transport,
		)
		return
	}
	log.Infof("copilot outbound: proxy_used=false reason=%s host=%s no_proxy=%q transport=%s",
		reason,
		host,
		noProxyRaw,
		transport,
	)
}

func (e *CopilotExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}

	payload := []byte{}
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		payload = body
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	copilotToken, _, err := e.getCopilotToken(req.Context(), auth)
	if err != nil {
		return err
	}

	incoming := req.Header.Clone()
	e.applyCopilotHeaders(req, copilotToken, payload, incoming)

	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)

	return nil
}

// HttpRequest injects Copilot credentials into the request and executes it.
func (e *CopilotExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("copilot executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}

	// Parity default: attempt to use Electron/Chromium net stack first (if available),
	// then fall back to Go's net/http transport.
	if copilotPreferElectronTransport() {
		var proxyURL string
		proxySource := ""
		if auth != nil {
			proxyURL = strings.TrimSpace(auth.ProxyURL)
			if proxyURL != "" {
				proxySource = "auth"
			}
		}
		if proxyURL == "" && e != nil && e.cfg != nil && e.cfg.SDKConfig.ProxyEnabledFor("copilot") {
			proxyURL = strings.TrimSpace(e.cfg.ProxyURL)
			if proxyURL != "" {
				proxySource = "global"
			}
		} else if proxyURL == "" && e != nil && e.cfg != nil && strings.TrimSpace(e.cfg.ProxyURL) != "" && !e.cfg.SDKConfig.ProxyEnabledFor("copilot") {
			logProxyOnce(
				"proxy.disabled.copilot.electron",
				"proxy: service=copilot disabled by OUTBOUND_PROXY_SERVICES (proxy configured but not enabled for service)",
			)
		}

		if proxyURL != "" {
			noProxyRaw := noProxyEnvRaw()
			noProxyList := parseNoProxyList(noProxyRaw)
			if httpReq.URL != nil {
				host := strings.TrimSpace(httpReq.URL.Hostname())
				if shouldBypassProxy(host, noProxyList) {
					logProxyOnce(
						"proxy.bypass.copilot.electron."+strings.ToLower(host),
						"proxy: service=copilot bypass host=%s reason=NO_PROXY transport=electron",
						host,
					)
					proxyURL = ""
				}
			}
			if proxyURL != "" {
				logProxyOnce(
					"proxy.enabled.copilot.electron."+maskProxyURL(proxyURL),
					"proxy: service=copilot enabled proxy=%s source=%s no_proxy=%q transport=electron",
					maskProxyURL(proxyURL),
					proxySource,
					noProxyRaw,
				)
			}
		}

		// Per-request proxy log (no dedupe) for the actual electron attempt.
		// If NO_PROXY caused a bypass above, proxyURL will be empty here.
		e.logOutboundProxyDecision(httpReq, auth, "electron")

		if resp, err := httpResponseFromElectron(ctx, httpReq, proxyURL); err == nil {
			return resp, nil
		} else if err != nil && !errors.Is(err, errCopilotElectronUnavailable) {
			log.Debugf("copilot executor: electron transport failed, falling back to go transport: %v", err)
		}
	}

	// Per-request proxy log (no dedupe) for Go net/http transport.
	e.logOutboundProxyDecision(httpReq, auth, "go")

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0, "copilot")
	return httpClient.Do(httpReq)
}

// reasoningCache returns the shared Gemini reasoning cache for a given auth, or a fresh
// cache when auth is nil/unknown. This keeps Gemini reasoning warm across reauths.
func (e *CopilotExecutor) reasoningCache(auth *cliproxyauth.Auth) *geminiReasoningCache {
	if auth == nil || strings.TrimSpace(auth.ID) == "" {
		return newGeminiReasoningCache()
	}
	return getSharedGeminiReasoningCache(strings.TrimSpace(auth.ID))
}

// stripCopilotPrefix removes the "copilot-" prefix from model names if present.
// This allows users to explicitly route to Copilot using "copilot-gpt-5" while
// the actual API call uses "gpt-5".
func stripCopilotPrefix(model string) string {
	return strings.TrimPrefix(model, registry.CopilotModelPrefix)
}

// essentialCopilotModels are models that Copilot supports but may not be returned
// by the /models API (e.g., ModelPickerEnabled=false). These are merged into the
// dynamic model list to ensure they're always available for explicit routing.
var essentialCopilotModels = []struct {
	ID                  string
	DisplayName         string
	Description         string
	ContextLength       int
	MaxCompletionTokens int
}{
	{
		ID:                  "gemini-3-flash-preview",
		DisplayName:         "Gemini 3 Flash (Preview)",
		Description:         "Google model via GitHub Copilot (Preview)",
		ContextLength:       128000,
		MaxCompletionTokens: 64000,
	},
}

// mergeEssentialCopilotModels adds essential models that may not be returned by /models
// but are known to work with Copilot. Only adds models that aren't already present.
func mergeEssentialCopilotModels(models []*registry.ModelInfo, now int64) []*registry.ModelInfo {
	existing := make(map[string]bool, len(models))
	for _, m := range models {
		existing[strings.ToLower(m.ID)] = true
	}

	paramsWithTools := []string{"temperature", "top_p", "max_tokens", "stream", "tools"}

	for _, em := range essentialCopilotModels {
		if existing[strings.ToLower(em.ID)] {
			continue
		}
		models = append(models, &registry.ModelInfo{
			ID:                  em.ID,
			Object:              "model",
			Created:             now,
			OwnedBy:             "copilot",
			Type:                "copilot",
			DisplayName:         em.DisplayName,
			Description:         em.Description,
			ContextLength:       em.ContextLength,
			MaxCompletionTokens: em.MaxCompletionTokens,
			SupportedParameters: paramsWithTools,
		})
		log.Debugf("copilot executor: added essential model %s", em.ID)
	}

	return models
}

// resolveCopilotAlias resolves model aliases with reasoning effort suffixes.
// For example: "gpt-5-high" -> ("gpt-5", "high", true)
// Supported efforts: minimal, none, low, medium, high, xhigh (xhigh only for gpt-5.1-codex-max, gpt-5.2, gpt-5.2-codex, gpt-5.3-codex)
func resolveCopilotAlias(modelName string) (baseModel, effort string, ok bool) {
	m := strings.ToLower(strings.TrimSpace(modelName))

	// gpt-5.2-codex variants (supports xhigh)
	switch m {
	case "gpt-5.2-codex-low":
		return "gpt-5.2-codex", "low", true
	case "gpt-5.2-codex-medium":
		return "gpt-5.2-codex", "medium", true
	case "gpt-5.2-codex-high":
		return "gpt-5.2-codex", "high", true
	case "gpt-5.2-codex-xhigh":
		return "gpt-5.2-codex", "xhigh", true
	}

	// gpt-5.3-codex variants (supports xhigh)
	switch m {
	case "gpt-5.3-codex-low":
		return "gpt-5.3-codex", "low", true
	case "gpt-5.3-codex-medium":
		return "gpt-5.3-codex", "medium", true
	case "gpt-5.3-codex-high":
		return "gpt-5.3-codex", "high", true
	case "gpt-5.3-codex-xhigh":
		return "gpt-5.3-codex", "xhigh", true
	}

	// gpt-5.2 variants (supports xhigh)
	switch m {
	case "gpt-5.2-none":
		return "gpt-5.2", "none", true
	case "gpt-5.2-low":
		return "gpt-5.2", "low", true
	case "gpt-5.2-medium":
		return "gpt-5.2", "medium", true
	case "gpt-5.2-high":
		return "gpt-5.2", "high", true
	case "gpt-5.2-xhigh":
		return "gpt-5.2", "xhigh", true
	}

	// gpt-5.1-codex-max variants (supports xhigh)
	switch m {
	case "gpt-5.1-codex-max-low":
		return "gpt-5.1-codex-max", "low", true
	case "gpt-5.1-codex-max-medium":
		return "gpt-5.1-codex-max", "medium", true
	case "gpt-5.1-codex-max-high":
		return "gpt-5.1-codex-max", "high", true
	case "gpt-5.1-codex-max-xhigh":
		return "gpt-5.1-codex-max", "xhigh", true
	}

	// gpt-5.1-codex variants
	switch m {
	case "gpt-5.1-codex-low":
		return "gpt-5.1-codex", "low", true
	case "gpt-5.1-codex-medium":
		return "gpt-5.1-codex", "medium", true
	case "gpt-5.1-codex-high":
		return "gpt-5.1-codex", "high", true
	}

	// gpt-5.1-codex-mini variants
	switch m {
	case "gpt-5.1-codex-mini-low":
		return "gpt-5.1-codex-mini", "low", true
	case "gpt-5.1-codex-mini-medium":
		return "gpt-5.1-codex-mini", "medium", true
	case "gpt-5.1-codex-mini-high":
		return "gpt-5.1-codex-mini", "high", true
	}

	// gpt-5.1 variants
	switch m {
	case "gpt-5.1-none":
		return "gpt-5.1", "none", true
	case "gpt-5.1-low":
		return "gpt-5.1", "low", true
	case "gpt-5.1-medium":
		return "gpt-5.1", "medium", true
	case "gpt-5.1-high":
		return "gpt-5.1", "high", true
	}

	// gpt-5-codex variants
	switch m {
	case "gpt-5-codex-low":
		return "gpt-5-codex", "low", true
	case "gpt-5-codex-medium":
		return "gpt-5-codex", "medium", true
	case "gpt-5-codex-high":
		return "gpt-5-codex", "high", true
	}

	// gpt-5-codex-mini variants
	switch m {
	case "gpt-5-codex-mini-medium":
		return "gpt-5-codex-mini", "medium", true
	case "gpt-5-codex-mini-high":
		return "gpt-5-codex-mini", "high", true
	}

	// gpt-5 variants
	switch m {
	case "gpt-5-minimal":
		return "gpt-5", "minimal", true
	case "gpt-5-low":
		return "gpt-5", "low", true
	case "gpt-5-medium":
		return "gpt-5", "medium", true
	case "gpt-5-high":
		return "gpt-5", "high", true
	}

	// gpt-5-mini variants
	switch m {
	case "gpt-5-mini-low":
		return "gpt-5-mini", "low", true
	case "gpt-5-mini-medium":
		return "gpt-5-mini", "medium", true
	case "gpt-5-mini-high":
		return "gpt-5-mini", "high", true
	}

	return "", "", false
}

// setCopilotReasoningEffort sets reasoning.effort in the payload for Copilot GPT models.
// Also sets reasoning.summary to "auto" and includes reasoning.encrypted_content.
func setCopilotReasoningEffort(payload []byte, effort string) []byte {
	if strings.TrimSpace(effort) == "" {
		return payload
	}
	payload, _ = sjson.SetBytes(payload, "reasoning.effort", strings.ToLower(strings.TrimSpace(effort)))
	payload, _ = sjson.SetBytes(payload, "reasoning.summary", "auto")
	// Note: "include" for encrypted_content is typically set via query params or headers,
	// but we set it in body for completeness where supported
	return payload
}

// sanitizeCopilotPayload removes fields that Copilot's Chat Completions endpoint
// rejects (strip max_tokens and parallel_tool_calls).
func sanitizeCopilotPayload(body []byte, model string) []byte {
	if len(body) == 0 {
		return body
	}
	if gjson.GetBytes(body, "max_tokens").Exists() {
		if cleaned, err := sjson.DeleteBytes(body, "max_tokens"); err == nil {
			body = cleaned
		}
	}
	if gjson.GetBytes(body, "parallel_tool_calls").Exists() {
		if cleaned, err := sjson.DeleteBytes(body, "parallel_tool_calls"); err == nil {
			body = cleaned
		}
	}
	return body
}

func (e *CopilotExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	copilotToken, accountType, err := e.getCopilotToken(ctx, auth)
	if err != nil {
		return resp, err
	}

	apiModel := stripCopilotPrefix(req.Model)

	// Resolve reasoning effort aliases (e.g., gpt-5-high -> gpt-5 with high effort)
	var aliasEffort string
	if resolvedModel, effort, ok := resolveCopilotAlias(apiModel); ok {
		apiModel = resolvedModel
		aliasEffort = effort
	}

	translatorModel := req.Model
	if !strings.HasPrefix(strings.ToLower(req.Model), "copilot-") && strings.HasPrefix(strings.ToLower(apiModel), "gemini") {
		translatorModel = "copilot-" + apiModel
	}

	reporter := newUsageReporter(ctx, e.Identifier(), apiModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")

	requestedModel := payloadRequestedModel(opts, req.Model)
	body := sdktranslator.TranslateRequest(from, to, apiModel, bytes.Clone(req.Payload), false)
	body = applyPayloadConfigWithRoot(e.cfg, apiModel, to.String(), "", body, nil, requestedModel)
	body = sanitizeCopilotPayload(body, apiModel)
	body, _ = sjson.SetBytes(body, "stream", false)

	// Apply reasoning effort from alias if resolved
	if aliasEffort != "" {
		body = setCopilotReasoningEffort(body, aliasEffort)
		body, _ = sjson.SetBytes(body, "model", apiModel)
	}

	// Inject cached Gemini reasoning for models that require it
	if strings.HasPrefix(strings.ToLower(apiModel), "gemini") {
		body = e.reasoningCache(auth).InjectReasoning(body)
	}

	baseURL := copilotauth.CopilotBaseURL(accountType)
	url := baseURL + "/chat/completions"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return resp, err
	}

	e.applyCopilotHeaders(httpReq, copilotToken, req.Payload, opts.Headers)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0, "copilot")
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("copilot executor: close response body error: %v", errClose)
		}
	}()

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		log.Debugf("request error, error status: %d, error body: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = copilotStatusErr(httpResp.StatusCode, string(b))
		return resp, err
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)

	// Parse usage from response
	reporter.publish(ctx, parseOpenAIUsage(data))

	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, translatorModel, bytes.Clone(opts.OriginalRequest), body, data, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out)}
	return resp, nil
}

func (e *CopilotExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	copilotToken, accountType, err := e.getCopilotToken(ctx, auth)
	if err != nil {
		return nil, err
	}

	apiModel := stripCopilotPrefix(req.Model)

	// Resolve reasoning effort aliases (e.g., gpt-5-high -> gpt-5 with high effort)
	var aliasEffort string
	if resolvedModel, effort, ok := resolveCopilotAlias(apiModel); ok {
		apiModel = resolvedModel
		aliasEffort = effort
	}

	translatorModel := req.Model
	if !strings.HasPrefix(strings.ToLower(req.Model), "copilot-") && strings.HasPrefix(strings.ToLower(apiModel), "gemini") {
		translatorModel = "copilot-" + apiModel
	}

	reporter := newUsageReporter(ctx, e.Identifier(), apiModel, auth)
	defer reporter.trackFailure(ctx, &err)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")

	requestedModel := payloadRequestedModel(opts, req.Model)
	body := sdktranslator.TranslateRequest(from, to, apiModel, bytes.Clone(req.Payload), true)
	body = applyPayloadConfigWithRoot(e.cfg, apiModel, to.String(), "", body, nil, requestedModel)
	body = sanitizeCopilotPayload(body, apiModel)
	body, _ = sjson.SetBytes(body, "stream", true)

	// Apply reasoning effort from alias if resolved
	if aliasEffort != "" {
		body = setCopilotReasoningEffort(body, aliasEffort)
		body, _ = sjson.SetBytes(body, "model", apiModel)
	}

	// Inject cached Gemini reasoning for models that require it
	if strings.HasPrefix(strings.ToLower(apiModel), "gemini") {
		body = e.reasoningCache(auth).InjectReasoning(body)
	}

	baseURL := copilotauth.CopilotBaseURL(accountType)
	url := baseURL + "/chat/completions"

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	e.applyCopilotHeaders(httpReq, copilotToken, req.Payload, opts.Headers)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := newProxyAwareHTTPClient(ctx, e.cfg, auth, 0, "copilot")
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		data, readErr := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("copilot executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			recordAPIResponseError(ctx, e.cfg, readErr)
			return nil, readErr
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		log.Debugf("request error, error status: %d, error body: %s", httpResp.StatusCode, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = copilotStatusErr(httpResp.StatusCode, string(data))
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("copilot executor: close response body error: %v", errClose)
			}
		}()

		isGemini := strings.HasPrefix(strings.ToLower(apiModel), "gemini")
		scanner := bufio.NewScanner(httpResp.Body)
		bufSize := e.cfg.ScannerBufferSize
		if bufSize <= 0 {
			bufSize = 20_971_520
		}
		scanner.Buffer(nil, bufSize)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			appendAPIResponseChunk(ctx, e.cfg, line)

			// Parse usage from final chunk if present
			if bytes.HasPrefix(line, dataTag) {
				data := bytes.TrimSpace(line[5:])
				if gjson.GetBytes(data, "usage").Exists() {
					reporter.publish(ctx, parseOpenAIUsage(data))
				}

				// Cache Gemini reasoning data for subsequent requests
				if isGemini {
					e.reasoningCache(auth).CacheReasoning(data)
				}
			}

			chunks := sdktranslator.TranslateStream(ctx, to, from, translatorModel, bytes.Clone(opts.OriginalRequest), body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			recordAPIResponseError(ctx, e.cfg, errScan)
			reporter.publishFailure(ctx)
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
		}
	}()

	return stream, nil
}

func (e *CopilotExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("copilot executor: refresh called")
	if auth == nil {
		return nil, statusErr{code: 500, msg: "copilot executor: auth is nil (copilot_refresh_auth_nil)"}
	}

	var githubToken string
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["github_token"].(string); ok && v != "" {
			githubToken = v
		}
	}
	// Fallback to storage if metadata is missing github_token
	if githubToken == "" {
		if storage, ok := auth.Storage.(*copilotauth.CopilotTokenStorage); ok && storage != nil {
			githubToken = storage.GitHubToken
		}
	}

	if githubToken == "" {
		log.Debug("copilot executor: no github_token in metadata, skipping refresh")
		return auth, nil
	}

	authSvc := copilotauth.NewCopilotAuth(e.cfg)
	tokenResp, err := authSvc.GetCopilotToken(ctx, githubToken)
	if err != nil {
		// Classify error: auth issues get 401, transient issues get 503.
		code := 503
		cause := "copilot_refresh_transient"

		if httpCode := copilotauth.StatusCode(err); httpCode != 0 {
			if httpCode == 401 || httpCode == 403 {
				code = 401
				cause = "copilot_auth_rejected"
			} else if httpCode >= 500 {
				cause = "copilot_upstream_error"
			}
		}

		var authErr *copilotauth.AuthenticationError
		if errors.As(err, &authErr) {
			switch authErr.Type {
			case copilotauth.ErrNoCopilotSubscription.Type:
				code = 401
				cause = "copilot_no_subscription"
			case copilotauth.ErrAccessDenied.Type:
				code = 401
				cause = "copilot_access_denied"
			case copilotauth.ErrNoGitHubToken.Type:
				code = 401
				cause = "copilot_no_github_token"
			}
		}

		log.Warnf("copilot executor: token refresh failed [cause: %s]: %v", cause, err)
		return nil, statusErr{code: code, msg: fmt.Sprintf("copilot token refresh failed (%s): %v", cause, err)}
	}

	// Update in-memory cache
	e.tokenMu.Lock()
	e.tokenCache[githubToken] = &cachedToken{
		token:     tokenResp.Token,
		expiresAt: time.Unix(tokenResp.ExpiresAt, 0),
	}
	e.tokenMu.Unlock()

	// We no longer rely on metadata for token caching, but we update it
	// for the current session in case other components need it.
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["copilot_token"] = tokenResp.Token
	auth.Metadata["copilot_token_expiry"] = time.Unix(tokenResp.ExpiresAt, 0).Format(time.RFC3339)
	auth.Metadata["type"] = "copilot"

	log.Debug("Copilot token refreshed successfully")
	return auth, nil
}

// getCopilotToken retrieves the Copilot token from auth metadata, refreshing if needed.
// Returns statusErr with appropriate HTTP codes:
// - 500 for missing auth or metadata (internal state error, cause: copilot_auth_nil, copilot_metadata_nil)
// - 401 for missing copilot token (auth configuration error, cause: copilot_token_missing)
// This allows callers to distinguish internal state issues from auth configuration problems.
//
// Note on account_type: See sdk/auth/copilot.go for full precedence documentation.
// Attributes["account_type"] is the canonical runtime source; storage is only a fallback.
//
// Note on metadata: auth.Metadata is used as a runtime cache and may be updated from
// CopilotTokenStorage. Both are kept in sync when tokens are refreshed.
func (e *CopilotExecutor) getCopilotToken(ctx context.Context, auth *cliproxyauth.Auth) (string, copilotauth.AccountType, error) {
	if auth == nil {
		return "", "", statusErr{code: 500, msg: "copilot executor: auth is nil (copilot_auth_nil)"}
	}

	copilotauth.EnsureMetadataHydrated(auth)
	githubToken := copilotauth.ResolveGitHubToken(auth)
	accountType := copilotauth.ResolveAccountType(auth)

	// 1. Check Memory Cache
	if token, valid := e.getValidCachedToken(githubToken); valid {
		return token, accountType, nil
	}

	// 2. Check Metadata (Storage) Cache
	copilotToken, copilotExpiry, hasCopilotToken := copilotauth.ResolveCopilotToken(auth)
	if hasCopilotToken {
		if time.Now().Add(60 * time.Second).Before(copilotExpiry) {
			e.setCachedToken(githubToken, copilotToken, copilotExpiry)
			return copilotToken, accountType, nil
		}
	}

	// 3. Refresh if needed
	if githubToken != "" {
		if _, err := e.Refresh(ctx, auth); err == nil {
			if token, valid := e.getValidCachedToken(githubToken); valid {
				return token, accountType, nil
			}
		}
	}

	// 4. Fallback: Use cached token if strictly valid (not expired) but near expiry
	if hasCopilotToken && time.Now().Before(copilotExpiry) {
		return copilotToken, accountType, nil
	}

	return "", accountType, statusErr{code: 401, msg: "no valid token available"}
}

func (e *CopilotExecutor) getValidCachedToken(githubToken string) (string, bool) {
	e.tokenMu.RLock()
	defer e.tokenMu.RUnlock()
	if cached, ok := e.tokenCache[githubToken]; ok {
		if time.Now().Add(60 * time.Second).Before(cached.expiresAt) {
			return cached.token, true
		}
	}
	return "", false
}

func (e *CopilotExecutor) setCachedToken(githubToken, token string, expiresAt time.Time) {
	e.tokenMu.Lock()
	defer e.tokenMu.Unlock()
	e.tokenCache[githubToken] = &cachedToken{
		token:     token,
		expiresAt: expiresAt,
	}
}

// CountTokens provides a token count estimate for Copilot models.
//
// This method uses the Codex/OpenAI tokenizer (via tokenizerForCodexModel) as an
// approximation for Copilot models. Since Copilot routes requests to various
// underlying models (GPT, Claude, Gemini), the token counts are best-effort
// estimates rather than exact billing equivalents.
//
// If a Copilot-specific tokenizer becomes available in the future, it can be
// swapped in by replacing the tokenizerForCodexModel call below.
func (e *CopilotExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	apiModel := stripCopilotPrefix(req.Model)

	// Copilot uses OpenAI models, so we can reuse the OpenAI tokenizer logic
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	body := sdktranslator.TranslateRequest(from, to, apiModel, bytes.Clone(req.Payload), false)

	// Use tiktoken for token counting via tokenizerForCodexModel helper.
	// This provides OpenAI-compatible token estimates.
	enc, err := tokenizerForCodexModel(apiModel)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("copilot executor: tokenizer init failed: %w", err)
	}

	// Extract messages and count tokens
	var textParts []string
	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		for _, msg := range messages.Array() {
			content := msg.Get("content")
			if content.Type == gjson.String {
				textParts = append(textParts, strings.TrimSpace(content.String()))
			} else if content.IsArray() {
				for _, part := range content.Array() {
					if part.Get("type").String() == "text" {
						textParts = append(textParts, strings.TrimSpace(part.Get("text").String()))
					}
				}
			}
		}
	}

	text := strings.Join(textParts, "\n")
	count, err := enc.Count(text)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("copilot executor: token counting failed: %w", err)
	}

	usageJSON := fmt.Sprintf(`{"usage":{"input_tokens":%d,"output_tokens":0}}`, count)
	translated := sdktranslator.TranslateTokenCount(ctx, to, from, int64(count), []byte(usageJSON))
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

func getCachedCopilotModels(authID string) []*registry.ModelInfo {
	sharedModelCacheMu.Lock()
	defer sharedModelCacheMu.Unlock()
	if entry, ok := sharedModelCache[authID]; ok {
		if time.Since(entry.fetchedAt) < sharedModelCacheTTL {
			return entry.models
		}
	}
	return nil
}

func setCachedCopilotModels(authID string, models []*registry.ModelInfo) {
	sharedModelCacheMu.Lock()
	defer sharedModelCacheMu.Unlock()
	sharedModelCache[authID] = &sharedModelCacheEntry{
		fetchedAt: time.Now(),
		models:    models,
	}
}

// EvictCopilotModelCache removes cached models for an auth ID when the auth is removed.
func EvictCopilotModelCache(authID string) {
	if authID == "" {
		return
	}
	sharedModelCacheMu.Lock()
	delete(sharedModelCache, authID)
	sharedModelCacheMu.Unlock()
}

func (e *CopilotExecutor) FetchModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	// 1. Check Cache
	if models := getCachedCopilotModels(auth.ID); models != nil {
		return models
	}

	// 2. Resolve Tokens
	copilotauth.EnsureMetadataHydrated(auth)
	copilotToken, _, _ := copilotauth.ResolveCopilotToken(auth)

	// 3. Fetch (auto-refresh if 401)
	authSvc := copilotauth.NewCopilotAuth(cfg)
	var modelsResp *copilotauth.CopilotModelsResponse
	var err error

	if copilotToken != "" {
		modelsResp, err = authSvc.GetModels(ctx, copilotToken, copilotauth.ResolveAccountType(auth))
	}

	if (copilotToken == "" || err != nil) && copilotauth.ResolveGitHubToken(auth) != "" {
		// Attempt refresh
		if _, refreshErr := e.Refresh(ctx, auth); refreshErr == nil {
			copilotToken, _, _ = copilotauth.ResolveCopilotToken(auth)
			modelsResp, err = authSvc.GetModels(ctx, copilotToken, copilotauth.ResolveAccountType(auth))
		}
	}

	if err != nil || modelsResp == nil {
		log.Warnf("copilot executor: failed to fetch models for auth %s: %v", auth.ID, err)
		return nil
	}

	// 4. Process and Cache
	now := time.Now().Unix()
	models := make([]*registry.ModelInfo, 0, len(modelsResp.Data))

	for _, m := range modelsResp.Data {
		if !m.ModelPickerEnabled {
			continue
		}
		modelInfo := &registry.ModelInfo{
			ID:          m.ID,
			Name:        m.Name,
			Object:      "model",
			Created:     now,
			OwnedBy:     "copilot",
			Type:        "copilot",
			DisplayName: m.Name,
			Version:     m.Version,
		}
		if m.Capabilities.Limits.MaxContextWindowTokens > 0 {
			modelInfo.ContextLength = m.Capabilities.Limits.MaxContextWindowTokens
		}
		if m.Capabilities.Limits.MaxOutputTokens > 0 {
			modelInfo.MaxCompletionTokens = m.Capabilities.Limits.MaxOutputTokens
		}
		params := []string{"temperature", "top_p", "max_tokens", "stream"}
		if m.Capabilities.Supports.ToolCalls {
			params = append(params, "tools")
		}
		modelInfo.SupportedParameters = params
		desc := fmt.Sprintf("%s model via GitHub Copilot", m.Vendor)
		if m.Preview {
			desc += " (Preview)"
		}
		modelInfo.Description = desc
		models = append(models, modelInfo)
	}

	// 5. Merge essential models that Copilot supports but may not return in /models
	models = mergeEssentialCopilotModels(models, now)

	models = registry.GenerateCopilotAliases(models)
	setCachedCopilotModels(auth.ID, models)
	return models
}

// FetchCopilotModels retrieves available models from the Copilot API using the supplied auth.
// Uses shared cache that persists across executor instances.
func FetchCopilotModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	// Use shared cache - check before creating executor
	if models := getCachedCopilotModels(auth.ID); models != nil {
		return models
	}
	e := NewCopilotExecutor(cfg)
	return e.FetchModels(ctx, auth, cfg)
}

// copilotStatusErr creates a statusErr with appropriate retry timing for Copilot.
// For 429 errors, it sets a longer retry delay (30 seconds) since Copilot quota
// limits typically require more time to recover than standard rate limits.
func copilotStatusErr(code int, msg string) statusErr {
	err := statusErr{code: code, msg: msg}
	if code == 429 {
		delay := 30 * time.Second
		err.retryAfter = &delay
	}
	return err
}
