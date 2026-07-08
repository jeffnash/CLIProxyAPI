package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func clearManagedProviderStateForTest(t *testing.T) {
	t.Helper()
	EvictManagedProviderModelCache("")
	resetManagedProviderHealthForTest()
	managedProviderAliasMapMu.Lock()
	previous := make(map[string]map[string]string, len(managedProviderAliasMap))
	for provider, aliases := range managedProviderAliasMap {
		previous[provider] = cloneStringMap(aliases)
	}
	managedProviderAliasMap = make(map[string]map[string]string)
	managedProviderAliasMapMu.Unlock()
	t.Cleanup(func() {
		EvictManagedProviderModelCache("")
		managedProviderAliasMapMu.Lock()
		managedProviderAliasMap = previous
		managedProviderAliasMapMu.Unlock()
	})
}

func managedProviderTestConfig(baseURL string) *config.Config {
	maxRetries := 0
	routeHealthEnabled := false
	return &config.Config{
		SDKConfig: config.SDKConfig{
			ManagedProviders: []config.ManagedProviderConfig{{
				Name:        "example-provider",
				Prefix:      "example-",
				APIKey:      "test-key",
				BaseURL:     baseURL,
				MaxRetries:  &maxRetries,
				RouteHealth: config.ManagedProviderRouteHealthConfig{Enabled: &routeHealthEnabled},

				FallbackModels: []string{"glm-5.2", "qwen3.7-max"},
			}},
		},
	}
}

func managedProviderTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "a",
		Provider: "example-provider",
		Attributes: map[string]string{
			"api_key":  "test-key",
			"base_url": baseURL,
			"prefix":   "example-",
		},
	}
}

func TestManagedProviderExecutorClaudeSourceUsesMessagesEndpoint(t *testing.T) {
	var sawRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path=%q, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization=%q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != "2023-06-01" {
			t.Fatalf("anthropic-version=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got := body["model"]; got != "glm-5.2" {
			t.Fatalf("model=%v, want glm-5.2", got)
		}
		if !strings.Contains(gjson.GetBytes(mustMarshalJSON(t, body), "messages.0.content").Raw, "hello") {
			t.Fatalf("expected original Claude message content in body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"glm-5.2","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	cfg := managedProviderTestConfig(srv.URL + "/v1")
	exec := NewManagedProviderExecutor("example-provider", cfg)
	resp, err := exec.Execute(context.Background(), managedProviderTestAuth(srv.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "glm-5.2",
		Payload: []byte(`{"model":"glm-5.2","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatClaude,
		ResponseFormat: sdktranslator.FormatClaude,
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "anthropic",
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !sawRequest {
		t.Fatal("server did not receive request")
	}
	if !strings.Contains(string(resp.Payload), `"ok"`) {
		t.Fatalf("payload=%s", resp.Payload)
	}
}

func TestManagedProviderExecutorOpenAISourceUsesChatCompletionsEndpoint(t *testing.T) {
	var sawRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%q, want /v1/chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization=%q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got := body["model"]; got != "qwen3.7-max" {
			t.Fatalf("model=%v, want qwen3.7-max", got)
		}
		if !strings.Contains(gjson.GetBytes(mustMarshalJSON(t, body), "messages.0.content").Raw, "hello") {
			t.Fatalf("expected original OpenAI message content in body: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	cfg := managedProviderTestConfig(srv.URL + "/v1")
	exec := NewManagedProviderExecutor("example-provider", cfg)
	resp, err := exec.Execute(context.Background(), managedProviderTestAuth(srv.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "qwen3.7-max",
		Payload: []byte(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !sawRequest {
		t.Fatal("server did not receive request")
	}
	if !strings.Contains(string(resp.Payload), `"ok"`) {
		t.Fatalf("payload=%s", resp.Payload)
	}
}

func TestManagedProviderExecutorOpenAIReasoningEffortPassesThroughDiscoveredModel(t *testing.T) {
	clearManagedProviderStateForTest(t)
	var sawChat bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"qwen3.7-max","object":"model"}]}`))
		case "/v1/chat/completions":
			sawChat = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if got := body["reasoning_effort"]; got != "high" {
				t.Fatalf("reasoning_effort=%v, want high; body=%#v", got, body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := managedProviderTestConfig(srv.URL + "/v1")
	exec := NewManagedProviderExecutor("example-provider", cfg)
	models := exec.FetchModels(context.Background(), nil, cfg)
	registry.GetGlobalRegistry().RegisterClient("managed-provider-thinking-test", "example-provider", models)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient("managed-provider-thinking-test")
	})

	_, err := exec.Execute(context.Background(), managedProviderTestAuth(srv.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "qwen3.7-max",
		Payload: []byte(`{"model":"qwen3.7-max","reasoning_effort":"high","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !sawChat {
		t.Fatal("server did not receive chat request")
	}
}

func TestManagedProviderExecutorOpenAIStreamNormalizesSSEFrames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := managedProviderTestConfig(srv.URL + "/v1")
	exec := NewManagedProviderExecutor("example-provider", cfg)
	result, err := exec.ExecuteStream(context.Background(), managedProviderTestAuth(srv.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "qwen3.7-max",
		Payload: []byte(`{"model":"qwen3.7-max","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, ResponseFormat: sdktranslator.FormatOpenAI})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var chunks []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		if strings.TrimSpace(string(chunk.Payload)) == "" {
			continue
		}
		chunks = append(chunks, string(chunk.Payload))
	}

	got := strings.Join(chunks, "")
	if strings.Contains(got, "data:") {
		t.Fatalf("stream chunks must be unframed JSON for OpenAI handler, got %q", got)
	}
	if strings.Contains(got, "[DONE]") {
		t.Fatalf("stream chunks must not forward upstream DONE marker, got %q", got)
	}
	if !strings.Contains(got, `"content":"ok"`) {
		t.Fatalf("stream chunks missing content: %q", got)
	}
}

func TestManagedProviderExecutorFetchModelsGeneratesExplicitAliases(t *testing.T) {
	clearManagedProviderStateForTest(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path=%q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("Authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.2","object":"model"},{"id":"qwen3.7-max","object":"model"}]}`))
	}))
	defer srv.Close()

	cfg := managedProviderTestConfig(srv.URL + "/v1")
	cfg.ManagedProviders[0].OpenAIResponsesPath = "/responses"
	models := NewManagedProviderExecutor("example-provider", cfg).FetchModels(context.Background(), nil, cfg)
	if !hasModelID(models, "glm-5.2") || !hasModelID(models, "example-glm-5.2") ||
		!hasModelID(models, "anthropic-example-glm-5.2") || !hasModelID(models, "openai-example-glm-5.2") ||
		!hasModelID(models, "openai-responses-example-glm-5.2") || !hasModelID(models, "openai-completions-example-glm-5.2") {
		t.Fatalf("expected base and alias models, got %#v", modelIDs(models))
	}
	for _, id := range []string{"glm-5.2", "example-glm-5.2", "anthropic-example-glm-5.2", "openai-example-glm-5.2", "openai-responses-example-glm-5.2", "openai-completions-example-glm-5.2"} {
		model := findModel(models, id)
		if model == nil {
			t.Fatalf("missing model %q", id)
		}
		if !model.UserDefined {
			t.Fatalf("model %q must be UserDefined so managed-provider thinking controls pass through", id)
		}
	}
	for _, alias := range []string{"example-glm-5.2", "anthropic-example-glm-5.2", "openai-example-glm-5.2", "openai-responses-example-glm-5.2", "openai-completions-example-glm-5.2"} {
		if got := resolveManagedProviderModel("example-provider", "example-", alias); got != "glm-5.2" {
			t.Fatalf("resolveManagedProviderModel(%q)=%q, want glm-5.2", alias, got)
		}
	}
	for _, alias := range []string{"openai-responses-example-manual-model", "openai-completions-example-manual-model"} {
		if got := resolveManagedProviderModel("example-provider", "example-", alias); got != "manual-model" {
			t.Fatalf("resolveManagedProviderModel(%q)=%q, want manual-model", alias, got)
		}
	}
}

func TestManagedProviderExecutorResponsesAliasFallsBackToChatCompletions(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1/responses":
			http.Error(w, `{"error":"not implemented"}`, http.StatusNotImplemented)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := managedProviderTestConfig(srv.URL + "/v1")
	cfg.ManagedProviders[0].OpenAIResponsesPath = "/responses"
	exec := NewManagedProviderExecutor("example-provider", cfg)
	resp, err := exec.Execute(context.Background(), managedProviderTestAuth(srv.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "qwen3.7-max",
		Payload: []byte(`{"model":"qwen3.7-max","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAI,
		ResponseFormat: sdktranslator.FormatOpenAI,
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "openai-responses",
		},
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if !strings.Contains(string(resp.Payload), `"ok"`) {
		t.Fatalf("payload=%s", resp.Payload)
	}
	if got := strings.Join(paths, ","); got != "/v1/responses,/v1/chat/completions" {
		t.Fatalf("paths=%s, want responses then chat completions", got)
	}
}

func TestManagedProviderExecutorAnthropicStreamNoFirstEventFallsBackToResponses(t *testing.T) {
	var paths []string
	var pathsMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathsMu.Lock()
		paths = append(paths, r.URL.Path)
		pathsMu.Unlock()
		switch r.URL.Path {
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			time.Sleep(150 * time.Millisecond)
		case "/v1/responses":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: response.completed\n\n"))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	cfg := managedProviderTestConfig(srv.URL + "/v1")
	cfg.ManagedProviders[0].OpenAIResponsesPath = "/responses"
	cfg.ManagedProviders[0].RouteHealth.FirstEventTimeout = "20ms"
	exec := NewManagedProviderExecutor("example-provider", cfg)
	start := time.Now()
	result, err := exec.ExecuteStream(context.Background(), managedProviderTestAuth(srv.URL+"/v1"), cliproxyexecutor.Request{
		Model:   "qwen3.7-max",
		Payload: []byte(`{"model":"qwen3.7-max","input":"hello","stream":true}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FormatOpenAIResponse,
		ResponseFormat: sdktranslator.FormatOpenAIResponse,
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "anthropic",
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before fallback chunk")
		}
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		if !strings.Contains(string(chunk.Payload), "response.completed") {
			t.Fatalf("payload=%q, want fallback responses event", chunk.Payload)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fallback stream chunk")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("fallback took %s, want under 1s", elapsed)
	}
	pathsMu.Lock()
	got := strings.Join(paths, ",")
	pathsMu.Unlock()
	if got != "/v1/messages,/v1/responses" {
		t.Fatalf("paths=%s, want anthropic then responses", got)
	}
}

func TestManagedProviderTransportHealthPersistsAndRanksAfterReload(t *testing.T) {
	clearManagedProviderStateForTest(t)
	enabled := true
	statePath := t.TempDir() + "/managed_provider_health.json"
	provider := config.ManagedProviderConfig{
		Name: "example-provider",
		RouteHealth: config.ManagedProviderRouteHealthConfig{
			Enabled:   &enabled,
			StatePath: statePath,
			Cooldown:  "1h",
		},
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{ManagedProviders: []config.ManagedProviderConfig{provider}}}

	recordManagedProviderTransportHealth(cfg, provider, "example-provider", "qwen3.7-max", managedProviderTransportOpenAI, managedProviderHealthOutcome{
		StatusCode: http.StatusNotImplemented,
		Err:        statusErr{code: http.StatusNotImplemented, msg: "not implemented"},
		Body:       []byte("not implemented"),
	})
	recordManagedProviderTransportHealth(cfg, provider, "example-provider", "qwen3.7-max", managedProviderTransportResponses, managedProviderHealthOutcome{
		Success:    true,
		StatusCode: http.StatusOK,
		Latency:    50 * time.Millisecond,
	})

	flushManagedProviderHealthState()
	resetManagedProviderHealthForTest()
	ranked := rankManagedProviderTransports(cfg, provider, "example-provider", "qwen3.7-max", []string{managedProviderTransportOpenAI, managedProviderTransportResponses}, sdktranslator.FormatOpenAIResponse)
	if len(ranked) != 2 || ranked[0] != managedProviderTransportResponses {
		t.Fatalf("ranked=%v, want responses first after reload", ranked)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state file missing after health updates: %v", err)
	}

	recordManagedProviderTransportHealth(cfg, provider, "example-provider", "unavailable-model", managedProviderTransportOpenAI, managedProviderHealthOutcome{
		StatusCode: http.StatusNotImplemented,
		Err:        statusErr{code: http.StatusNotImplemented, msg: "not implemented"},
		Body:       []byte("not implemented"),
	})
	recordManagedProviderTransportHealth(cfg, provider, "example-provider", "unavailable-model", managedProviderTransportResponses, managedProviderHealthOutcome{
		StatusCode: http.StatusNotImplemented,
		Err:        statusErr{code: http.StatusNotImplemented, msg: "not implemented"},
		Body:       []byte("not implemented"),
	})
	provider.BaseURL = "https://provider.example/v1"
	provider.Prefix = "example-"
	provider.APIKey = "test-key"
	provider.OpenAIResponsesPath = "/responses"
	cfg.ManagedProviders = []config.ManagedProviderConfig{provider}
	exec := NewManagedProviderExecutor("example-provider", cfg)
	plan := exec.transportPlan(nil, cliproxyexecutor.Request{Model: "unavailable-model"}, cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "openai",
		},
	})
	if len(plan.Transports) == 0 || plan.Transports[0] != managedProviderTransportClaude {
		t.Fatalf("openai-family unavailable plan=%v, want claude first", plan.Transports)
	}
}

func TestManagedProviderClientErrorDoesNotCooldownOrPersistRawBody(t *testing.T) {
	clearManagedProviderStateForTest(t)
	enabled := true
	provider := config.ManagedProviderConfig{
		Name: "example-provider",
		RouteHealth: config.ManagedProviderRouteHealthConfig{
			Enabled:   &enabled,
			StatePath: t.TempDir() + "/managed_provider_health.json",
			Cooldown:  "1h",
		},
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{ManagedProviders: []config.ManagedProviderConfig{provider}}}
	body := []byte(`{"error":{"type":"invalid_request_error","code":"bad_request","message":"bad request echoed prompt content secret-token-123 in messages"}}`)

	recordManagedProviderTransportHealth(cfg, provider, "example-provider", "bad-request-model", managedProviderTransportOpenAI, managedProviderHealthOutcome{
		StatusCode: http.StatusBadRequest,
		Err:        statusErr{code: http.StatusBadRequest, msg: string(body)},
		Body:       body,
	})

	managedProviderHealth.mu.Lock()
	record := managedProviderHealth.records[managedProviderHealthKey("example-provider", "bad-request-model", managedProviderTransportOpenAI)]
	managedProviderHealth.mu.Unlock()
	if record == nil {
		t.Fatal("missing health record")
	}
	if !record.CooldownUntil.IsZero() {
		t.Fatalf("CooldownUntil=%s, want zero for request-specific 400", record.CooldownUntil)
	}
	if strings.Contains(record.LastError, "secret-token-123") || strings.Contains(record.LastError, "messages") || strings.Contains(record.LastError, "prompt content") {
		t.Fatalf("LastError leaked upstream body content: %q", record.LastError)
	}
	if !strings.Contains(record.LastError, "status=400") || !strings.Contains(record.LastError, "type=invalid_request_error") || !strings.Contains(record.LastError, "code=bad_request") {
		t.Fatalf("LastError=%q, want status/type/code summary", record.LastError)
	}
}

func TestManagedProviderPinnedIntentIgnoresCooldownButDemotesUnsupported(t *testing.T) {
	clearManagedProviderStateForTest(t)
	enabled := true
	provider := config.ManagedProviderConfig{
		Name:                "example-provider",
		Prefix:              "example-",
		APIKey:              "test-key",
		BaseURL:             "https://provider.example/v1",
		OpenAIResponsesPath: "/responses",
		RouteHealth: config.ManagedProviderRouteHealthConfig{
			Enabled:   &enabled,
			StatePath: t.TempDir() + "/managed_provider_health.json",
			Cooldown:  "1h",
		},
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{ManagedProviders: []config.ManagedProviderConfig{provider}}}

	recordManagedProviderTransportHealth(cfg, provider, "example-provider", "qwen3.7-max", managedProviderTransportOpenAI, managedProviderHealthOutcome{
		StatusCode: http.StatusServiceUnavailable,
		Err:        statusErr{code: http.StatusServiceUnavailable, msg: "temporarily unavailable"},
		Body:       []byte(`{"error":{"message":"temporarily unavailable"}}`),
	})
	exec := NewManagedProviderExecutor("example-provider", cfg)
	pinned := exec.transportPlan(nil, cliproxyexecutor.Request{Model: "qwen3.7-max"}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "openai-completions",
		},
	})
	if len(pinned.Transports) == 0 || pinned.Transports[0] != managedProviderTransportOpenAI {
		t.Fatalf("pinned plan=%v, want openai first despite cooldown", pinned.Transports)
	}

	dynamic := exec.transportPlan(nil, cliproxyexecutor.Request{Model: "qwen3.7-max"}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "openai",
		},
	})
	if len(dynamic.Transports) == 0 || dynamic.Transports[0] == managedProviderTransportOpenAI {
		t.Fatalf("dynamic openai-family plan=%v, want cooled-down openai demoted", dynamic.Transports)
	}

	recordManagedProviderTransportHealth(cfg, provider, "example-provider", "qwen3.7-max", managedProviderTransportOpenAI, managedProviderHealthOutcome{
		StatusCode: http.StatusNotImplemented,
		Err:        statusErr{code: http.StatusNotImplemented, msg: "not implemented"},
		Body:       []byte(`{"error":{"message":"not implemented"}}`),
	})
	pinned = exec.transportPlan(nil, cliproxyexecutor.Request{Model: "qwen3.7-max"}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "openai-completions",
		},
	})
	if len(pinned.Transports) == 0 || pinned.Transports[0] == managedProviderTransportOpenAI {
		t.Fatalf("pinned unsupported plan=%v, want unsupported openai demoted", pinned.Transports)
	}
}

func TestManagedProviderSelectTransportAutoAndConfig(t *testing.T) {
	cfg := managedProviderTestConfig("https://provider.example/v1")
	exec := NewManagedProviderExecutor("example-provider", cfg)
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); got != managedProviderTransportClaude {
		t.Fatalf("generic auto transport=%q, want claude", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); got != managedProviderTransportOpenAI {
		t.Fatalf("OpenAI source transport=%q", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}); got != managedProviderTransportOpenAI {
		t.Fatalf("Responses source without direct path transport=%q, want openai", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); got != managedProviderTransportOpenAI {
		t.Fatalf("Gemini source default transport=%q, want openai", got)
	}

	cfg.ManagedProviders[0].OpenAIResponsesPath = "/responses"
	exec = NewManagedProviderExecutor("example-provider", cfg)
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); got != managedProviderTransportOpenAI {
		t.Fatalf("OpenAI source with responses available transport=%q, want openai", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}); got != managedProviderTransportResponses {
		t.Fatalf("Responses source with responses available transport=%q, want responses", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); got != managedProviderTransportClaude {
		t.Fatalf("Claude source with responses available transport=%q, want claude", got)
	}

	cfg.ManagedProviders[0].TransportMode = "openai-completions"
	exec = NewManagedProviderExecutor("example-provider", cfg)
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); got != managedProviderTransportOpenAI {
		t.Fatalf("forced chat-completions transport=%q, want openai", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatOpenAI,
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "anthropic",
		},
	}); got != managedProviderTransportClaude {
		t.Fatalf("metadata forced transport=%q, want claude", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Metadata: map[string]any{
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "openai-responses",
		},
	}); got != managedProviderTransportResponses {
		t.Fatalf("metadata forced transport=%q, want responses", got)
	}
}

func mustMarshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}

func hasModelID(models []*registry.ModelInfo, id string) bool {
	return findModel(models, id) != nil
}

func findModel(models []*registry.ModelInfo, id string) *registry.ModelInfo {
	for _, model := range models {
		if model != nil && model.ID == id {
			return model
		}
	}
	return nil
}

func modelIDs(models []*registry.ModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		if model != nil {
			out = append(out, model.ID)
		}
	}
	return out
}
