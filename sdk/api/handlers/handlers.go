// Package handlers provides core API handler functionality for the CLI Proxy API server.
// It includes common types, client management, load balancing, and error handling
// shared across all API endpoint handlers (OpenAI, Claude, Gemini).
package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/secretdlp"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/session"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/net/context"
)

// ErrorResponse represents a standard error response format for the API.
// It contains a single ErrorDetail field.
type ErrorResponse struct {
	// Error contains detailed information about the error that occurred.
	Error ErrorDetail `json:"error"`
}

// ErrorDetail provides specific information about an error that occurred.
// It includes a human-readable message, an error type, and an optional error code.
type ErrorDetail struct {
	// Message is a human-readable message providing more details about the error.
	Message string `json:"message"`

	// Type is the category of error that occurred (e.g., "invalid_request_error").
	Type string `json:"type"`

	// Code is a short code identifying the error, if applicable.
	Code string `json:"code,omitempty"`
}

const idempotencyKeyMetadataKey = "idempotency_key"

const (
	defaultStreamingKeepAliveSeconds = 0
	defaultStreamingBootstrapRetries = 0
	// Stream interceptor history is intentionally bounded and not configurable in the first SDK surface.
	maxStreamInterceptorHistoryChunks = 64
	maxStreamInterceptorHistoryBytes  = 1 << 20
)

// BuildErrorResponseBody builds an OpenAI-compatible JSON error response body.
// If errText is already valid JSON, it is returned as-is to preserve upstream error payloads.
func BuildErrorResponseBody(status int, errText string) []byte {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if strings.TrimSpace(errText) == "" {
		errText = http.StatusText(status)
	}

	trimmed := strings.TrimSpace(errText)
	if trimmed != "" && json.Valid([]byte(trimmed)) {
		return []byte(trimmed)
	}

	errType := "invalid_request_error"
	var code string
	switch status {
	case http.StatusUnauthorized:
		errType = "authentication_error"
		code = "invalid_api_key"
	case http.StatusForbidden:
		errType = "permission_error"
		code = "insufficient_quota"
	case http.StatusTooManyRequests:
		errType = "rate_limit_error"
		code = "rate_limit_exceeded"
	case http.StatusNotFound:
		errType = "invalid_request_error"
		code = "model_not_found"
	default:
		if status >= http.StatusInternalServerError {
			errType = "server_error"
			code = "internal_server_error"
		}
	}

	payload, err := json.Marshal(ErrorResponse{
		Error: ErrorDetail{
			Message: errText,
			Type:    errType,
			Code:    code,
		},
	})
	if err != nil {
		return []byte(fmt.Sprintf(`{"error":{"message":%q,"type":"server_error","code":"internal_server_error"}}`, errText))
	}
	return payload
}

// StreamingKeepAliveInterval returns the streaming keep-alive interval for this server (SSE heartbeats and WebSocket Ping frames).
// Returning 0 disables keep-alives (default when unset).
func StreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := defaultStreamingKeepAliveSeconds
	if cfg != nil {
		seconds = cfg.Streaming.KeepAliveSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// NonStreamingKeepAliveInterval returns the keep-alive interval for non-streaming responses.
// Returning 0 disables keep-alives (default when unset).
func NonStreamingKeepAliveInterval(cfg *config.SDKConfig) time.Duration {
	seconds := 0
	if cfg != nil {
		seconds = cfg.NonStreamKeepAliveInterval
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// StreamingBootstrapRetries returns how many times a streaming request may be retried before any bytes are sent.
func StreamingBootstrapRetries(cfg *config.SDKConfig) int {
	retries := defaultStreamingBootstrapRetries
	if cfg != nil {
		retries = cfg.Streaming.BootstrapRetries
	}
	if retries < 0 {
		retries = 0
	}
	return retries
}

// PassthroughHeadersEnabled returns whether upstream response headers should be forwarded to clients.
// Default is false.
func PassthroughHeadersEnabled(cfg *config.SDKConfig) bool {
	return cfg != nil && cfg.PassthroughHeaders
}

func requestExecutionMetadata(ctx context.Context) map[string]any {
	// Idempotency-Key is an optional client-supplied header used to correlate retries.
	// Only include it if the client explicitly provides it.
	key := ""
	requestPath := ""
	var ginCtx *gin.Context
	if ctx != nil {
		if requestGinCtx, ok := ctx.Value("gin").(*gin.Context); ok && requestGinCtx != nil && requestGinCtx.Request != nil {
			ginCtx = requestGinCtx
			key = strings.TrimSpace(ginCtx.GetHeader("Idempotency-Key"))
			requestPath = strings.TrimSpace(ginCtx.FullPath())
			if requestPath == "" && ginCtx.Request.URL != nil {
				requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
			}
		}
	}

	meta := make(map[string]any)
	if key != "" {
		meta[idempotencyKeyMetadataKey] = key
	}
	if requestPath != "" {
		meta[coreexecutor.RequestPathMetadataKey] = requestPath
	}
	if pinnedAuthID := pinnedAuthIDFromContext(ctx); pinnedAuthID != "" {
		meta[coreexecutor.PinnedAuthMetadataKey] = pinnedAuthID
	}
	if selectedCallback := selectedAuthIDCallbackFromContext(ctx); selectedCallback != nil {
		meta[coreexecutor.SelectedAuthCallbackMetadataKey] = selectedCallback
	}
	if ginCtx != nil && !websocket.IsWebSocketUpgrade(ginCtx.Request) {
		if traceCallback := logging.GinCPATraceIDCallback(ginCtx); traceCallback != nil {
			meta[coreexecutor.SelectedAuthIndexCallbackMetadataKey] = traceCallback
		}
	}
	if executionSessionID := executionSessionIDFromContext(ctx); executionSessionID != "" {
		meta[coreexecutor.ExecutionSessionMetadataKey] = executionSessionID
	}
	if callerScope := requestCallerScope(ginCtx); callerScope != "" {
		meta[coreexecutor.CallerScopeMetadataKey] = callerScope
	}
	if disallowFreeAuthFromContext(ctx) {
		meta[coreexecutor.DisallowFreeAuthMetadataKey] = true
	}
	return meta
}

func requestClientIP(request *http.Request) string {
	if request == nil {
		return ""
	}
	remoteAddr := strings.TrimSpace(request.RemoteAddr)
	if host, _, errSplit := net.SplitHostPort(remoteAddr); errSplit == nil {
		return strings.TrimSpace(host)
	}
	return remoteAddr
}

func extractSessionIDsFromRequest(request *http.Request) (string, string) {
	if request == nil || request.Header == nil {
		return "", ""
	}
	if info, ok := coresession.ExtractSessionInfo(request.Header, nil, nil); ok {
		return info.SessionID, info.ParentSessionID
	}
	return "", ""
}

// EnrichContextWithSessionHierarchy extracts canonical session and parent session identities
// from headers, payload, and metadata and records them in ClientRequestMetadata.
func EnrichContextWithSessionHierarchy(ctx context.Context, headers http.Header, payload []byte, metadata map[string]any) context.Context {
	meta := logging.GetClientRequestMetadata(ctx)
	if info, ok := coresession.ExtractSessionInfo(headers, payload, metadata); ok {
		meta.SessionID = info.SessionID
		meta.ParentSessionID = info.ParentSessionID
		if meta.SessionID != "" && meta.SessionID == meta.ParentSessionID {
			meta.ParentSessionID = ""
		}
		return logging.WithClientRequestMetadata(ctx, meta)
	}
	if meta.SessionID != "" || meta.ParentSessionID != "" {
		meta.SessionID = ""
		meta.ParentSessionID = ""
		return logging.WithClientRequestMetadata(ctx, meta)
	}
	return ctx
}

func enrichContextWithSessionHierarchy(ctx context.Context, headers http.Header, payload []byte, metadata map[string]any) context.Context {
	return EnrichContextWithSessionHierarchy(ctx, headers, payload, metadata)
}

func requestCallerScope(ginCtx *gin.Context) string {
	if ginCtx == nil {
		return ""
	}
	value, exists := ginCtx.Get("userApiKey")
	if !exists || value == nil {
		return ""
	}
	return coresession.CallerScope(fmt.Sprint(value))
}

func addAuthSelectionModelMetadata(meta map[string]any, model string) {
	if meta == nil {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	meta[coreexecutor.AuthSelectionModelMetadataKey] = model
}

func setReasoningEffortMetadata(meta map[string]any, handlerType, model string, rawJSON []byte) {
	if meta == nil {
		return
	}
	effort := thinking.ExtractReasoningEffort(rawJSON, handlerType, model)
	if effort == "" {
		return
	}
	meta[coreexecutor.ReasoningEffortMetadataKey] = effort
}

func setServiceTierMetadata(meta map[string]any, rawJSON []byte) {
	if meta == nil {
		return
	}
	serviceTier := coreusage.AutoServiceTier
	node := gjson.GetBytes(rawJSON, "service_tier")
	if node.Exists() {
		value := strings.TrimSpace(node.String())
		if value != "" {
			serviceTier = value
		}
	}
	meta[coreexecutor.ServiceTierMetadataKey] = serviceTier
}

func setGenerateMetadata(meta map[string]any, rawJSON []byte) {
	if meta == nil {
		return
	}
	// Missing or true means generation is enabled; only an explicit false disables generation.
	generate := true
	node := gjson.GetBytes(rawJSON, "generate")
	if node.Exists() && node.IsBool() && !node.Bool() {
		generate = false
	}
	meta[coreexecutor.GenerateMetadataKey] = generate
}

// BaseAPIHandler contains the handlers for API endpoints.
// It holds a pool of clients to interact with the backend service and manages
// load balancing, client selection, and configuration.
type BaseAPIHandler struct {
	// AuthManager manages auth lifecycle and execution in the new architecture.
	AuthManager *coreauth.Manager

	// Cfg holds the current application configuration.
	Cfg *config.SDKConfig
	cfg atomic.Pointer[config.SDKConfig]

	// PluginHost optionally applies plugin interceptors around upstream execution.
	PluginHost PluginInterceptorHost

	// ModelRouterHost optionally routes matching requests to a plugin executor, the router's own
	// executor, or a built-in provider before model-to-provider resolution and auth selection.
	ModelRouterHost PluginModelRouterHost

	// SecretDLP restores hosted egress-token-vault placeholders before responses reach downstream clients.
	SecretDLP *secretdlp.Service
}

// NewBaseAPIHandlers creates a new API handlers instance.
// It takes a slice of clients and configuration as input.
//
// Parameters:
//   - cliClients: A slice of AI service clients
//   - cfg: The application configuration
//
// Returns:
//   - *BaseAPIHandler: A new API handlers instance
func NewBaseAPIHandlers(cfg *config.SDKConfig, authManager *coreauth.Manager) *BaseAPIHandler {
	h := &BaseAPIHandler{
		Cfg:         cfg,
		AuthManager: authManager,
	}
	h.cfg.Store(cfg)
	return h
}

// UpdateClients updates the handlers' client list and configuration.
// This method is called when the configuration or authentication tokens change.
//
// Parameters:
//   - clients: The new slice of AI service clients
//   - cfg: The new application configuration
func (h *BaseAPIHandler) UpdateClients(cfg *config.SDKConfig) {
	if h == nil {
		return
	}
	h.Cfg = cfg
	h.cfg.Store(cfg)
}

// CurrentConfig returns the latest configuration snapshot published by reload.
func (h *BaseAPIHandler) CurrentConfig() *config.SDKConfig {
	if h == nil {
		return nil
	}
	if cfg := h.cfg.Load(); cfg != nil {
		return cfg
	}
	return h.Cfg
}

// SetPluginHost configures the optional plugin interceptor host.
func (h *BaseAPIHandler) SetPluginHost(host PluginInterceptorHost) {
	if h == nil {
		return
	}
	if isNilPluginInterceptorHost(host) {
		h.PluginHost = nil
		return
	}
	h.PluginHost = host
}

// SetModelRouterHost configures the optional plugin model router host.
func (h *BaseAPIHandler) SetModelRouterHost(host PluginModelRouterHost) {
	if h == nil {
		return
	}
	if isNilPluginModelRouterHost(host) {
		h.ModelRouterHost = nil
		return
	}
	h.ModelRouterHost = host
}

func (h *BaseAPIHandler) SetSecretDLP(svc *secretdlp.Service) {
	if h == nil {
		return
	}
	h.SecretDLP = svc
}

func (h *BaseAPIHandler) restoreSecretDLPResponse(ctx context.Context, body []byte) []byte {
	if h == nil || h.SecretDLP == nil {
		return body
	}
	return h.SecretDLP.RestoreResponse(ctx, body)
}

func (h *BaseAPIHandler) restoreSecretDLPStreamChunk(ctx context.Context, body []byte) []byte {
	if h == nil || h.SecretDLP == nil {
		return body
	}
	return h.SecretDLP.RestoreStreamChunk(ctx, body)
}

func (h *BaseAPIHandler) flushSecretDLPStream(ctx context.Context) []byte {
	if h == nil || h.SecretDLP == nil {
		return nil
	}
	return h.SecretDLP.FlushStream(ctx)
}

func isNilPluginInterceptorHost(host PluginInterceptorHost) bool {
	return isNilInterface(host)
}

func isNilPluginModelRouterHost(host PluginModelRouterHost) bool {
	return isNilInterface(host)
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	// A typed nil pointer stored in an interface is not equal to nil.
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// GetAlt extracts the 'alt' parameter from the request query string.
// It checks both 'alt' and '$alt' parameters and returns the appropriate value.
//
// Parameters:
//   - c: The Gin context containing the HTTP request
//
// Returns:
//   - string: The alt parameter value, or empty string if it's "sse"
func (h *BaseAPIHandler) GetAlt(c *gin.Context) string {
	var alt string
	var hasAlt bool
	alt, hasAlt = c.GetQuery("alt")
	if !hasAlt {
		alt, _ = c.GetQuery("$alt")
	}
	if alt == "sse" {
		return ""
	}
	return alt
}

// GetContextWithCancel creates a new context with cancellation capabilities.
// It embeds the Gin context and the API handler into the new context for later use.
// The returned cancel function also handles logging the API response if request logging is enabled.
//
// Parameters:
//   - handler: The API handler associated with the request.
//   - c: The Gin context of the current request.
//   - ctx: The parent context (caller values/deadlines are preserved; request context adds cancellation and request ID).
//
// Returns:
//   - context.Context: The new context with cancellation and embedded values.
//   - APIHandlerCancelFunc: A function to cancel the context and log the response.
func (h *BaseAPIHandler) GetContextWithCancel(handler interfaces.APIHandler, c *gin.Context, ctx context.Context) (context.Context, APIHandlerCancelFunc) {
	parentCtx := ctx
	if parentCtx == nil {
		parentCtx = context.Background()
	}

	var requestCtx context.Context
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}

	if requestCtx != nil && logging.GetRequestID(parentCtx) == "" {
		if requestID := logging.GetRequestID(requestCtx); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		} else if requestID = logging.GetGinRequestID(c); requestID != "" {
			parentCtx = logging.WithRequestID(parentCtx, requestID)
		}
	}
	newCtx, cancel := context.WithCancel(parentCtx)

	endpoint := ""
	if c != nil && c.Request != nil {
		path := strings.TrimSpace(c.FullPath())
		if path == "" && c.Request.URL != nil {
			path = strings.TrimSpace(c.Request.URL.Path)
		}
		if path != "" {
			method := strings.TrimSpace(c.Request.Method)
			if method != "" {
				endpoint = method + " " + path
			} else {
				endpoint = path
			}
		}
	}
	if endpoint != "" {
		newCtx = logging.WithEndpoint(newCtx, endpoint)
	}
	if c != nil && c.Request != nil {
		sessionID, parentSessionID := extractSessionIDsFromRequest(c.Request)
		newCtx = logging.WithClientRequestMetadata(newCtx, logging.ClientRequestMetadata{
			ClientIP:        requestClientIP(c.Request),
			XForwardedFor:   strings.TrimSpace(strings.Join(c.Request.Header.Values("X-Forwarded-For"), ", ")),
			UserAgent:       strings.TrimSpace(c.Request.UserAgent()),
			SessionID:       sessionID,
			ParentSessionID: parentSessionID,
		})
	}
	newCtx = logging.WithResponseStatusHolder(newCtx)
	newCtx = logging.WithResponseHeadersHolder(newCtx)

	cancelCtx := newCtx
	if requestCtx != nil && requestCtx != parentCtx {
		go func() {
			select {
			case <-requestCtx.Done():
				cancel()
			case <-cancelCtx.Done():
			}
		}()
	}
	newCtx = context.WithValue(newCtx, "gin", c)
	newCtx = context.WithValue(newCtx, "handler", handler)
	return newCtx, func(params ...interface{}) {
		if c != nil {
			logging.SetResponseStatus(cancelCtx, c.Writer.Status())
		}
		if cfg := h.CurrentConfig(); cfg != nil && cfg.RequestLog && len(params) == 1 {
			if captured, exists := c.Get(logging.APIResponseCapturedContextKey); exists {
				if capturedBool, ok := captured.(bool); ok && capturedBool {
					cancel()
					return
				}
			}
			if existing, exists := c.Get("API_RESPONSE"); exists {
				if existingBytes, ok := existing.([]byte); ok && len(bytes.TrimSpace(existingBytes)) > 0 {
					switch params[0].(type) {
					case error, string:
						cancel()
						return
					}
				}
			}

			var payload []byte
			switch data := params[0].(type) {
			case []byte:
				payload = data
			case error:
				if data != nil {
					payload = []byte(data.Error())
				}
			case string:
				payload = []byte(data)
			}
			if len(payload) > 0 {
				if existing, exists := c.Get("API_RESPONSE"); exists {
					if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
						trimmedPayload := bytes.TrimSpace(payload)
						if len(trimmedPayload) > 0 && bytes.Contains(existingBytes, trimmedPayload) {
							cancel()
							return
						}
					}
				}
				appendAPIResponse(c, payload)
			}
		}

		cancel()
	}
}

// StartNonStreamingKeepAlive intentionally does not emit pre-response bytes.
// A non-streaming heartbeat would commit HTTP 200 before the upstream outcome is known.
func (h *BaseAPIHandler) StartNonStreamingKeepAlive(c *gin.Context, ctx context.Context) func() {
	return func() {}
}

// appendAPIResponse preserves any previously captured API response and appends new data.
func appendAPIResponse(c *gin.Context, data []byte) {
	if c == nil || len(data) == 0 {
		return
	}

	// Capture timestamp on first API response
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); !exists {
		c.Set("API_RESPONSE_TIMESTAMP", time.Now())
	}

	if existing, exists := c.Get("API_RESPONSE"); exists {
		if existingBytes, ok := existing.([]byte); ok && len(existingBytes) > 0 {
			combined := make([]byte, 0, len(existingBytes)+len(data)+1)
			combined = append(combined, existingBytes...)
			if existingBytes[len(existingBytes)-1] != '\n' {
				combined = append(combined, '\n')
			}
			combined = append(combined, data...)
			c.Set("API_RESPONSE", combined)
			return
		}
	}

	c.Set("API_RESPONSE", bytes.Clone(data))
}

// Branch-owned helpers retained in handlers.go after the upstream split of this
// file. Execution, routing, stream, error, and interceptor logic now lives in
// handlers_execution.go, handlers_routing.go, handlers_stream.go,
// handlers_errors.go, handlers_interceptors.go, handlers_context.go, and
// handlers_invocation.go; the symbols below have no counterpart there.

func (h *BaseAPIHandler) secretDLPAfterAuthEnabled() bool {
	return h != nil && h.SecretDLP != nil && h.SecretDLP.Enabled()
}

func (h *BaseAPIHandler) applySecretDLPAfterAuth(ctx context.Context, req coreexecutor.RequestAfterAuthInterceptRequest, resp coreexecutor.RequestAfterAuthInterceptResponse) coreexecutor.RequestAfterAuthInterceptResponse {
	if h == nil || h.SecretDLP == nil || !h.SecretDLP.Enabled() || resp.Err != nil {
		return resp
	}
	if !h.SecretDLP.ProviderRedactionEnabled(req.Provider, req.SecretRedactionPolicy) {
		return resp
	}
	body := req.Body
	if len(resp.Body) > 0 {
		body = resp.Body
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return resp
	}
	path := ""
	if req.Metadata != nil {
		if raw, ok := req.Metadata[coreexecutor.RequestPathMetadataKey]; ok {
			switch v := raw.(type) {
			case string:
				path = strings.TrimSpace(v)
			case []byte:
				path = strings.TrimSpace(string(v))
			}
		}
	}
	redacted, session, err := h.SecretDLP.RedactPayload(ctx, path, body)
	if err != nil {
		resp.Err = err
		return resp
	}
	if session == nil && bytes.Equal(redacted, body) {
		return resp
	}
	resp.Body = cloneBytes(redacted)
	return resp
}

func cloneRequestHeaders(ctx context.Context) http.Header {
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil || ginCtx.Request.Header == nil {
		return nil
	}
	return ginCtx.Request.Header.Clone()
}

func cloneMetadata(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func replaceHeader(dst http.Header, src http.Header) {
	for key := range dst {
		delete(dst, key)
	}
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

type streamInterceptorRequestBase struct {
	sourceFormat    string
	model           string
	requestedModel  string
	requestHeaders  http.Header
	originalRequest []byte
	requestBody     []byte
	metadata        map[string]any
}

func newStreamInterceptorRequestBase(sourceFormat, model, requestedModel string, requestHeaders http.Header, originalRequest, requestBody []byte, metadata map[string]any) streamInterceptorRequestBase {
	return streamInterceptorRequestBase{
		sourceFormat:    sourceFormat,
		model:           model,
		requestedModel:  requestedModel,
		requestHeaders:  cloneHeader(requestHeaders),
		originalRequest: cloneBytes(originalRequest),
		requestBody:     cloneBytes(requestBody),
		metadata:        metadata,
	}
}

func (b streamInterceptorRequestBase) request(responseHeaders http.Header, body []byte, historyChunks [][]byte, chunkIndex int) pluginapi.StreamChunkInterceptRequest {
	return pluginapi.StreamChunkInterceptRequest{
		SourceFormat:    b.sourceFormat,
		Model:           b.model,
		RequestedModel:  b.requestedModel,
		RequestHeaders:  cloneHeader(b.requestHeaders),
		ResponseHeaders: cloneHeader(responseHeaders),
		OriginalRequest: b.originalRequest,
		RequestBody:     b.requestBody,
		Body:            body,
		HistoryChunks:   snapshotStreamInterceptorHistory(historyChunks),
		ChunkIndex:      chunkIndex,
		Metadata:        b.metadata,
	}
}

func snapshotStreamInterceptorHistory(history [][]byte) [][]byte {
	if len(history) == 0 {
		return nil
	}
	out := make([][]byte, len(history))
	copy(out, history)
	return out
}

// TemperatureSuffixMetadataKey is re-exported from the executor package for convenience.
const TemperatureSuffixMetadataKey = coreexecutor.TemperatureSuffixMetadataKey

// temperatureSuffixRe matches the -temp-<number> suffix in a model name,
// which must appear before any thinking suffix, e.g. model-temp-0.7(16384).
var temperatureSuffixRe = regexp.MustCompile(`-temp-(\d+(?:\.\d+)?)$`)

func parseTemperatureSuffix(model string) (cleanModel string, temperature float64, hasTemp bool) {
	// Separate the thinking suffix (...) from the model name if present.
	thinkingSuffix := ""
	base := model
	if idx := strings.LastIndex(model, "("); idx != -1 && strings.HasSuffix(model, ")") {
		base = model[:idx]
		thinkingSuffix = model[idx:]
	}

	loc := temperatureSuffixRe.FindStringSubmatchIndex(base)
	if loc == nil {
		return model, 0, false
	}

	// Extract the numeric value.
	valueStr := base[loc[2]:loc[3]]
	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return model, 0, false
	}

	// Strip the -temp-x.x suffix from the base model name and re-attach thinking suffix.
	cleanBase := base[:loc[0]]
	if cleanBase == "" {
		return model, 0, false
	}

	log.WithFields(log.Fields{
		"model":       model,
		"temperature": value,
		"clean_model": cleanBase + thinkingSuffix,
	}).Info("temperature: parsed -temp- suffix from model name |")

	return cleanBase + thinkingSuffix, value, true
}

// APIHandlerCancelFunc is a function type for canceling an API handler's context.
// It can optionally accept parameters, which are used for logging the response.
type APIHandlerCancelFunc func(params ...interface{})
