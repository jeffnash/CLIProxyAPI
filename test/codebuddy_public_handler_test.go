package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

var codeBuddyPublicHarnessSeq atomic.Int64

type codeBuddyPublicTransport struct {
	mu      sync.Mutex
	wires   [][]byte
	calls   int
	respond func(call int, body []byte) *http.Response
}

func (t *codeBuddyPublicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	t.mu.Lock()
	t.wires = append(t.wires, body)
	call := t.calls
	t.calls++
	t.mu.Unlock()
	return t.respond(call, body), nil
}

func (t *codeBuddyPublicTransport) wireCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.wires)
}

type codeBuddyPublicRoundTripperProvider struct {
	transport http.RoundTripper
}

func (p codeBuddyPublicRoundTripperProvider) RoundTripperFor(_ *coreauth.Auth) http.RoundTripper {
	return p.transport
}

type codeBuddyPublicHarness struct {
	router    *gin.Engine
	transport *codeBuddyPublicTransport
	manager   *coreauth.Manager
	authID    string
}

func codeBuddyPublicRegister(t *testing.T, authID string, ids ...*registry.ModelInfo) {
	t.Helper()
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(authID, "codebuddy", ids)
	t.Cleanup(func() { reg.UnregisterClient(authID) })
}

func newCodeBuddyPublicHarness(t *testing.T, respond func(call int, body []byte) *http.Response) *codeBuddyPublicHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	seq := codeBuddyPublicHarnessSeq.Add(1)
	transport := &codeBuddyPublicTransport{respond: respond}
	mgr := coreauth.NewManager(nil, nil, nil)
	mgr.SetConfig(&config.Config{})
	mgr.RegisterExecutor(executor.NewCodeBuddyExecutor(&config.Config{}))
	mgr.SetRoundTripperProvider(codeBuddyPublicRoundTripperProvider{transport: transport})
	auth := codeBuddyCompatAuth()
	auth.ID = "codebuddy-public-" + strconv.FormatInt(seq, 10)
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	codeBuddyPublicRegister(t, auth.ID, &registry.ModelInfo{ID: "codebuddy-global-hy4-preview", UpstreamID: "hy4-preview", Type: "codebuddy"})
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, mgr)
	openaiHandler := openai.NewOpenAIAPIHandler(base)
	responsesHandler := openai.NewOpenAIResponsesAPIHandler(base)
	claudeHandler := claude.NewClaudeCodeAPIHandler(base)
	router := gin.New()
	router.POST("/v1/chat/completions", openaiHandler.ChatCompletions)
	router.POST("/v1/responses", responsesHandler.Responses)
	router.POST("/v1/messages", claudeHandler.ClaudeMessages)
	return &codeBuddyPublicHarness{router: router, transport: transport, manager: mgr, authID: auth.ID}
}

func (h *codeBuddyPublicHarness) post(t *testing.T, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	h.router.ServeHTTP(recorder, request)
	return recorder
}

// parsePublicSSEData returns decoded data payloads from an SSE body.
func parsePublicSSEData(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			out = append(out, payload)
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			t.Fatalf("invalid SSE JSON %q: %v", payload, err)
		}
		out = append(out, payload)
	}
	return out
}

const codeBuddyPublicTextSSE = "data: {\"id\":\"pub-1\",\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\ndata: {\"id\":\"pub-1\",\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"id\":\"pub-1\",\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"pub-1\",\"model\":\"hy4-preview\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\ndata: [DONE]\n\n"

func codeBuddyPublicSSE(sse string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse))}
}

func TestCodeBuddyPublicChat(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		recorder := harness.post(t, "/v1/chat/completions", `{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"hi"}]}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		out := recorder.Body.Bytes()
		if gjson.GetBytes(out, "choices.0.message.content").String() != "Hello" {
			t.Fatalf("content = %s", out)
		}
		if gjson.GetBytes(out, "choices.0.finish_reason").String() != "stop" {
			t.Fatalf("finish = %s", out)
		}
		if gjson.GetBytes(out, "usage.total_tokens").Int() != 9 {
			t.Fatalf("usage = %s", out)
		}
		if harness.transport.wireCount() != 1 {
			t.Fatalf("upstream calls = %d", harness.transport.wireCount())
		}
	})
	t.Run("stream", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		recorder := harness.post(t, "/v1/chat/completions", `{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
			t.Fatalf("content-type = %q", contentType)
		}
		events := parsePublicSSEData(t, recorder.Body.String())
		if len(events) == 0 || events[len(events)-1] != "[DONE]" {
			t.Fatalf("missing terminal [DONE]: %q", events)
		}
		var sawRole, sawContent, stops int
		for _, event := range events[:len(events)-1] {
			if gjson.Get(event, "choices.0.delta.role").String() == "assistant" {
				sawRole++
			}
			if strings.Contains(gjson.Get(event, "choices.0.delta.content").String(), "Hello") {
				sawContent++
			}
			if gjson.Get(event, "choices.0.finish_reason").String() == "stop" {
				stops++
			}
		}
		if sawRole == 0 || sawContent == 0 || stops != 1 {
			t.Fatalf("role=%d content=%d stops=%d in %q", sawRole, sawContent, stops, events)
		}
	})
	t.Run("tool round trip", func(t *testing.T) {
		toolSSE := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_pub_1\",\"type\":\"function\",\"function\":{\"name\":\"echo_nonce\",\"arguments\":\"{\\\"nonce\\\":\\\"PUB_OK\\\"}\"}}]}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		harness := newCodeBuddyPublicHarness(t, func(call int, _ []byte) *http.Response {
			if call == 0 {
				return codeBuddyPublicSSE(toolSSE)
			}
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		first := harness.post(t, "/v1/chat/completions", `{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"echo_nonce","parameters":{"type":"object"}}}],"tool_choice":"auto"}`, nil)
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d (%s)", first.Code, first.Body.String())
		}
		call := gjson.GetBytes(first.Body.Bytes(), "choices.0.message.tool_calls.0")
		if call.Get("id").String() != "call_pub_1" || call.Get("function.name").String() != "echo_nonce" {
			t.Fatalf("call = %s", first.Body.String())
		}
		var args map[string]any
		if err := json.Unmarshal([]byte(call.Get("function.arguments").String()), &args); err != nil || args["nonce"] != "PUB_OK" {
			t.Fatalf("args invalid: %v (%s)", err, first.Body.String())
		}
		second := harness.post(t, "/v1/chat/completions", `{"model":"codebuddy-global-hy4-preview","messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_pub_1","type":"function","function":{"name":"echo_nonce","arguments":"{\"nonce\":\"PUB_OK\"}"}}]},
			{"role":"tool","tool_call_id":"call_pub_1","content":"PUB_OK"}]}`, nil)
		if second.Code != http.StatusOK {
			t.Fatalf("second status = %d (%s)", second.Code, second.Body.String())
		}
		if gjson.GetBytes(second.Body.Bytes(), "choices.0.message.content").String() != "Hello" {
			t.Fatalf("final = %s", second.Body.String())
		}
		if harness.transport.wireCount() != 2 {
			t.Fatalf("upstream calls = %d", harness.transport.wireCount())
		}
	})
	t.Run("error after first output", func(t *testing.T) {
		broken := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"code\":11115,\"msg\":\"prompt is too long\"}\n\n"
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(broken)
		})
		recorder := harness.post(t, "/v1/chat/completions", `{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		events := parsePublicSSEData(t, recorder.Body.String())
		for _, event := range events {
			if event == "" || event == "[DONE]" {
				continue
			}
			if reason := gjson.Get(event, "choices.0.finish_reason").String(); reason == "stop" || reason == "tool_calls" {
				t.Fatalf("fake success terminal %q in %q", reason, events)
			}
		}
		if !strings.Contains(recorder.Body.String(), "error") && !strings.Contains(recorder.Body.String(), "prompt is too long") {
			t.Fatalf("upstream failure not surfaced: %q", events)
		}
		if harness.transport.wireCount() != 1 {
			t.Fatalf("midstream failure replayed upstream: %d calls", harness.transport.wireCount())
		}
	})
	t.Run("empty upstream is a gateway failure", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE("")
		})
		recorder := harness.post(t, "/v1/chat/completions", `{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"hi"}]}`, nil)
		if recorder.Code != http.StatusBadGateway {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if gjson.GetBytes(recorder.Body.Bytes(), "choices").Exists() {
			t.Fatalf("fake completion in %s", recorder.Body.String())
		}
	})
	t.Run("alias routes to entitled model", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		harness.manager.SetOAuthModelAlias(map[string][]config.OAuthModelAlias{
			"codebuddy": {{Name: "codebuddy-global-hy4-preview", Alias: "hy4", Fork: true}},
		})
		// The service registers forked aliases alongside the public ID so the
		// handler layer can route them.
		codeBuddyPublicRegister(t, harness.authID,
			&registry.ModelInfo{ID: "codebuddy-global-hy4-preview", UpstreamID: "hy4-preview", Type: "codebuddy"},
			&registry.ModelInfo{ID: "hy4", UpstreamID: "hy4-preview", Type: "codebuddy"},
		)
		recorder := harness.post(t, "/v1/chat/completions", `{"model":"hy4","messages":[{"role":"user","content":"hi"}]}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if gjson.GetBytes(recorder.Body.Bytes(), "choices.0.message.content").String() != "Hello" {
			t.Fatalf("content = %s", recorder.Body.String())
		}
		if harness.transport.wireCount() != 1 {
			t.Fatalf("upstream calls = %d", harness.transport.wireCount())
		}
		if wire := harness.transport.wires[0]; !strings.Contains(string(wire), `"hy4-preview"`) {
			t.Fatalf("alias misresolved: %s", wire)
		}
	})
}

func TestCodeBuddyPublicResponses(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		recorder := harness.post(t, "/v1/responses", `{"model":"codebuddy-global-hy4-preview","input":"hi"}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		out := recorder.Body.Bytes()
		if gjson.GetBytes(out, "object").String() != "response" {
			t.Fatalf("object = %s", out)
		}
		if gjson.GetBytes(out, "status").String() != "completed" {
			t.Fatalf("status = %s", out)
		}
		if !strings.Contains(string(out), "Hello") {
			t.Fatalf("text missing: %s", out)
		}
		if !gjson.GetBytes(out, "usage.total_tokens").Exists() {
			t.Fatalf("usage missing: %s", out)
		}
	})
	t.Run("stream", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		recorder := harness.post(t, "/v1/responses", `{"model":"codebuddy-global-hy4-preview","input":"hi","stream":true}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		events := parsePublicSSEData(t, recorder.Body.String())
		var created, completed, failed int
		for _, event := range events {
			switch gjson.Get(event, "type").String() {
			case "response.created":
				created++
			case "response.completed":
				completed++
			case "response.failed":
				failed++
			}
		}
		if created != 1 || completed != 1 || failed != 0 {
			t.Fatalf("created=%d completed=%d failed=%d in %q", created, completed, failed, events)
		}
	})
	t.Run("tool round trip", func(t *testing.T) {
		toolSSE := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_pub_r1\",\"type\":\"function\",\"function\":{\"name\":\"echo_nonce\",\"arguments\":\"{\\\"nonce\\\":\\\"R_OK\\\"}\"}}]}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		harness := newCodeBuddyPublicHarness(t, func(call int, _ []byte) *http.Response {
			if call == 0 {
				return codeBuddyPublicSSE(toolSSE)
			}
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		first := harness.post(t, "/v1/responses", `{"model":"codebuddy-global-hy4-preview","input":"go","tools":[{"type":"function","name":"echo_nonce","parameters":{"type":"object"}}]}`, nil)
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d (%s)", first.Code, first.Body.String())
		}
		var callID string
		for _, item := range gjson.GetBytes(first.Body.Bytes(), "output").Array() {
			if item.Get("type").String() == "function_call" {
				callID = item.Get("call_id").String()
				if item.Get("name").String() != "echo_nonce" {
					t.Fatalf("call = %s", first.Body.String())
				}
				var args map[string]any
				if err := json.Unmarshal([]byte(item.Get("arguments").String()), &args); err != nil || args["nonce"] != "R_OK" {
					t.Fatalf("args invalid: %v (%s)", err, first.Body.String())
				}
			}
		}
		if callID == "" {
			t.Fatalf("no function_call in %s", first.Body.String())
		}
		second := harness.post(t, "/v1/responses", `{"model":"codebuddy-global-hy4-preview","input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
			{"type":"function_call","call_id":"`+callID+`","name":"echo_nonce","arguments":"{\"nonce\":\"R_OK\"}"},
			{"type":"function_call_output","call_id":"`+callID+`","output":"R_OK"}]}`, nil)
		if second.Code != http.StatusOK {
			t.Fatalf("second status = %d (%s)", second.Code, second.Body.String())
		}
		if gjson.GetBytes(second.Body.Bytes(), "status").String() != "completed" {
			t.Fatalf("final = %s", second.Body.String())
		}
	})
	t.Run("error terminal", func(t *testing.T) {
		broken := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"code\":11115,\"msg\":\"prompt is too long\"}\n\n"
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(broken)
		})
		recorder := harness.post(t, "/v1/responses", `{"model":"codebuddy-global-hy4-preview","input":"hi","stream":true}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		events := parsePublicSSEData(t, recorder.Body.String())
		for _, event := range events {
			if gjson.Get(event, "type").String() == "response.completed" {
				t.Fatalf("fake completed success in %q", events)
			}
		}
		joined := strings.Join(events, "\n")
		if !strings.Contains(joined, "response.failed") && !strings.Contains(joined, "error") {
			t.Fatalf("failure terminal not surfaced: %q", events)
		}
		if harness.transport.wireCount() != 1 {
			t.Fatalf("midstream failure replayed upstream: %d calls", harness.transport.wireCount())
		}
	})
	t.Run("server tool rejected", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		recorder := harness.post(t, "/v1/responses", `{"model":"codebuddy-global-hy4-preview","input":"hi","tools":[{"type":"web_search_preview"}]}`, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "web_search_preview") {
			t.Fatalf("rejection names no tool: %s", recorder.Body.String())
		}
		if harness.transport.wireCount() != 0 {
			t.Fatal("upstream contacted for rejected request")
		}
	})
}

func TestCodeBuddyPublicMessages(t *testing.T) {
	t.Run("non-stream", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		recorder := harness.post(t, "/v1/messages", `{"model":"codebuddy-global-hy4-preview","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		out := recorder.Body.Bytes()
		if gjson.GetBytes(out, "type").String() != "message" {
			t.Fatalf("type = %s", out)
		}
		if gjson.GetBytes(out, "stop_reason").String() != "end_turn" {
			t.Fatalf("stop = %s", out)
		}
		if gjson.GetBytes(out, "content.0.text").String() != "Hello" {
			t.Fatalf("content = %s", out)
		}
		if !gjson.GetBytes(out, "usage.input_tokens").Exists() || !gjson.GetBytes(out, "usage.output_tokens").Exists() {
			t.Fatalf("usage = %s", out)
		}
	})
	t.Run("stream", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		recorder := harness.post(t, "/v1/messages", `{"model":"codebuddy-global-hy4-preview","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		events := parsePublicSSEData(t, recorder.Body.String())
		var starts, deltas, stops int
		for _, event := range events {
			switch gjson.Get(event, "type").String() {
			case "message_start":
				starts++
			case "content_block_delta":
				deltas++
			case "message_stop":
				stops++
			case "error":
				t.Fatalf("error event in %q", events)
			}
		}
		if starts != 1 || deltas == 0 || stops != 1 {
			t.Fatalf("start=%d deltas=%d stop=%d in %q", starts, deltas, stops, events)
		}
	})
	t.Run("tool round trip", func(t *testing.T) {
		toolSSE := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_pub_m1\",\"type\":\"function\",\"function\":{\"name\":\"echo_nonce\",\"arguments\":\"{\\\"nonce\\\":\\\"M_OK\\\"}\"}}]}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
		harness := newCodeBuddyPublicHarness(t, func(call int, _ []byte) *http.Response {
			if call == 0 {
				return codeBuddyPublicSSE(toolSSE)
			}
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		first := harness.post(t, "/v1/messages", `{"model":"codebuddy-global-hy4-preview","max_tokens":64,"messages":[{"role":"user","content":"go"}],"tools":[{"name":"echo_nonce","description":"d","input_schema":{"type":"object"}}]}`, nil)
		if first.Code != http.StatusOK {
			t.Fatalf("first status = %d (%s)", first.Code, first.Body.String())
		}
		var toolID string
		for _, block := range gjson.GetBytes(first.Body.Bytes(), "content").Array() {
			if block.Get("type").String() == "tool_use" {
				toolID = block.Get("id").String()
				if block.Get("name").String() != "echo_nonce" {
					t.Fatalf("tool = %s", first.Body.String())
				}
			}
		}
		if toolID == "" {
			t.Fatalf("no tool_use in %s", first.Body.String())
		}
		if gjson.GetBytes(first.Body.Bytes(), "stop_reason").String() != "tool_use" {
			t.Fatalf("stop = %s", first.Body.String())
		}
		second := harness.post(t, "/v1/messages", `{"model":"codebuddy-global-hy4-preview","max_tokens":64,"messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":[{"type":"tool_use","id":"`+toolID+`","name":"echo_nonce","input":{"nonce":"M_OK"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+toolID+`","content":"M_OK"}]}]}`, nil)
		if second.Code != http.StatusOK {
			t.Fatalf("second status = %d (%s)", second.Code, second.Body.String())
		}
		if gjson.GetBytes(second.Body.Bytes(), "stop_reason").String() != "end_turn" {
			t.Fatalf("final = %s", second.Body.String())
		}
	})
	t.Run("error event", func(t *testing.T) {
		broken := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"code\":11115,\"msg\":\"prompt is too long\"}\n\n"
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(broken)
		})
		recorder := harness.post(t, "/v1/messages", `{"model":"codebuddy-global-hy4-preview","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		events := parsePublicSSEData(t, recorder.Body.String())
		var stops int
		for _, event := range events {
			if gjson.Get(event, "type").String() == "message_stop" {
				stops++
			}
		}
		if stops != 0 {
			t.Fatalf("successful message_stop claimed in %q", events)
		}
		if !strings.Contains(strings.Join(events, "\n"), "error") {
			t.Fatalf("error event missing in %q", events)
		}
		if harness.transport.wireCount() != 1 {
			t.Fatalf("midstream failure replayed upstream: %d calls", harness.transport.wireCount())
		}
	})
	t.Run("beta handling", func(t *testing.T) {
		for _, variant := range []struct {
			name    string
			path    string
			headers map[string]string
		}{
			{"query", "/v1/messages?beta=true", nil},
			{"header", "/v1/messages", map[string]string{"anthropic-beta": "advanced-tool-use-2025-11-20"}},
			{"plain", "/v1/messages", nil},
		} {
			t.Run(variant.name, func(t *testing.T) {
				harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
					return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
				})
				recorder := harness.post(t, variant.path, `{"model":"codebuddy-global-hy4-preview","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, variant.headers)
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
				}
				if gjson.GetBytes(recorder.Body.Bytes(), "stop_reason").String() != "end_turn" {
					t.Fatalf("stop = %s", recorder.Body.String())
				}
			})
		}
	})
	t.Run("server tool rejected", func(t *testing.T) {
		harness := newCodeBuddyPublicHarness(t, func(_ int, _ []byte) *http.Response {
			return codeBuddyPublicSSE(codeBuddyPublicTextSSE)
		})
		recorder := harness.post(t, "/v1/messages", `{"model":"codebuddy-global-hy4-preview","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status = %d (%s)", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "web_search_20250305") {
			t.Fatalf("rejection names no tool: %s", recorder.Body.String())
		}
		if harness.transport.wireCount() != 0 {
			t.Fatal("upstream contacted for rejected request")
		}
	})
}
