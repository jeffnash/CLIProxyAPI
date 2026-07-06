package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const (
	managedProviderModelsPath          = "/models"
	managedProviderClaudeMessagesPath  = "/messages"
	managedProviderOpenAIChatPath      = "/chat/completions"
	managedProviderTransportAuto       = "auto"
	managedProviderTransportClaude     = "claude"
	managedProviderTransportOpenAI     = "openai"
	managedProviderTransportResponses  = "openai-response"
	managedProviderDefaultMaxRetries   = 4
	managedProviderDefaultDisplayLabel = "Managed"
)

var managedProviderBackoffSchedule = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

var managedProviderRetryableStatusCodes = map[int]bool{
	429: true,
	502: true,
	503: true,
	504: true,
}

var (
	managedProviderModelCacheMu sync.Mutex
	managedProviderModelCache   = make(map[string]*managedProviderModelCacheEntry)

	managedProviderAliasMapMu sync.RWMutex
	managedProviderAliasMap   = make(map[string]map[string]string)
)

type managedProviderModelCacheEntry struct {
	models    []*registry.ModelInfo
	aliases   map[string]string
	fetchedAt time.Time
	ttl       time.Duration
}

type managedProviderRemoteModel struct {
	ID                        string   `json:"id"`
	Object                    string   `json:"object"`
	Created                   int64    `json:"created"`
	OwnedBy                   string   `json:"owned_by"`
	ContextLength             int      `json:"context_length"`
	MaxCompletionTokens       int      `json:"max_completion_tokens"`
	SupportedParameters       []string `json:"supported_parameters"`
	SupportedInputModalities  []string `json:"supportedInputModalities"`
	SupportedOutputModalities []string `json:"supportedOutputModalities"`
}

type ManagedProviderExecutor struct {
	provider string
	cfg      *config.Config
}

func NewManagedProviderExecutor(provider string, cfg *config.Config) *ManagedProviderExecutor {
	provider = strings.ToLower(strings.TrimSpace(provider))
	return &ManagedProviderExecutor{provider: provider, cfg: cfg}
}

func (e *ManagedProviderExecutor) Identifier() string {
	if e == nil || e.provider == "" {
		return "managed-provider"
	}
	return e.provider
}

func (e *ManagedProviderExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	creds := e.creds(auth)
	if creds.apiKey == "" {
		return resp, fmt.Errorf("%s executor: missing api key", e.Identifier())
	}

	transport := e.selectTransport(auth, opts)
	prepared, err := e.prepareRequestBody(ctx, auth, req, opts, transport, false)
	if err != nil {
		return resp, err
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, prepared.targetFormat.String())

	endpoint := e.endpointURL(creds, transport)
	if endpoint == "" {
		return resp, fmt.Errorf("%s executor: missing upstream endpoint for transport %s", e.Identifier(), transport)
	}
	attempts := e.maxRetries(auth) + 1
	if attempts < 1 {
		attempts = 1
	}

	var data []byte
	var headers http.Header
	for attempt := 0; attempt < attempts; attempt++ {
		start := time.Now()
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(prepared.body))
		if errReq != nil {
			return resp, errReq
		}
		e.applyHeaders(httpReq, auth, creds.apiKey, transport, false)
		e.recordRequest(ctx, auth, endpoint, prepared.body, httpReq.Header)

		httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0, e.Identifier())
		httpResp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errDo)
			if attempt < attempts-1 {
				if errWait := e.waitForRetry(ctx, auth, attempt, prepared.baseModel, 0); errWait != nil {
					return resp, errWait
				}
				continue
			}
			return resp, errDo
		}

		headers = httpResp.Header.Clone()
		helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, headers)
		data, _ = io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("%s executor: close response body error: %v", e.Identifier(), errClose)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)

		if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
			log.WithFields(log.Fields{
				"provider":  e.Identifier(),
				"attempt":   attempt + 1,
				"status":    httpResp.StatusCode,
				"duration":  time.Since(start).String(),
				"transport": transport,
			}).Info("managed provider: upstream ok")
			break
		}

		log.WithFields(log.Fields{
			"provider":  e.Identifier(),
			"attempt":   attempt + 1,
			"status":    httpResp.StatusCode,
			"duration":  time.Since(start).String(),
			"transport": transport,
			"body":      sanitizeResponseBody(data),
		}).Info("managed provider: upstream non-2xx")

		if managedProviderRetryableStatusCodes[httpResp.StatusCode] && attempt < attempts-1 {
			if errWait := e.waitForRetry(ctx, auth, attempt, prepared.baseModel, httpResp.StatusCode); errWait != nil {
				return resp, errWait
			}
			continue
		}
		return resp, statusErr{code: httpResp.StatusCode, msg: string(data)}
	}

	if len(data) == 0 {
		data = []byte("{}")
	}
	if prepared.targetFormat == sdktranslator.FormatClaude {
		reporter.Publish(ctx, helps.ParseClaudeUsage(data))
	} else {
		reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	}
	reporter.EnsurePublished(ctx)

	out := e.translateNonStream(ctx, prepared, req, opts, data)
	return cliproxyexecutor.Response{Payload: out, Headers: headers}, nil
}

func (e *ManagedProviderExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	creds := e.creds(auth)
	if creds.apiKey == "" {
		return nil, fmt.Errorf("%s executor: missing api key", e.Identifier())
	}

	transport := e.selectTransport(auth, opts)
	prepared, err := e.prepareRequestBody(ctx, auth, req, opts, transport, true)
	if err != nil {
		return nil, err
	}

	reporter := helps.NewUsageReporter(ctx, e.Identifier(), prepared.baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	reporter.SetTranslatedReasoningEffort(prepared.body, prepared.targetFormat.String())

	endpoint := e.endpointURL(creds, transport)
	if endpoint == "" {
		return nil, fmt.Errorf("%s executor: missing upstream endpoint for transport %s", e.Identifier(), transport)
	}
	attempts := e.maxRetries(auth) + 1
	if attempts < 1 {
		attempts = 1
	}

	var httpResp *http.Response
	for attempt := 0; attempt < attempts; attempt++ {
		httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(prepared.body))
		if errReq != nil {
			return nil, errReq
		}
		e.applyHeaders(httpReq, auth, creds.apiKey, transport, true)
		e.recordRequest(ctx, auth, endpoint, prepared.body, httpReq.Header)

		httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0, e.Identifier())
		resp, errDo := httpClient.Do(httpReq)
		if errDo != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errDo)
			if attempt < attempts-1 {
				if errWait := e.waitForRetry(ctx, auth, attempt, prepared.baseModel, 0); errWait != nil {
					return nil, errWait
				}
				continue
			}
			return nil, errDo
		}

		helps.RecordAPIResponseMetadata(ctx, e.cfg, resp.StatusCode, resp.Header.Clone())
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			httpResp = resp
			break
		}

		data, _ := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("%s executor: close response body error: %v", e.Identifier(), errClose)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		if managedProviderRetryableStatusCodes[resp.StatusCode] && attempt < attempts-1 {
			if errWait := e.waitForRetry(ctx, auth, attempt, prepared.baseModel, resp.StatusCode); errWait != nil {
				return nil, errWait
			}
			continue
		}
		return nil, statusErr{code: resp.StatusCode, msg: string(data)}
	}
	if httpResp == nil {
		return nil, fmt.Errorf("%s executor: missing response", e.Identifier())
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("%s executor: close response body error: %v", e.Identifier(), errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			if prepared.targetFormat == sdktranslator.FormatClaude {
				if detail, ok := helps.ParseClaudeStreamUsage(line); ok {
					reporter.Publish(ctx, detail)
				}
			} else if detail, ok := helps.ParseOpenAIStreamUsage(line); ok {
				reporter.Publish(ctx, detail)
			}
			for _, chunk := range e.translateStreamChunk(ctx, prepared, req, opts, bytes.Clone(line), &param) {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
			return
		}
		if prepared.responseFormat != prepared.targetFormat && prepared.targetFormat == sdktranslator.FormatOpenAI {
			done := []byte("data: [DONE]")
			for _, chunk := range e.translateStreamChunk(ctx, prepared, req, opts, done, &param) {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
				case <-ctx.Done():
					return
				}
			}
		}
		reporter.EnsurePublished(ctx)
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *ManagedProviderExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

func (e *ManagedProviderExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	transport := e.selectTransport(auth, opts)
	prepared, err := e.prepareRequestBody(ctx, auth, req, opts, transport, false)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	if prepared.targetFormat == sdktranslator.FormatClaude {
		enc, errTok := helps.GetTokenizer(req.Model)
		if errTok != nil {
			return cliproxyexecutor.Response{}, fmt.Errorf("%s executor: tokenizer init failed: %w", e.Identifier(), errTok)
		}
		count, errCount := helps.CountClaudeChatTokens(enc, prepared.body)
		if errCount != nil {
			return cliproxyexecutor.Response{}, fmt.Errorf("%s executor: token counting failed: %w", e.Identifier(), errCount)
		}
		usageJSON := []byte(fmt.Sprintf(`{"input_tokens":%d}`, count))
		translated := sdktranslator.TranslateTokenCount(ctx, prepared.targetFormat, prepared.responseFormat, count, usageJSON)
		return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
	}

	enc, errTok := helps.TokenizerForModel(req.Model)
	if errTok != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("%s executor: tokenizer init failed: %w", e.Identifier(), errTok)
	}
	count, errCount := helps.CountOpenAIChatTokens(enc, prepared.body)
	if errCount != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("%s executor: token counting failed: %w", e.Identifier(), errCount)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translated := sdktranslator.TranslateTokenCount(ctx, prepared.targetFormat, prepared.responseFormat, count, usageJSON)
	return cliproxyexecutor.Response{Payload: []byte(translated)}, nil
}

func (e *ManagedProviderExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("%s executor: request is nil", e.Identifier())
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	creds := e.creds(auth)
	if creds.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+creds.apiKey)
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0, e.Identifier())
	return httpClient.Do(httpReq)
}

func (e *ManagedProviderExecutor) FetchModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	if cfg != nil && e.cfg == nil {
		e.cfg = cfg
	}
	creds := e.creds(auth)
	ttl := config.ManagedProviderModelCacheTTL(creds.provider)
	cacheKey := e.Identifier()

	managedProviderModelCacheMu.Lock()
	if cached := managedProviderModelCache[cacheKey]; cached != nil && time.Since(cached.fetchedAt) < cached.ttl {
		models := cached.models
		restoreManagedProviderAliasMap(cacheKey, cached.aliases)
		managedProviderModelCacheMu.Unlock()
		return models
	}
	managedProviderModelCacheMu.Unlock()

	if creds.apiKey == "" || !config.ManagedProviderDiscoveryEnabled(creds.provider) {
		return e.fallbackModels(creds)
	}

	endpoint := e.modelsEndpointURL(creds)
	if endpoint == "" {
		return e.fallbackModels(creds)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		log.Warnf("%s: failed to create models request: %v", e.Identifier(), err)
		return e.fallbackModels(creds)
	}
	req.Header.Set("Authorization", "Bearer "+creds.apiKey)
	util.ApplyCustomHeadersFromAttrs(req, attrsFromAuth(auth))

	httpClient := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 15*time.Second, e.Identifier())
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Warnf("%s: failed to fetch models: %v", e.Identifier(), err)
		return e.fallbackModels(creds)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warnf("%s: /models returned status %d", e.Identifier(), resp.StatusCode)
		return e.fallbackModels(creds)
	}

	var listResp struct {
		Data []managedProviderRemoteModel `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		log.Warnf("%s: failed to decode models response: %v", e.Identifier(), err)
		return e.fallbackModels(creds)
	}

	models, aliases := processManagedProviderModels(e.Identifier(), creds.prefix, listResp.Data, creds.models, creds.modelsExclude)
	if len(models) == 0 {
		return e.fallbackModels(creds)
	}
	models = registry.GenerateManagedProviderAliases(models, creds.prefix, creds.label)

	managedProviderModelCacheMu.Lock()
	managedProviderModelCache[cacheKey] = &managedProviderModelCacheEntry{
		models:    models,
		aliases:   aliases,
		fetchedAt: time.Now(),
		ttl:       ttl,
	}
	managedProviderModelCacheMu.Unlock()
	restoreManagedProviderAliasMap(cacheKey, aliases)

	return models
}

func (e *ManagedProviderExecutor) RequestToFormat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) sdktranslator.Format {
	return managedProviderTransportFormat(e.selectTransport(nil, opts))
}

type managedProviderPreparedRequest struct {
	baseModel      string
	body           []byte
	targetFormat   sdktranslator.Format
	responseFormat sdktranslator.Format
}

type managedProviderCredentials struct {
	provider           config.ManagedProviderConfig
	apiKey             string
	baseURL            string
	claudeBaseURL      string
	openAIBaseURL      string
	claudePath         string
	openAIChatPath     string
	openAIResponsePath string
	modelsPath         string
	prefix             string
	label              string
	models             []string
	modelsExclude      []string
	fallbackModels     []string
}

func (e *ManagedProviderExecutor) prepareRequestBody(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, transport string, stream bool) (managedProviderPreparedRequest, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	creds := e.creds(auth)
	apiModel := resolveManagedProviderModel(e.Identifier(), creds.prefix, baseModel)
	from := opts.SourceFormat
	target := managedProviderTransportFormat(transport)
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)

	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}

	var originalTranslated []byte
	var body []byte
	if from == target {
		body = bytes.Clone(req.Payload)
		originalTranslated = originalTranslatedForPayloadConfig(e.cfg, originalPayloadSource, req.Payload, body, bytes.Clone)
	} else {
		originalTranslated, body = translateRequestPairForPayloadConfig(e.cfg, from, target, baseModel, originalPayloadSource, req.Payload, stream)
	}
	body, _ = sjson.SetBytes(body, "model", apiModel)

	var err error
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), target.String(), e.Identifier())
	if err != nil {
		return managedProviderPreparedRequest{}, err
	}
	body = helps.ApplyTemperatureSuffix(body, req.Model, opts, target.String())
	if target == sdktranslator.FormatOpenAI {
		body = helps.RepairMissingReasoningContentForToolCalls(auth, body)
		if stream {
			body, _ = sjson.SetBytes(body, "stream_options.include_usage", true)
		}
	} else if target == sdktranslator.FormatClaude {
		body = ensureModelMaxTokens(body, baseModel)
	}

	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, target.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)

	return managedProviderPreparedRequest{
		baseModel:      baseModel,
		body:           body,
		targetFormat:   target,
		responseFormat: responseFormat,
	}, nil
}

func (e *ManagedProviderExecutor) selectTransport(auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) string {
	creds := e.creds(auth)
	mode := managedProviderTransportAuto
	defaultTransport := managedProviderTransportClaude
	if creds.provider.TransportMode != "" {
		mode = creds.provider.TransportMode
	}
	if creds.provider.DefaultTransport != "" {
		defaultTransport = normalizeManagedProviderTransport(creds.provider.DefaultTransport)
	}
	if auth != nil && auth.Attributes != nil {
		if v := normalizeManagedProviderTransport(auth.Attributes["transport_mode"]); v != "" {
			mode = v
		}
		if v := normalizeManagedProviderTransport(auth.Attributes["default_transport"]); v != "" {
			defaultTransport = v
		}
	}
	switch normalizeManagedProviderTransport(mode) {
	case managedProviderTransportClaude, managedProviderTransportOpenAI, managedProviderTransportResponses:
		return normalizeManagedProviderTransport(mode)
	}
	switch opts.SourceFormat {
	case sdktranslator.FormatOpenAIResponse:
		if strings.TrimSpace(creds.openAIResponsePath) != "" {
			return managedProviderTransportResponses
		}
		if creds.openAIBaseURL != "" {
			return managedProviderTransportOpenAI
		}
		return managedProviderTransportClaude
	case sdktranslator.FormatOpenAI:
		if creds.openAIBaseURL != "" {
			return managedProviderTransportOpenAI
		}
		return managedProviderTransportClaude
	case sdktranslator.FormatClaude:
		if creds.claudeBaseURL != "" {
			return managedProviderTransportClaude
		}
		return managedProviderTransportOpenAI
	default:
		if defaultTransport == managedProviderTransportOpenAI && creds.openAIBaseURL != "" {
			return managedProviderTransportOpenAI
		}
		if defaultTransport == managedProviderTransportResponses && strings.TrimSpace(creds.openAIResponsePath) != "" {
			return managedProviderTransportResponses
		}
		return managedProviderTransportClaude
	}
}

func (e *ManagedProviderExecutor) endpointURL(creds managedProviderCredentials, transport string) string {
	switch normalizeManagedProviderTransport(transport) {
	case managedProviderTransportOpenAI:
		return joinManagedProviderURL(creds.openAIBaseURL, creds.openAIChatPath)
	case managedProviderTransportResponses:
		return joinManagedProviderURL(creds.openAIBaseURL, creds.openAIResponsePath)
	default:
		return joinManagedProviderURL(creds.claudeBaseURL, creds.claudePath)
	}
}

func (e *ManagedProviderExecutor) modelsEndpointURL(creds managedProviderCredentials) string {
	if endpoint := strings.TrimSpace(creds.provider.ModelDiscovery.URL); endpoint != "" {
		return endpoint
	}
	base := creds.openAIBaseURL
	if base == "" {
		base = creds.baseURL
	}
	return joinManagedProviderURL(base, creds.modelsPath)
}

func (e *ManagedProviderExecutor) translateNonStream(ctx context.Context, prepared managedProviderPreparedRequest, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, data []byte) []byte {
	if prepared.responseFormat == prepared.targetFormat {
		return data
	}
	var param any
	return sdktranslator.TranslateNonStream(ctx, prepared.targetFormat, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, data, &param)
}

func (e *ManagedProviderExecutor) translateStreamChunk(ctx context.Context, prepared managedProviderPreparedRequest, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, line []byte, param *any) [][]byte {
	if prepared.responseFormat == prepared.targetFormat {
		return [][]byte{append(bytes.Clone(line), '\n')}
	}
	return sdktranslator.TranslateStream(ctx, prepared.targetFormat, prepared.responseFormat, req.Model, opts.OriginalRequest, prepared.body, line, param)
}

func (e *ManagedProviderExecutor) recordRequest(ctx context.Context, auth *cliproxyauth.Auth, endpoint string, body []byte, headers http.Header) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       endpoint,
		Method:    http.MethodPost,
		Headers:   headers.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

func (e *ManagedProviderExecutor) applyHeaders(r *http.Request, auth *cliproxyauth.Auth, apiKey, transport string, stream bool) {
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+apiKey)
	r.Header.Set("User-Agent", "cli-proxy-"+e.Identifier())
	if managedProviderTransportFormat(transport) == sdktranslator.FormatClaude {
		r.Header.Set("x-api-key", apiKey)
		r.Header.Set("anthropic-version", "2023-06-01")
	}
	if stream {
		r.Header.Set("Accept", "text/event-stream")
		r.Header.Set("Cache-Control", "no-cache")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	util.ApplyCustomHeadersFromAttrs(r, attrsFromAuth(auth))
}

func processManagedProviderModels(providerName, prefix string, raw []managedProviderRemoteModel, whitelist, blocklist []string) ([]*registry.ModelInfo, map[string]string) {
	whitelistSet := managedProviderFilterSet(whitelist, prefix)
	blocklistSet := managedProviderFilterSet(blocklist, prefix)
	aliases := make(map[string]string)

	models := make([]*registry.ModelInfo, 0, len(raw))
	for _, m := range raw {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if len(whitelistSet) > 0 && !whitelistSet[id] && !whitelistSet[prefix+id] {
			continue
		}
		if blocklistSet[id] || blocklistSet[prefix+id] {
			continue
		}
		created := m.Created
		if created == 0 {
			created = time.Now().Unix()
		}
		ownedBy := strings.TrimSpace(m.OwnedBy)
		if ownedBy == "" {
			ownedBy = providerName
		}
		params := m.SupportedParameters
		if len(params) == 0 {
			params = []string{"temperature", "top_p", "max_tokens", "stream", "tools"}
		}
		info := &registry.ModelInfo{
			ID:                        id,
			Object:                    "model",
			Created:                   created,
			OwnedBy:                   ownedBy,
			Type:                      providerName,
			DisplayName:               id,
			Description:               "via " + providerName + " API",
			ContextLength:             m.ContextLength,
			MaxCompletionTokens:       m.MaxCompletionTokens,
			SupportedParameters:       params,
			SupportedInputModalities:  m.SupportedInputModalities,
			SupportedOutputModalities: m.SupportedOutputModalities,
			UpstreamID:                id,
		}
		aliases[id] = id
		if prefix != "" {
			aliases[prefix+id] = id
		}
		models = append(models, info)
	}
	return models, aliases
}

func managedProviderFilterSet(values []string, prefix string) map[string]bool {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values)*2)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		out[value] = true
		if prefix != "" {
			out[strings.TrimPrefix(value, prefix)] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func resolveManagedProviderModel(providerName, prefix, modelID string) string {
	stripped := modelID
	if prefix != "" {
		stripped = strings.TrimPrefix(stripped, prefix)
	}

	managedProviderAliasMapMu.RLock()
	if aliases := managedProviderAliasMap[providerName]; len(aliases) > 0 {
		if apiID, ok := aliases[stripped]; ok {
			managedProviderAliasMapMu.RUnlock()
			return apiID
		}
		if apiID, ok := aliases[modelID]; ok {
			managedProviderAliasMapMu.RUnlock()
			return apiID
		}
	}
	managedProviderAliasMapMu.RUnlock()

	if info := registry.LookupModelInfo(stripped, providerName); info != nil && strings.TrimSpace(info.UpstreamID) != "" {
		return strings.TrimSpace(info.UpstreamID)
	}
	return stripped
}

func restoreManagedProviderAliasMap(providerName string, aliases map[string]string) {
	if len(aliases) == 0 {
		return
	}
	managedProviderAliasMapMu.Lock()
	defer managedProviderAliasMapMu.Unlock()
	managedProviderAliasMap[providerName] = cloneStringMap(aliases)
}

func EvictManagedProviderModelCache(providerName string) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	managedProviderModelCacheMu.Lock()
	defer managedProviderModelCacheMu.Unlock()
	if providerName == "" {
		managedProviderModelCache = make(map[string]*managedProviderModelCacheEntry)
		return
	}
	delete(managedProviderModelCache, providerName)
}

func (e *ManagedProviderExecutor) creds(auth *cliproxyauth.Auth) managedProviderCredentials {
	provider, _ := config.FindManagedProvider(e.sdkConfig(), e.Identifier())
	if provider.Name == "" {
		provider.Name = e.Identifier()
	}
	provider = config.NormalizeManagedProviders([]config.ManagedProviderConfig{provider})[0]

	attrs := attrsFromAuth(auth)
	out := managedProviderCredentials{
		provider:           provider,
		apiKey:             strings.TrimSpace(attrs["api_key"]),
		baseURL:            strings.TrimRight(strings.TrimSpace(attrs["base_url"]), "/"),
		claudeBaseURL:      strings.TrimRight(firstNonEmpty(attrs["claude_base_url"], attrs["anthropic_base_url"]), "/"),
		openAIBaseURL:      strings.TrimRight(strings.TrimSpace(attrs["openai_base_url"]), "/"),
		claudePath:         firstNonEmpty(attrs["claude_messages_path"], attrs["anthropic_messages_path"]),
		openAIChatPath:     strings.TrimSpace(attrs["openai_chat_path"]),
		openAIResponsePath: strings.TrimSpace(attrs["openai_responses_path"]),
		modelsPath:         strings.TrimSpace(attrs["models_path"]),
		prefix:             firstNonEmpty(attrs["prefix"], config.ManagedProviderPrefix(provider)),
		label:              firstNonEmpty(attrs["label"], provider.Name),
		models:             parseManagedProviderListAttr(attrs["models_json"], provider.Models),
		modelsExclude:      parseManagedProviderListAttr(attrs["models_exclude_json"], provider.ModelsExclude),
		fallbackModels:     parseManagedProviderListAttr(attrs["fallback_models_json"], provider.FallbackModels),
	}
	if out.apiKey == "" {
		out.apiKey = strings.TrimSpace(provider.APIKey)
	}
	if out.apiKey == "" && provider.APIKeyEnv != "" {
		out.apiKey = strings.TrimSpace(os.Getenv(provider.APIKeyEnv))
	}
	if out.baseURL == "" {
		out.baseURL = provider.BaseURL
	}
	if out.claudeBaseURL == "" {
		out.claudeBaseURL = firstNonEmpty(provider.ClaudeBaseURL, provider.AnthropicBaseURL, out.baseURL)
	}
	if out.openAIBaseURL == "" {
		out.openAIBaseURL = firstNonEmpty(provider.OpenAIBaseURL, out.baseURL)
	}
	if out.claudePath == "" {
		out.claudePath = firstNonEmpty(provider.ClaudeMessagesPath, provider.AnthropicMessagesPath, managedProviderClaudeMessagesPath)
	}
	if out.openAIChatPath == "" {
		out.openAIChatPath = firstNonEmpty(provider.OpenAIChatPath, managedProviderOpenAIChatPath)
	}
	if out.openAIResponsePath == "" {
		out.openAIResponsePath = provider.OpenAIResponsesPath
	}
	if out.modelsPath == "" {
		out.modelsPath = firstNonEmpty(provider.ModelDiscovery.Path, managedProviderModelsPath)
	}
	if out.label == "" {
		out.label = managedProviderDefaultDisplayLabel
	}
	out.claudePath = normalizeManagedProviderPath(out.claudePath)
	out.openAIChatPath = normalizeManagedProviderPath(out.openAIChatPath)
	out.openAIResponsePath = normalizeManagedProviderPath(out.openAIResponsePath)
	out.modelsPath = normalizeManagedProviderPath(out.modelsPath)
	return out
}

func (e *ManagedProviderExecutor) sdkConfig() *config.SDKConfig {
	if e == nil || e.cfg == nil {
		return nil
	}
	return &e.cfg.SDKConfig
}

func (e *ManagedProviderExecutor) fallbackModels(creds managedProviderCredentials) []*registry.ModelInfo {
	models := registry.GetManagedProviderFallbackModels(e.Identifier(), creds.prefix, creds.label, creds.fallbackModels)
	if len(models) == 0 {
		return nil
	}
	aliases := make(map[string]string)
	for _, model := range models {
		if model == nil {
			continue
		}
		upstream := strings.TrimSpace(model.UpstreamID)
		if upstream == "" {
			upstream = strings.TrimPrefix(model.ID, creds.prefix)
		}
		aliases[model.ID] = upstream
	}
	restoreManagedProviderAliasMap(e.Identifier(), aliases)
	return models
}

func (e *ManagedProviderExecutor) maxRetries(auth *cliproxyauth.Auth) int {
	if auth != nil && auth.Attributes != nil {
		if raw := strings.TrimSpace(auth.Attributes["max_retries"]); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err == nil {
				if parsed < 0 {
					return 0
				}
				return parsed
			}
		}
	}
	provider, ok := config.FindManagedProvider(e.sdkConfig(), e.Identifier())
	if !ok || provider.MaxRetries == nil {
		return managedProviderDefaultMaxRetries
	}
	if *provider.MaxRetries < 0 {
		return 0
	}
	return *provider.MaxRetries
}

func (e *ManagedProviderExecutor) backoffDuration(auth *cliproxyauth.Auth, attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	schedule := e.backoffSchedule(auth)
	if attempt >= len(schedule) {
		return schedule[len(schedule)-1]
	}
	return schedule[attempt]
}

func (e *ManagedProviderExecutor) backoffSchedule(auth *cliproxyauth.Auth) []time.Duration {
	raw := ""
	if auth != nil && auth.Attributes != nil {
		raw = strings.TrimSpace(auth.Attributes["retry_backoff"])
	}
	if raw == "" {
		if provider, ok := config.FindManagedProvider(e.sdkConfig(), e.Identifier()); ok {
			raw = strings.TrimSpace(provider.RetryBackoff)
		}
	}
	if raw == "" {
		return managedProviderBackoffSchedule
	}
	parts := strings.Split(raw, ",")
	schedule := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		seconds, err := strconv.Atoi(trimmed)
		if err != nil || seconds < 0 {
			log.WithField("value", trimmed).Warnf("%s: invalid backoff value, using default schedule", e.Identifier())
			return managedProviderBackoffSchedule
		}
		schedule = append(schedule, time.Duration(seconds)*time.Second)
	}
	if len(schedule) == 0 {
		return managedProviderBackoffSchedule
	}
	return schedule
}

func (e *ManagedProviderExecutor) waitForRetry(ctx context.Context, auth *cliproxyauth.Auth, attempt int, model string, statusCode int) error {
	backoff := e.backoffDuration(auth, attempt)
	log.WithFields(log.Fields{
		"provider":    e.Identifier(),
		"attempt":     attempt + 1,
		"backoff":     backoff.String(),
		"model":       model,
		"status_code": statusCode,
	}).Info("managed provider: retrying after backoff")
	return waitForDuration(ctx, backoff)
}

func managedProviderTransportFormat(transport string) sdktranslator.Format {
	switch normalizeManagedProviderTransport(transport) {
	case managedProviderTransportOpenAI:
		return sdktranslator.FormatOpenAI
	case managedProviderTransportResponses:
		return sdktranslator.FormatOpenAIResponse
	default:
		return sdktranslator.FormatClaude
	}
}

func normalizeManagedProviderTransport(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "anthropic":
		return managedProviderTransportClaude
	case "openai", "openai-chat", "chat":
		return managedProviderTransportOpenAI
	case "responses", "openai-response", "openai-responses":
		return managedProviderTransportResponses
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func joinManagedProviderURL(baseURL, path string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	path = normalizeManagedProviderPath(path)
	if baseURL == "" || path == "" {
		return ""
	}
	return baseURL + path
}

func normalizeManagedProviderPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

func attrsFromAuth(auth *cliproxyauth.Auth) map[string]string {
	if auth == nil || len(auth.Attributes) == 0 {
		return nil
	}
	return auth.Attributes
}

func parseManagedProviderListAttr(raw string, fallback []string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]string(nil), fallback...)
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err == nil {
		return trimStringList(values)
	}
	return trimStringList(strings.Split(raw, ","))
}

func trimStringList(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
