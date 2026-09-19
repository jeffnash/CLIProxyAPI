package executor

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// CodeBuddyExecutor executes HY4 inference through Tencent's SSE chat API.
type CodeBuddyExecutor struct {
	cfg *config.Config
}

var _ cliproxyauth.ProviderExecutor = (*CodeBuddyExecutor)(nil)

// NewCodeBuddyExecutor creates a native CodeBuddy executor.
func NewCodeBuddyExecutor(cfg *config.Config) *CodeBuddyExecutor {
	return &CodeBuddyExecutor{cfg: cfg}
}

// Identifier returns the provider key.
func (e *CodeBuddyExecutor) Identifier() string { return codebuddy.Provider }

// Execute performs a non-streaming request. Upstream always streams; the
// proxy aggregates exactly one SSE response and translates it once.
func (e *CodeBuddyExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	prepared, err := helps.PrepareCodeBuddyRequest(ctx, e.cfg, auth, req, opts)
	if err != nil {
		return resp, err
	}
	reporter.SetTranslatedReasoningEffort(prepared.Body, e.Identifier())
	httpClient := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	client := codebuddy.NewClient(httpClient)
	recordCodeBuddyRequest(ctx, e.cfg, auth, prepared.Credentials.Realm, prepared.Body)
	httpResp, err := client.OpenChat(ctx, prepared.Credentials, prepared.Body, codeBuddyUpstreamHeaders(auth, opts))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, codebuddy.AuthEnvelopeLimit+1))
		helps.AppendAPIResponseChunk(ctx, e.cfg, body)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		err = helps.ClassifyCodeBuddyError(httpResp.StatusCode, httpResp.Header, body, time.Now())
		return resp, err
	}
	stream := helps.NewCodeBuddyStream(httpResp.Body, prepared.Model.ID)
	aggregate, err := helps.AggregateCodeBuddyStream(stream)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, helps.WrapCodeBuddyStreamError(err)
	}
	helps.RecordCodeBuddyReasoningContent(auth, aggregate)
	helps.AppendAPIResponseChunk(ctx, e.cfg, aggregate)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(aggregate))
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FromString("openai"), responseFormat, req.Model, opts.OriginalRequest, prepared.Body, aggregate, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{
		Payload:  out,
		Headers:  httpResp.Header.Clone(),
		Metadata: codeBuddyIdentityMetadata(req.Model, prepared, stream),
	}
	return resp, nil
}

// ExecuteStream performs a streaming request. The first actual chunk is
// pre-read and validated before the StreamResult is returned; later failures
// surface through StreamChunk.Err without a second upstream attempt.
func (e *CodeBuddyExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)
	prepared, err := helps.PrepareCodeBuddyRequest(ctx, e.cfg, auth, req, opts)
	if err != nil {
		return nil, err
	}
	reporter.SetTranslatedReasoningEffort(prepared.Body, e.Identifier())
	httpClient := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0))
	client := codebuddy.NewClient(httpClient)
	recordCodeBuddyRequest(ctx, e.cfg, auth, prepared.Credentials.Realm, prepared.Body)
	httpResp, err := client.OpenChat(ctx, prepared.Credentials, prepared.Body, codeBuddyUpstreamHeaders(auth, opts))
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(httpResp.Body, codebuddy.AuthEnvelopeLimit+1))
		helps.AppendAPIResponseChunk(ctx, e.cfg, body)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy executor: close response body error: %v", errClose)
		}
		err = helps.ClassifyCodeBuddyError(httpResp.StatusCode, httpResp.Header, body, time.Now())
		return nil, err
	}
	stream := helps.NewCodeBuddyStream(httpResp.Body, prepared.Model.ID)
	first, err := stream.Next()
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codebuddy executor: close response body error: %v", errClose)
		}
		return nil, helps.WrapCodeBuddyStreamError(err)
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	recorder := helps.NewCodeBuddyReasoningStreamRecorder(auth)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codebuddy executor: close response body error: %v", errClose)
			}
		}()
		claudeState := helps.NewClaudeInputTokenState(from, to, responseFormat, opts.OriginalRequest)
		var param any
		var streamUsage helps.StreamUsageBuffer
		defer streamUsage.Publish(ctx, reporter)
		send := func(payload []byte) bool {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		emit := func(chunk []byte) bool {
			helps.AppendAPIResponseChunk(ctx, e.cfg, chunk)
			if gjson.GetBytes(chunk, "usage").Exists() {
				streamUsage.Observe(helps.ParseOpenAIUsage(chunk), true)
			}
			framed := append([]byte("data: "), chunk...)
			if recorder != nil && (bytes.Contains(chunk, []byte("reasoning_content")) || bytes.Contains(chunk, []byte("tool_calls"))) {
				recorder.Observe(framed)
			}
			for _, translated := range helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, prepared.Body, framed, &param, claudeState) {
				if !send(bytes.Clone(translated)) {
					return false
				}
			}
			return true
		}
		if !emit(first) {
			return
		}
		for {
			next, errNext := stream.Next()
			if errNext == io.EOF {
				break
			}
			if errNext != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errNext)
				reporter.PublishFailure(ctx, errNext)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errNext}:
				case <-ctx.Done():
				}
				return
			}
			if !emit(next) {
				return
			}
		}
		for _, translated := range helps.TranslateStreamWithClaudeInputTokens(ctx, to, responseFormat, req.Model, opts.OriginalRequest, prepared.Body, []byte("[DONE]"), &param, claudeState) {
			if !send(bytes.Clone(translated)) {
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// Refresh rotates tokens through the provider refresh endpoint and returns a
// clone carrying only replaced token lifecycle metadata. The catalog snapshot
// is preserved untouched; the conductor owns persistence.
func (e *CodeBuddyExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	if auth == nil {
		return nil, &helps.CodeBuddyError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "codebuddy refresh requires an auth record", CredentialScoped: true}
	}
	clone := auth.Clone()
	credentials, err := codebuddy.CredentialsFromMetadata(clone.Metadata)
	if err != nil {
		return nil, &helps.CodeBuddyError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "invalid codebuddy credential", CredentialScoped: true}
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, clone, 0)
	updated, err := codebuddy.NewClient(httpClient).Refresh(ctx, credentials)
	if err != nil {
		return nil, &helps.CodeBuddyError{Status: http.StatusUnauthorized, Code: "refresh_failed", Message: err.Error(), CredentialScoped: true}
	}
	clone.Metadata = codebuddy.UpdatedTokenMetadata(clone.Metadata, updated, time.Now())
	return clone, nil
}

// CountTokens reports that no exact HY4 tokenizer is available. It never
// issues inference or labels a guessed tokenizer exact.
func (e *CodeBuddyExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, &helps.CodeBuddyError{Status: http.StatusNotImplemented, Code: "not_supported", Message: "codebuddy: no exact HY4 tokenizer or count endpoint is available", RequestScoped: true}
}

// HttpRequest executes a boundary-validated provider request with injected
// credentials. Only the chat and catalog paths on the exact realm host are
// allowed, with POST for chat and GET for catalog; auth/token/refresh routes
// are never reachable through this generic method.
func (e *CodeBuddyExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codebuddy executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), codebuddy.Provider) {
		return nil, fmt.Errorf("codebuddy executor: auth is not a codebuddy credential")
	}
	credentials, err := codebuddy.CredentialsFromMetadata(auth.Metadata)
	if err != nil {
		return nil, fmt.Errorf("codebuddy executor: invalid credential: %w", err)
	}
	expectedHost := codebuddy.RealmHost(credentials.Realm)
	if expectedHost == "" {
		return nil, fmt.Errorf("codebuddy executor: invalid realm")
	}
	cloned := req.Clone(ctx)
	if cloned.URL == nil {
		return nil, fmt.Errorf("codebuddy executor: request has no URL")
	}
	if !strings.EqualFold(cloned.URL.Scheme, "https") {
		return nil, fmt.Errorf("codebuddy executor: only https provider requests are allowed")
	}
	if cloned.URL.User != nil {
		return nil, fmt.Errorf("codebuddy executor: userinfo is not allowed in provider requests")
	}
	if !strings.EqualFold(cloned.URL.Host, expectedHost) || cloned.URL.Host == "" {
		return nil, fmt.Errorf("codebuddy executor: host %q is outside the codebuddy realm boundary", cloned.URL.Host)
	}
	cloned.Host = cloned.URL.Host
	switch cloned.URL.Path {
	case "/v2/chat/completions":
		if cloned.Method != http.MethodPost {
			return nil, fmt.Errorf("codebuddy executor: chat path requires POST")
		}
	case "/v3/config", "/console/enterprises/personal/models", "/v2/enterprises/personal/models":
		if cloned.Method != http.MethodGet {
			return nil, fmt.Errorf("codebuddy executor: catalog paths require GET")
		}
	default:
		return nil, fmt.Errorf("codebuddy executor: path %q is not allowed", cloned.URL.Path)
	}
	for _, header := range []string{
		"Authorization", "Cookie", "X-Forwarded-For", "X-Real-Ip", "X-Client-Ip",
		"X-Refresh-Token", "X-Auth-Refresh-Source", "X-User-Id", "X-Domain",
		"X-Enterprise-Id", "X-No-Enterprise-Id", "X-No-Department-Info",
		"X-Device-Token", "X-Machine-Id", "X-Session-Id", "X-Request-Id",
		"X-Trace-Id", "X-Conversation-Id", "X-Conversation-Request-Id",
		"X-Conversation-Message-Id", "X-Root-Request-Id",
	} {
		cloned.Header.Del(header)
	}
	if err := codebuddy.InjectHeadersForRequest(cloned, credentials); err != nil {
		return nil, err
	}
	baseClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	noRedirect := *baseClient
	noRedirect.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noRedirect.Do(cloned)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("codebuddy executor: redirect rejected for credential-bearing request")
	}
	return resp, nil
}

// codeBuddyUpstreamHeaders merges safe operator headers for the chat call:
// inbound headers plus auth-configured header overrides. The provider client
// honors only a User-Agent override and a validated trace ID from this set.
func codeBuddyUpstreamHeaders(auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) http.Header {
	merged := http.Header{}
	for key, values := range opts.Headers {
		merged[key] = append([]string(nil), values...)
	}
	if auth != nil {
		for key, value := range auth.Attributes {
			if !strings.HasPrefix(key, "header:") {
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(key, "header:"))
			if name == "" || strings.TrimSpace(value) == "" {
				continue
			}
			merged.Set(name, value)
		}
	}
	return merged
}

// recordCodeBuddyRequest logs a redacted upstream request: URL, method, body
// and provider/auth identity without any credential values.
func recordCodeBuddyRequest(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, realm codebuddy.Realm, body []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{
		URL:       codebuddy.ChatURL(realm),
		Method:    http.MethodPost,
		Headers:   http.Header{"Content-Type": []string{"application/json"}, "Accept": []string{"text/event-stream"}},
		Body:      body,
		Provider:  codebuddy.Provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

// codeBuddyIdentityMetadata captures requested, resolved and returned model
// identity separately. An absent returned identifier stays absent.
func codeBuddyIdentityMetadata(requested string, prepared helps.CodeBuddyPreparedRequest, stream *helps.CodeBuddyStream) map[string]any {
	metadata := map[string]any{
		"codebuddy_requested_model": requested,
		"codebuddy_upstream_model":  prepared.Model.ID,
		"codebuddy_realm":           string(prepared.Credentials.Realm),
	}
	if returned, ok := stream.ObservedModel(); ok {
		metadata["codebuddy_returned_model"] = returned
	}
	return metadata
}
