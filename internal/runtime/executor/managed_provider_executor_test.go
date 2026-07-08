package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	return &config.Config{
		SDKConfig: config.SDKConfig{
			ManagedProviders: []config.ManagedProviderConfig{{
				Name:           "example-provider",
				Prefix:         "example-",
				APIKey:         "test-key",
				BaseURL:        baseURL,
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
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, ResponseFormat: sdktranslator.FormatClaude})
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
	models := NewManagedProviderExecutor("example-provider", cfg).FetchModels(context.Background(), nil, cfg)
	if !hasModelID(models, "glm-5.2") || !hasModelID(models, "example-glm-5.2") ||
		!hasModelID(models, "anthropic-example-glm-5.2") || !hasModelID(models, "openai-example-glm-5.2") {
		t.Fatalf("expected base and alias models, got %#v", modelIDs(models))
	}
	for _, id := range []string{"glm-5.2", "example-glm-5.2", "anthropic-example-glm-5.2", "openai-example-glm-5.2"} {
		model := findModel(models, id)
		if model == nil {
			t.Fatalf("missing model %q", id)
		}
		if !model.UserDefined {
			t.Fatalf("model %q must be UserDefined so managed-provider thinking controls pass through", id)
		}
	}
	for _, alias := range []string{"example-glm-5.2", "anthropic-example-glm-5.2", "openai-example-glm-5.2"} {
		if got := resolveManagedProviderModel("example-provider", "example-", alias); got != "glm-5.2" {
			t.Fatalf("resolveManagedProviderModel(%q)=%q, want glm-5.2", alias, got)
		}
	}
}

func TestManagedProviderSelectTransportAutoAndConfig(t *testing.T) {
	cfg := managedProviderTestConfig("https://provider.example/v1")
	exec := NewManagedProviderExecutor("example-provider", cfg)
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); got != managedProviderTransportClaude {
		t.Fatalf("Claude source transport=%q", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}); got != managedProviderTransportOpenAI {
		t.Fatalf("OpenAI source transport=%q", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}); got != managedProviderTransportOpenAI {
		t.Fatalf("Responses source without direct path transport=%q, want openai", got)
	}
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatGemini}); got != managedProviderTransportClaude {
		t.Fatalf("Gemini source default transport=%q", got)
	}

	cfg.ManagedProviders[0].TransportMode = managedProviderTransportOpenAI
	exec = NewManagedProviderExecutor("example-provider", cfg)
	if got := exec.selectTransport(nil, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude}); got != managedProviderTransportOpenAI {
		t.Fatalf("forced transport=%q, want openai", got)
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
			cliproxyexecutor.ManagedProviderTransportMetadataKey: "openai",
		},
	}); got != managedProviderTransportOpenAI {
		t.Fatalf("metadata forced transport=%q, want openai", got)
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
