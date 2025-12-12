package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	grokauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/grok"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	grokchat "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/grok/openai/chat-completions"
	groktranslator "github.com/router-for-me/CLIProxyAPI/v6/internal/translator/openai/grok"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// GrokExecutor is a stateless executor for Grok (X.AI) using SSO cookies.
// It leverages a Chrome TLS-fingerprinted HTTP client for Cloudflare bypass.
type GrokExecutor struct {
	cfg        *config.Config
	httpClient *grokauth.GrokHTTPClient
	auth       *grokauth.GrokAuth
}

// NewGrokExecutor constructs a Grok executor with a shared HTTP client.
func NewGrokExecutor(cfg *config.Config) *GrokExecutor {
	return &GrokExecutor{
		cfg:        cfg,
		httpClient: grokauth.NewGrokHTTPClient(cfg, ""),
		auth:       grokauth.NewGrokAuth(cfg),
	}
}

// Identifier implements cliproxy executor identification.
func (e *GrokExecutor) Identifier() string { return "grok" }

// PrepareRequest is a no-op hook for now.
func (e *GrokExecutor) PrepareRequest(_ *http.Request, _ *cliproxyauth.Auth) error { return nil }

func (e *GrokExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ssoToken, cfClearance, proxyURL := grokCreds(auth)
	if strings.TrimSpace(ssoToken) == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "grok executor: missing sso token"}
		return resp, err
	}

	var storage *grokauth.GrokTokenStorage
	if auth != nil && auth.Storage != nil {
		if st, storageErr := e.getGrokStorage(auth); storageErr != nil {
			err = storageErr
			return resp, err
		} else {
			storage = st
		}
	}

	reporter := newUsageReporter(ctx, e.Identifier(), req.Model, auth)
	defer reporter.trackFailure(ctx, &err)

	ctx = grokchat.WithGrokConfig(ctx, e.cfg)
	maskedToken := grokauth.MaskToken(ssoToken)

	client := e.httpClient
	if proxyURL != "" {
		client = grokauth.NewGrokHTTPClient(e.cfg, proxyURL)
	}

	body, headerOpts, buildErr := e.buildGrokPayload(ctx, req, opts, false, ssoToken, cfClearance, client)
	if buildErr != nil {
		err = buildErr
		return resp, err
	}

	headers := grokauth.BuildHeaders(e.cfg, ssoToken, cfClearance, headerOpts)
	httpHeaders := http.Header{}
	for k, v := range headers {
		httpHeaders.Set(k, v)
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       grokauth.GrokAPIEndpoint,
		Method:    http.MethodPost,
		Headers:   httpHeaders.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpResp, err := client.Post(ctx, grokauth.GrokAPIEndpoint, headers, body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if httpResp != nil && httpResp.Body != nil {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("grok executor: close response body error: %v", errClose)
			}
		}
	}()

	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		appendAPIResponseChunk(ctx, e.cfg, b)
		log.Debugf("grok executor: request error, status: %d, token=%s, body: %s", httpResp.StatusCode, maskedToken, summarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if storage != nil {
			err = e.handleError(httpResp.StatusCode, b, storage, maskedToken)
		} else {
			err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		}
		return resp, err
	}

	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	appendAPIResponseChunk(ctx, e.cfg, data)
	if storage != nil {
		storage.FailedCount = 0
	}

	finalPayload, err := e.extractFinalJSONLine(data)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}

	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FromString("grok"), opts.SourceFormat, req.Model, bytes.Clone(opts.OriginalRequest), body, finalPayload, &param)
	resp = cliproxyexecutor.Response{Payload: []byte(out)}
	reporter.ensurePublished(ctx)
	reporter.publish(ctx, usage.Detail{})
	if storage != nil && e.auth != nil {
		if _, rateErr := e.auth.CheckRateLimits(ctx, storage, req.Model); rateErr != nil {
			log.Debugf("grok executor: rate-limit refresh failed for token=%s: %v", maskedToken, rateErr)
		}
	}
	return resp, nil
}

func (e *GrokExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (stream <-chan cliproxyexecutor.StreamChunk, err error) {
	ssoToken, cfClearance, proxyURL := grokCreds(auth)
	if strings.TrimSpace(ssoToken) == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "grok executor: missing sso token"}
		return nil, err
	}

	var storage *grokauth.GrokTokenStorage
	if auth != nil && auth.Storage != nil {
		if st, storageErr := e.getGrokStorage(auth); storageErr != nil {
			err = storageErr
			return nil, err
		} else {
			storage = st
		}
	}

	reporter := newUsageReporter(ctx, e.Identifier(), req.Model, auth)
	defer reporter.trackFailure(ctx, &err)

	ctx = grokchat.WithGrokConfig(ctx, e.cfg)
	maskedToken := grokauth.MaskToken(ssoToken)

	client := e.httpClient
	if proxyURL != "" {
		client = grokauth.NewGrokHTTPClient(e.cfg, proxyURL)
	}

	body, headerOpts, buildErr := e.buildGrokPayload(ctx, req, opts, true, ssoToken, cfClearance, client)
	if buildErr != nil {
		err = buildErr
		return nil, err
	}

	headers := grokauth.BuildHeaders(e.cfg, ssoToken, cfClearance, headerOpts)
	httpHeaders := http.Header{}
	for k, v := range headers {
		httpHeaders.Set(k, v)
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	recordAPIRequest(ctx, e.cfg, upstreamRequestLog{
		URL:       grokauth.GrokAPIEndpoint,
		Method:    http.MethodPost,
		Headers:   httpHeaders.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	streamCtx, cancel := context.WithCancel(ctx)
	httpResp, err := client.PostStream(streamCtx, grokauth.GrokAPIEndpoint, headers, body)
	if err != nil {
		recordAPIResponseError(ctx, e.cfg, err)
		cancel()
		return nil, err
	}
	recordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		var data []byte
		if httpResp.Body != nil {
			data, _ = io.ReadAll(httpResp.Body)
			_ = httpResp.Body.Close()
		}
		appendAPIResponseChunk(ctx, e.cfg, data)
		log.Debugf("grok executor: streaming request error, status: %d, token=%s, body: %s", httpResp.StatusCode, maskedToken, summarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		if storage != nil {
			err = e.handleError(httpResp.StatusCode, data, storage, maskedToken)
		} else {
			err = statusErr{code: httpResp.StatusCode, msg: string(data)}
		}
		cancel()
		return nil, err
	}
	if storage != nil {
		storage.FailedCount = 0
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	stream = out
	go func() {
		defer close(out)
		defer cancel()
		defer func() {
			if httpResp.Body != nil {
				if errClose := httpResp.Body.Close(); errClose != nil {
					log.Errorf("grok executor: close streaming response body error: %v", errClose)
				}
			}
		}()
		scanner := bufio.NewScanner(httpResp.Body)
		bufSize := e.cfg.ScannerBufferSize
		if bufSize <= 0 {
			bufSize = 20_971_520
		}
		scanner.Buffer(nil, bufSize)

		timeoutTracker := newStreamTimeoutTracker(e.cfg)
		timeoutCh := timeoutTracker.Start(streamCtx)

		var param any
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			timeoutTracker.MarkChunk()
			appendAPIResponseChunk(ctx, e.cfg, line)
			chunks := sdktranslator.TranslateStream(streamCtx, sdktranslator.FromString("grok"), opts.SourceFormat, req.Model, bytes.Clone(opts.OriginalRequest), body, bytes.Clone(line), &param)
			for i := range chunks {
				out <- cliproxyexecutor.StreamChunk{Payload: []byte(chunks[i])}
			}
		}
		timeoutReason := timeoutTracker.Reason(timeoutCh)
		doneChunks := sdktranslator.TranslateStream(streamCtx, sdktranslator.FromString("grok"), opts.SourceFormat, req.Model, bytes.Clone(opts.OriginalRequest), body, []byte("[DONE]"), &param)
		if timeoutReason != "" {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(buildTimeoutStreamChunk(req.Model, timeoutReason))}
		}
		for i := range doneChunks {
			out <- cliproxyexecutor.StreamChunk{Payload: []byte(doneChunks[i])}
		}
		if errScan := scanner.Err(); errScan != nil {
			if timeoutReason == "" {
				recordAPIResponseError(ctx, e.cfg, errScan)
				reporter.publishFailure(ctx)
				out <- cliproxyexecutor.StreamChunk{Err: errScan}
				return
			}
		}
		reporter.ensurePublished(ctx)
		if storage != nil && e.auth != nil {
			if _, rateErr := e.auth.CheckRateLimits(ctx, storage, req.Model); rateErr != nil {
				log.Debugf("grok executor: rate-limit refresh failed for token=%s: %v", maskedToken, rateErr)
			}
		}
	}()
	return stream, nil
}

func (e *GrokExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("grok executor: refresh called")
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "grok executor: auth is nil"}
	}

	if storage, ok := auth.Storage.(*grokauth.GrokTokenStorage); ok && storage != nil {
		if storage.Status == "expired" {
			return nil, statusErr{code: http.StatusUnauthorized, msg: "grok session expired - re-authenticate required"}
		}
	}

	return auth, nil
}

func (e *GrokExecutor) FetchModels(_ context.Context, _ *cliproxyauth.Auth, _ *config.Config) []*registry.ModelInfo {
	return grokauth.GetGrokModels()
}

// CountTokens is not supported for Grok; return a clear error.
func (e *GrokExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "grok count_tokens not supported"}
}

func (e *GrokExecutor) buildGrokPayload(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool, ssoToken, cfClearance string, client *grokauth.GrokHTTPClient) ([]byte, grokauth.HeaderOptions, error) {
	headerOpts := grokauth.HeaderOptions{}

	if grokauth.IsVideoModel(req.Model) {
		body, referer, err := groktranslator.BuildGrokVideoPayload(ctx, client, e.cfg, ssoToken, cfClearance, req.Model, bytes.Clone(req.Payload))
		if err != nil {
			return nil, headerOpts, err
		}
		headerOpts.Referer = referer
		return body, headerOpts, nil
	}

	from := opts.SourceFormat
	to := sdktranslator.FromString("grok")
	body := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), stream)
	apiModel := stripGrokPrefix(req.Model)
	if apiModel == "" {
		apiModel = req.Model
	}
	body, _ = sjson.SetBytes(body, "model", apiModel)
	body = e.applyGrokConfigToPayload(body)

	return body, headerOpts, nil
}

func (e *GrokExecutor) applyGrokConfigToPayload(body []byte) []byte {
	if e.cfg == nil {
		return body
	}
	body, _ = sjson.SetBytes(body, "temporary", e.cfg.Grok.TemporaryValue())
	return body
}

func grokCreds(a *cliproxyauth.Auth) (ssoToken, cfClearance, proxyURL string) {
	if a == nil {
		return "", "", ""
	}
	if a.Attributes != nil {
		ssoToken = a.Attributes["sso_token"]
		cfClearance = a.Attributes["cf_clearance"]
		proxyURL = a.Attributes["proxy_url"]
	}
	if ssoToken == "" && a.Metadata != nil {
		if v, ok := a.Metadata["sso_token"].(string); ok {
			ssoToken = v
		}
		if v, ok := a.Metadata["cf_clearance"].(string); ok {
			cfClearance = v
		}
	}
	if ssoToken == "" && a.Storage != nil {
		if storage, ok := a.Storage.(*grokauth.GrokTokenStorage); ok {
			ssoToken = storage.SSOToken
			cfClearance = storage.CFClearance
		}
	}
	ssoToken = grokauth.NormalizeSSOToken(ssoToken)
	cfClearance = strings.TrimSpace(cfClearance)
	return
}

func (e *GrokExecutor) getGrokStorage(auth *cliproxyauth.Auth) (*grokauth.GrokTokenStorage, error) {
	if auth == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "grok executor: auth is nil"}
	}
	storage, ok := auth.Storage.(*grokauth.GrokTokenStorage)
	if !ok || storage == nil {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "grok executor: invalid storage type"}
	}
	if storage.Status == "expired" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "grok executor: token expired"}
	}
	return storage, nil
}

func stripGrokPrefix(model string) string {
	return strings.TrimPrefix(model, "grok-")
}

func buildTimeoutStreamChunk(model, reason string) string {
	chunk := `{"id":"","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop"}]}`
	chunk, _ = sjson.Set(chunk, "model", model)
	chunk, _ = sjson.Set(chunk, "created", time.Now().Unix())
	chunk, _ = sjson.Set(chunk, "choices.0.delta.content", reason)
	chunk, _ = sjson.Set(chunk, "choices.0.finish_reason", "stop")
	return "data: " + chunk + "\n\n"
}

type streamTimeoutTracker struct {
	cfg           *config.Config
	start         time.Time
	last          time.Time
	firstReceived bool
	mu            sync.Mutex
}

func newStreamTimeoutTracker(cfg *config.Config) *streamTimeoutTracker {
	now := time.Now()
	return &streamTimeoutTracker{
		cfg:   cfg,
		start: now,
		last:  now,
	}
}

func (t *streamTimeoutTracker) MarkChunk() {
	t.mu.Lock()
	t.last = time.Now()
	t.firstReceived = true
	t.mu.Unlock()
}

func (t *streamTimeoutTracker) Start(ctx context.Context) <-chan string {
	reasonCh := make(chan string, 1)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		defer close(reasonCh)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if reason := t.check(); reason != "" {
					select {
					case reasonCh <- reason:
					default:
					}
					return
				}
			}
		}
	}()
	return reasonCh
}

func (t *streamTimeoutTracker) Reason(ch <-chan string) string {
	select {
	case reason := <-ch:
		return reason
	default:
		return ""
	}
}

func (t *streamTimeoutTracker) check() string {
	if t.cfg == nil {
		return ""
	}

	firstTimeout := time.Duration(t.cfg.Grok.StreamFirstChunkTimeoutSeconds) * time.Second
	chunkTimeout := time.Duration(t.cfg.Grok.StreamChunkTimeoutSeconds) * time.Second
	totalTimeout := time.Duration(t.cfg.Grok.StreamTotalTimeoutSeconds) * time.Second

	t.mu.Lock()
	first := t.firstReceived
	last := t.last
	start := t.start
	t.mu.Unlock()

	now := time.Now()
	if !first && firstTimeout > 0 && now.Sub(start) > firstTimeout {
		return fmt.Sprintf("stream timed out waiting for first chunk after %d seconds", t.cfg.Grok.StreamFirstChunkTimeoutSeconds)
	}
	if totalTimeout > 0 && now.Sub(start) > totalTimeout {
		return fmt.Sprintf("stream timed out after %d seconds", t.cfg.Grok.StreamTotalTimeoutSeconds)
	}
	if first && chunkTimeout > 0 && now.Sub(last) > chunkTimeout {
		return fmt.Sprintf("stream idle for %d seconds", t.cfg.Grok.StreamChunkTimeoutSeconds)
	}
	return ""
}

func (e *GrokExecutor) handleError(statusCode int, body []byte, storage *grokauth.GrokTokenStorage, maskedToken string) error {
	switch statusCode {
	case http.StatusForbidden:
		return statusErr{code: http.StatusForbidden, msg: "Cloudflare blocked request - change IP, configure cf_clearance, or use a proxy"}
	case http.StatusUnauthorized:
		if storage != nil {
			storage.FailedCount++
			if storage.FailedCount >= grokauth.MaxFailures {
				storage.Status = "expired"
			}
		}
		log.Warnf("grok executor: authentication failed for token=%s (failed_count=%d)", maskedToken, storageFailedCount(storage))
		return statusErr{code: http.StatusUnauthorized, msg: "Grok authentication failed - SSO token may be expired"}
	case http.StatusTooManyRequests:
		delay := 30 * time.Second
		return statusErr{code: http.StatusTooManyRequests, msg: "Grok rate limited", retryAfter: &delay}
	default:
		return statusErr{code: statusCode, msg: string(body)}
	}
}

func storageFailedCount(storage *grokauth.GrokTokenStorage) int {
	if storage == nil {
		return 0
	}
	return storage.FailedCount
}

func (e *GrokExecutor) extractFinalJSONLine(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	bufSize := e.cfg.ScannerBufferSize
	if bufSize <= 0 {
		bufSize = 20_971_520
	}
	scanner.Buffer(nil, bufSize)

	var lastLine []byte
	builder := strings.Builder{}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		lastLine = append(lastLine[:0], line...)
		if token := gjson.GetBytes(line, "result.response.token").String(); token != "" {
			builder.WriteString(token)
			continue
		}
		if msg := gjson.GetBytes(line, "result.response.modelResponse.message").String(); msg != "" {
			builder.Reset()
			builder.WriteString(msg)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lastLine) == 0 {
		return bytes.TrimSpace(data), nil
	}
	if msg := builder.String(); msg != "" && !gjson.GetBytes(lastLine, "result.response.modelResponse.message").Exists() {
		if updated, err := sjson.SetBytes(lastLine, "result.response.modelResponse.message", msg); err == nil {
			lastLine = updated
		}
	}
	return lastLine, nil
}
