package test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type codeBuddyCompatTransport struct {
	mu      sync.Mutex
	wires   [][]byte
	hosts   []string
	respond func(body []byte) *http.Response
}

func (t *codeBuddyCompatTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	t.mu.Lock()
	t.wires = append(t.wires, raw)
	t.hosts = append(t.hosts, req.URL.Host)
	t.mu.Unlock()
	return t.respond(raw), nil
}

func codeBuddyCompatAuth() *cliproxyauth.Auth {
	tools := true
	reasoning := true
	creds := codebuddy.Credentials{
		Realm: codebuddy.RealmGlobal, AccessToken: "A", RefreshToken: "R", UID: "compat-user",
		Expired: testCodeBuddyCompatExpiry(), MachineID: "m", SessionID: "s",
	}
	catalog := codebuddy.Catalog{Models: []codebuddy.Model{{
		ID: "hy4-preview", Name: "HY4", MaxInputTokens: 1000000, MaxOutputTokens: 64000,
		SupportsTools: &tools, SupportsReasoning: &reasoning, Efforts: []string{"high"},
	}}}
	return &cliproxyauth.Auth{ID: "compat", Provider: "codebuddy", Metadata: codebuddy.Metadata(creds, catalog, testCodeBuddyCompatExpiry())}
}

func testCodeBuddyCompatExpiry() time.Time {
	return time.Now().Add(time.Hour)
}

const codeBuddyCompatSSE = "data: {\"id\":\"compat-1\",\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\ndata: {\"id\":\"compat-1\",\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"id\":\"compat-1\",\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"compat-1\",\"model\":\"hy4-preview\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\ndata: [DONE]\n\n"

func codeBuddyCompatOK(_ []byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(codeBuddyCompatSSE))}
}

func TestCodeBuddyAPICompatibility(t *testing.T) {
	exec := executor.NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyCompatAuth()
	run := func(t *testing.T, format sdktranslator.Format, payload string, stream bool) ([]byte, *codeBuddyCompatTransport) {
		t.Helper()
		transport := &codeBuddyCompatTransport{respond: codeBuddyCompatOK}
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
		req := cliproxyexecutor.Request{Model: "codebuddy-global-hy4-preview", Payload: []byte(payload)}
		opts := cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: []byte(payload)}
		if !stream {
			resp, err := exec.Execute(ctx, auth, req, opts)
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			return resp.Payload, transport
		}
		result, err := exec.ExecuteStream(ctx, auth, req, opts)
		if err != nil {
			t.Fatalf("stream: %v", err)
		}
		var out []string
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatalf("chunk: %v", chunk.Err)
			}
			out = append(out, string(chunk.Payload))
		}
		return []byte(strings.Join(out, "\n")), transport
	}

	t.Run("chat non-stream", func(t *testing.T) {
		out, transport := run(t, sdktranslator.FormatOpenAI, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false)
		if gjson.GetBytes(out, "choices.0.message.content").String() != "Hello" {
			t.Fatalf("out = %s", out)
		}
		if gjson.GetBytes(out, "usage.total_tokens").Int() != 9 {
			t.Fatalf("usage = %s", out)
		}
		if transport.hosts[0] != "www.codebuddy.ai" {
			t.Fatalf("host = %s", transport.hosts[0])
		}
	})
	t.Run("chat stream", func(t *testing.T) {
		out, _ := run(t, sdktranslator.FormatOpenAI, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, true)
		if !strings.Contains(string(out), "Hello") || strings.Count(string(out), `"finish_reason":"stop"`) != 1 {
			t.Fatalf("out = %s", out)
		}
	})
	t.Run("responses non-stream", func(t *testing.T) {
		out, transport := run(t, sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi"}`, false)
		if gjson.GetBytes(out, "object").String() != "response" {
			t.Fatalf("out = %s", out)
		}
		if !strings.Contains(string(out), "Hello") {
			t.Fatalf("text = %s", out)
		}
		if gjson.GetBytes(transport.wires[0], "messages").String() == "" {
			t.Fatalf("wire = %s", transport.wires[0])
		}
	})
	t.Run("responses stream", func(t *testing.T) {
		out, _ := run(t, sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi"}`, true)
		if !strings.Contains(string(out), "response.completed") {
			t.Fatalf("out = %s", out)
		}
	})
	t.Run("claude non-stream", func(t *testing.T) {
		out, _ := run(t, sdktranslator.FormatClaude, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, false)
		if gjson.GetBytes(out, "type").String() != "message" || gjson.GetBytes(out, "stop_reason").String() != "end_turn" {
			t.Fatalf("out = %s", out)
		}
		if !gjson.GetBytes(out, "usage.input_tokens").Exists() {
			t.Fatalf("usage = %s", out)
		}
	})
	t.Run("claude stream", func(t *testing.T) {
		out, _ := run(t, sdktranslator.FormatClaude, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, true)
		if !strings.Contains(string(out), "end_turn") || !strings.Contains(string(out), "input_tokens") {
			t.Fatalf("out = %s", out)
		}
	})
	t.Run("tool equivalence", func(t *testing.T) {
		chatPayload := `{"model":"m","messages":[{"role":"user","content":"run it"}],"tools":[{"type":"function","function":{"name":"echo_nonce","description":"d","parameters":{"type":"object"}}}],"tool_choice":"auto"}`
		claudePayload := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"run it"}],"tools":[{"name":"echo_nonce","description":"d","input_schema":{"type":"object"}}]}`
		_, chatTransport := run(t, sdktranslator.FormatOpenAI, chatPayload, false)
		_, claudeTransport := run(t, sdktranslator.FormatClaude, claudePayload, false)
		chatWire, claudeWire := chatTransport.wires[0], claudeTransport.wires[0]
		for _, wire := range [][]byte{chatWire, claudeWire} {
			if gjson.GetBytes(wire, "model").String() != "hy4-preview" || !gjson.GetBytes(wire, "stream").Bool() {
				t.Fatalf("wire = %s", wire)
			}
			if !strings.Contains(gjson.GetBytes(wire, "messages").String(), "run it") {
				t.Fatalf("messages = %s", wire)
			}
			if gjson.GetBytes(wire, "tools.0.function.name").String() != "echo_nonce" {
				t.Fatalf("tools = %s", wire)
			}
		}
	})
}

func TestCodeBuddyAPIFailureMatrix(t *testing.T) {
	exec := executor.NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyCompatAuth()
	cases := []struct {
		name      string
		status    int
		body      string
		wantCode  int
		reqScope  bool
		credScope bool
	}{
		{"prompt too long", 200, `{"code":11115,"msg":"prompt is too long"}`, 400, true, false},
		{"trial", 403, `{"code":14017,"msg":"trial not activated"}`, 403, false, true},
		{"rate 11140", 200, `{"code":11140,"msg":"rate-limiting requests"}`, 429, false, false},
		{"offline session", 200, `{"code":12153,"msg":"Offline user session not found"}`, 401, false, true},
		{"policy", 400, `{"code":0,"msg":"blocked by security policy"}`, 400, true, false},
		{"waf", 403, `<html>blocked</html>`, 403, false, false},
	}
	for _, tc := range cases {
		transport := &codeBuddyCompatTransport{respond: func(_ []byte) *http.Response {
			if tc.status == 200 && strings.Contains(tc.body, `"code":`) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + tc.body + "\n\n"))}
			}
			return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body))}
		}}
		ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
		payload := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
		req := cliproxyexecutor.Request{Model: "codebuddy-global-hy4-preview", Payload: []byte(payload)}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(payload)}
		_, err := exec.Execute(ctx, auth, req, opts)
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		typed, ok := err.(*helps.CodeBuddyError)
		if !ok {
			t.Fatalf("%s: type = %T", tc.name, err)
		}
		if typed.StatusCode() != tc.wantCode || typed.IsRequestScoped() != tc.reqScope || typed.IsCredentialScoped() != tc.credScope {
			t.Fatalf("%s: got %d req=%v cred=%v", tc.name, typed.StatusCode(), typed.IsRequestScoped(), typed.IsCredentialScoped())
		}
	}
}
