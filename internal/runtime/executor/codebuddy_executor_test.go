package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type codeBuddyRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f codeBuddyRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func codeBuddyTestAuth() *cliproxyauth.Auth {
	tools := true
	reasoning := true
	creds := codebuddy.Credentials{
		Realm: codebuddy.RealmGlobal, AccessToken: "SYNTHETIC_ACCESS", RefreshToken: "SYNTHETIC_REFRESH",
		UID: "fixture-user", Domain: "www.codebuddy.ai", DeviceToken: "SYNTHETIC_DEVICE",
		MachineID: "mid", SessionID: "sid", Expired: time.Now().Add(time.Hour),
	}
	catalog := codebuddy.Catalog{Models: []codebuddy.Model{{
		ID: "hy4-preview", Name: "HY4", MaxInputTokens: 1000000, MaxOutputTokens: 64000,
		SupportsTools: &tools, SupportsReasoning: &reasoning,
		Efforts: []string{"high"}, DefaultEffort: "high",
	}}}
	return &cliproxyauth.Auth{ID: "codebuddy-test", Provider: "codebuddy", Metadata: codebuddy.Metadata(creds, catalog, time.Now())}
}

func codeBuddyTestContext(rt http.RoundTripper) context.Context {
	return context.WithValue(context.Background(), "cliproxy.roundtripper", rt)
}

func codeBuddySSEResponse(sse string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(sse)),
	}
}

func readCodeBuddyHelperFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("helps/testdata/codebuddy/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func codeBuddyOpenAIRequest(payload string) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	return cliproxyexecutor.Request{Model: "codebuddy-global-hy4-preview", Payload: []byte(payload)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(payload)}
}

func TestCodeBuddyExecutorNonStream(t *testing.T) {
	sse := readCodeBuddyHelperFixture(t, "text.sse")
	var wireBody []byte
	var wireURL string
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		wireURL = req.URL.String()
		raw, _ := io.ReadAll(req.Body)
		wireBody = raw
		return codeBuddySSEResponse(sse), nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	payload := `{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"hi"}],"stream":false}`
	req, opts := codeBuddyOpenAIRequest(payload)
	resp, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if wireURL != "https://www.codebuddy.ai/v2/chat/completions" {
		t.Fatalf("url = %s", wireURL)
	}
	if gjson.GetBytes(wireBody, "stream").Bool() != true || gjson.GetBytes(wireBody, "model").String() != "hy4-preview" {
		t.Fatalf("wire = %s", wireBody)
	}
	if gjson.GetBytes(resp.Payload, "choices.0.message.content").String() != "Hello" {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if gjson.GetBytes(resp.Payload, "usage.total_tokens").Int() != 9 {
		t.Fatalf("usage = %s", resp.Payload)
	}
	if resp.Metadata["codebuddy_upstream_model"] != "hy4-preview" || resp.Metadata["codebuddy_returned_model"] != "hy4-preview" {
		t.Fatalf("metadata = %v", resp.Metadata)
	}
}

func TestCodeBuddyExecutorStream(t *testing.T) {
	sse := readCodeBuddyHelperFixture(t, "text.sse")
	requests := 0
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return codeBuddySSEResponse(sse), nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	req, opts := codeBuddyOpenAIRequest(`{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"hi"}]}`)
	result, err := executor.ExecuteStream(ctx, codeBuddyTestAuth(), req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var payloads []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("chunk err: %v", chunk.Err)
		}
		payloads = append(payloads, string(chunk.Payload))
	}
	if requests != 1 {
		t.Fatalf("upstream requests = %d", requests)
	}
	joined := strings.Join(payloads, "\n")
	if !strings.Contains(joined, "Hello") {
		t.Fatalf("chunks = %s", joined)
	}
	// One terminal finish event; the API layer appends the SSE [DONE] marker.
	if count := strings.Count(joined, `"finish_reason":"stop"`); count != 1 {
		t.Fatalf("finish events = %d in %s", count, joined)
	}
}

func TestCodeBuddyExecutorFormats(t *testing.T) {
	sse := readCodeBuddyHelperFixture(t, "text.sse")
	run := func(t *testing.T, format sdktranslator.Format, payload string) ([]byte, []byte) {
		var wire []byte
		ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			wire = raw
			return codeBuddySSEResponse(sse), nil
		}))
		executor := NewCodeBuddyExecutor(&config.Config{})
		req := cliproxyexecutor.Request{Model: "codebuddy-global-hy4-preview", Payload: []byte(payload)}
		opts := cliproxyexecutor.Options{SourceFormat: format, OriginalRequest: []byte(payload)}
		resp, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		return wire, resp.Payload
	}
	t.Run("responses", func(t *testing.T) {
		wire, out := run(t, sdktranslator.FormatOpenAIResponse, `{"model":"codebuddy-global-hy4-preview","input":"hi"}`)
		if gjson.GetBytes(wire, "messages").String() == "" {
			t.Fatalf("wire not openai: %s", wire)
		}
		if gjson.GetBytes(out, "object").String() != "response" {
			t.Fatalf("out = %s", out)
		}
	})
	t.Run("claude", func(t *testing.T) {
		wire, out := run(t, sdktranslator.FormatClaude, `{"model":"codebuddy-global-hy4-preview","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
		if gjson.GetBytes(wire, "model").String() != "hy4-preview" {
			t.Fatalf("wire = %s", wire)
		}
		if gjson.GetBytes(out, "type").String() != "message" {
			t.Fatalf("out = %s", out)
		}
	})
}

func TestCodeBuddyExecutorReasoningTraceRoundTrip(t *testing.T) {
	toolCallSSE := func(callID, trace string) string {
		return "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"" + trace + "\"}}]}\n\n" +
			"data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"" + callID + "\",\"type\":\"function\",\"function\":{\"name\":\"echo_nonce\",\"arguments\":\"{}\"}}]}}]}\n\n" +
			"data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	}
	followUp := func(callID string) string {
		return `{"model":"codebuddy-global-hy4-preview","messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"` + callID + `","type":"function","function":{"name":"echo_nonce","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"` + callID + `","content":"ok"}],
			"tools":[{"type":"function","function":{"name":"echo_nonce"}}]}`
	}
	run := func(t *testing.T, authID, callID, trace string, stream bool) []byte {
		t.Helper()
		auth := codeBuddyTestAuth()
		auth.ID = authID
		var wires [][]byte
		calls := 0
		ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			wires = append(wires, raw)
			calls++
			if calls == 1 {
				return codeBuddySSEResponse(toolCallSSE(callID, trace)), nil
			}
			return codeBuddySSEResponse(readCodeBuddyHelperFixture(t, "text.sse")), nil
		}))
		executor := NewCodeBuddyExecutor(&config.Config{})
		first := `{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"echo_nonce"}}],"tool_choice":"auto"}`
		req, opts := codeBuddyOpenAIRequest(first)
		if stream {
			result, err := executor.ExecuteStream(ctx, auth, req, opts)
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("chunk: %v", chunk.Err)
				}
			}
		} else if _, err := executor.Execute(ctx, auth, req, opts); err != nil {
			t.Fatalf("first: %v", err)
		}
		req2, opts2 := codeBuddyOpenAIRequest(followUp(callID))
		if _, err := executor.Execute(ctx, auth, req2, opts2); err != nil {
			t.Fatalf("follow-up: %v", err)
		}
		if len(wires) != 2 {
			t.Fatalf("upstream requests = %d", len(wires))
		}
		return wires[1]
	}
	t.Run("non-stream records trace", func(t *testing.T) {
		wire := run(t, "codebuddy-reasoning-loop", "call_rc_loop_1", "loop trace", false)
		if rc := gjson.GetBytes(wire, "messages.2.reasoning_content").String(); rc != "loop trace" {
			t.Fatalf("restored reasoning_content = %q in %s", rc, wire)
		}
	})
	t.Run("stream records trace", func(t *testing.T) {
		wire := run(t, "codebuddy-reasoning-loop-stream", "call_rc_loop_2", "stream loop trace", true)
		if rc := gjson.GetBytes(wire, "messages.2.reasoning_content").String(); rc != "stream loop trace" {
			t.Fatalf("restored reasoning_content = %q in %s", rc, wire)
		}
	})
}

func TestCodeBuddyExecutorRejectsUnsupportedSourceTools(t *testing.T) {
	cases := []struct {
		name    string
		format  sdktranslator.Format
		payload string
		want    string
	}{
		{"responses web search", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"web_search_preview"}]}`, "web_search_preview"},
		{"responses file search", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"file_search"}]}`, "file_search"},
		{"claude web search", sdktranslator.FormatClaude, `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, "web_search_20250305"},
		{"chat custom tool", sdktranslator.FormatOpenAI, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"f"}]}`, "custom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contacted := false
			ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(_ *http.Request) (*http.Response, error) {
				contacted = true
				return codeBuddySSEResponse("data: [DONE]\n\n"), nil
			}))
			executor := NewCodeBuddyExecutor(&config.Config{})
			req := cliproxyexecutor.Request{Model: "codebuddy-global-hy4-preview", Payload: []byte(tc.payload)}
			opts := cliproxyexecutor.Options{SourceFormat: tc.format, OriginalRequest: []byte(tc.payload)}
			_, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts)
			if err == nil {
				t.Fatal("unsupported server tool accepted")
			}
			typed, ok := err.(*helps.CodeBuddyError)
			if !ok || !typed.IsRequestScoped() || typed.StatusCode() != http.StatusBadRequest {
				t.Fatalf("error = %#v", err)
			}
			if !strings.Contains(typed.Message, tc.want) {
				t.Fatalf("message = %q, want %q", typed.Message, tc.want)
			}
			if contacted {
				t.Fatal("upstream contacted for a request that must fail before translation")
			}
		})
	}
}

func TestCodeBuddyExecutorToolsRoundTrip(t *testing.T) {
	toolsSSE := readCodeBuddyHelperFixture(t, "tools.sse")
	var wires [][]byte
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		wires = append(wires, raw)
		if len(wires) == 1 {
			if gjson.GetBytes(raw, "tool_choice").String() != "echo_nonce" {
				t.Fatalf("tool_choice = %s", raw)
			}
			return codeBuddySSEResponse("data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"echo_nonce\",\"arguments\":\"{\\\"nonce\\\":\\\"HY4_TOOL_OK\\\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"), nil
		}
		return codeBuddySSEResponse(readCodeBuddyHelperFixture(t, "text.sse")), nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth()
	first := `{"model":"codebuddy-global-hy4-preview","messages":[{"role":"user","content":"go"}],"tools":[{"type":"function","function":{"name":"echo_nonce","parameters":{"type":"object","properties":{"nonce":{"type":"string"}},"required":["nonce"],"additionalProperties":false}}}],"tool_choice":{"type":"function","function":{"name":"echo_nonce"}}}`
	req, opts := codeBuddyOpenAIRequest(first)
	resp, err := executor.Execute(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if gjson.GetBytes(resp.Payload, "choices.0.message.tool_calls.0.id").String() != "call_1" {
		t.Fatalf("calls = %s", resp.Payload)
	}
	_ = toolsSSE
	second := `{"model":"codebuddy-global-hy4-preview","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo_nonce","arguments":"{\"nonce\":\"HY4_TOOL_OK\"}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"HY4_TOOL_OK"}]}`
	req2, opts2 := codeBuddyOpenAIRequest(second)
	resp2, err := executor.Execute(ctx, auth, req2, opts2)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if gjson.GetBytes(resp2.Payload, "choices.0.finish_reason").String() != "stop" {
		t.Fatalf("second = %s", resp2.Payload)
	}
	if len(wires) != 2 {
		t.Fatalf("requests = %d", len(wires))
	}
}

func TestCodeBuddyExecutorThinking(t *testing.T) {
	sse := readCodeBuddyHelperFixture(t, "text.sse")
	run := func(t *testing.T, model, payload string) ([]byte, error) {
		var wire []byte
		ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			wire = raw
			return codeBuddySSEResponse(sse), nil
		}))
		executor := NewCodeBuddyExecutor(&config.Config{})
		req := cliproxyexecutor.Request{Model: model, Payload: []byte(payload)}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(payload)}
		_, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts)
		return wire, err
	}
	t.Run("suffix overrides body", func(t *testing.T) {
		wire, err := run(t, "codebuddy-global-hy4-preview(high)", `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if gjson.GetBytes(wire, "reasoning_effort").String() != "high" {
			t.Fatalf("wire = %s", wire)
		}
	})
	t.Run("unsupported rejected", func(t *testing.T) {
		if _, err := run(t, "codebuddy-global-hy4-preview", `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"ultra"}`); err == nil {
			t.Fatal("unsupported effort accepted")
		}
	})
	t.Run("known level clamped per canonical rules", func(t *testing.T) {
		wire, err := run(t, "codebuddy-global-hy4-preview", `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if gjson.GetBytes(wire, "reasoning_effort").String() != "high" {
			t.Fatalf("wire = %s", wire)
		}
	})
	t.Run("reasoning preserved", func(t *testing.T) {
		var wire []byte
		ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			wire = raw
			return codeBuddySSEResponse(readCodeBuddyHelperFixture(t, "reasoning.sse")), nil
		}))
		executor := NewCodeBuddyExecutor(&config.Config{})
		req, opts := codeBuddyOpenAIRequest(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		resp, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		_ = wire
		if gjson.GetBytes(resp.Payload, "choices.0.message.content").String() != "42" {
			t.Fatalf("payload = %s", resp.Payload)
		}
		if !strings.Contains(string(resp.Payload), "Synthetic reasoning") {
			t.Fatalf("reasoning lost: %s", resp.Payload)
		}
	})
}

type codeBuddyBlockingBody struct {
	ctx    context.Context
	chunks []string
	pos    int
	closed chan struct{}
}

func (b *codeBuddyBlockingBody) Read(out []byte) (int, error) {
	if b.pos < len(b.chunks) {
		n := copy(out, b.chunks[b.pos])
		b.chunks[b.pos] = b.chunks[b.pos][n:]
		if len(b.chunks[b.pos]) == 0 {
			b.pos++
		}
		return n, nil
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	}
}

func (b *codeBuddyBlockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestCodeBuddyExecutorCancel(t *testing.T) {
	closed := make(chan struct{})
	var body *codeBuddyBlockingBody
	baseCtx, cancel := context.WithCancel(context.Background())
	ctx := context.WithValue(baseCtx, "cliproxy.roundtripper", codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body = &codeBuddyBlockingBody{ctx: req.Context(), chunks: []string{"data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\n"}, closed: closed}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body}, nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	req, opts := codeBuddyOpenAIRequest(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	result, err := executor.ExecuteStream(ctx, codeBuddyTestAuth(), req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("channel closed before first chunk")
		}
		if chunk.Err != nil {
			t.Fatalf("first chunk err: %v", chunk.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no first chunk")
	}
	cancel()
	for range result.Chunks {
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream body not closed after cancel")
	}
}

func TestCodeBuddyExecutorRefresh(t *testing.T) {
	var refreshHeaders http.Header
	var paths []string
	var hosts []string
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		hosts = append(hosts, req.URL.Host)
		refreshHeaders = req.Header.Clone()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"code":0,"msg":"ok","data":{"accessToken":"NEW_ACCESS","refreshToken":"NEW_REFRESH","expiresIn":3600}}`))}, nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth()
	auth.Attributes = map[string]string{"proxy_url": "http://proxy.test"}
	clone, err := executor.Refresh(ctx, auth)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if clone.Metadata["access_token"] != "NEW_ACCESS" || clone.Metadata["refresh_token"] != "NEW_REFRESH" {
		t.Fatalf("tokens = %v", clone.Metadata)
	}
	if _, err := codebuddy.CatalogFromMetadata(clone.Metadata); err != nil {
		t.Fatalf("catalog lost: %v", err)
	}
	if clone.Attributes["proxy_url"] != "http://proxy.test" {
		t.Fatal("user metadata lost")
	}
	if auth.Metadata["access_token"] != "SYNTHETIC_ACCESS" {
		t.Fatal("original auth mutated")
	}
	if len(paths) != 1 || paths[0] != "/v2/plugin/auth/token/refresh" {
		t.Fatalf("paths = %v", paths)
	}
	if len(hosts) != 1 || hosts[0] != "www.codebuddy.ai" {
		t.Fatalf("refresh not realm-bound: %v", hosts)
	}
	if refreshHeaders.Get("X-Refresh-Token") != "SYNTHETIC_REFRESH" {
		t.Fatalf("headers = %v", refreshHeaders)
	}
}

func TestCodeBuddyExecutorMidstreamFailure(t *testing.T) {
	raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"code\":11115,\"msg\":\"prompt is too long\"}\n\n"
	requests := 0
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return codeBuddySSEResponse(raw), nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	req, opts := codeBuddyOpenAIRequest(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts); err == nil {
		t.Fatal("non-stream swallowed midstream error")
	}
	requests = 0
	ctx2 := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return codeBuddySSEResponse(raw), nil
	}))
	result, err := executor.ExecuteStream(ctx2, codeBuddyTestAuth(), req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream: %v", err)
	}
	var sawContent, sawErr, sawDone bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			sawErr = true
			continue
		}
		if strings.Contains(string(chunk.Payload), "Hi") {
			sawContent = true
		}
		if strings.Contains(string(chunk.Payload), "[DONE]") {
			sawDone = true
		}
	}
	if !sawContent || !sawErr || sawDone {
		t.Fatalf("content=%v err=%v done=%v", sawContent, sawErr, sawDone)
	}
	if requests != 1 {
		t.Fatalf("requests = %d", requests)
	}
}

func TestCodeBuddyExecutorEmptyStreamIsUpstreamFailure(t *testing.T) {
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return codeBuddySSEResponse(""), nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	req, opts := codeBuddyOpenAIRequest(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	_, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts)
	typed, ok := err.(*helps.CodeBuddyError)
	if !ok || typed.StatusCode() != http.StatusBadGateway {
		t.Fatalf("err = %#v", err)
	}
	if _, err := executor.ExecuteStream(ctx, codeBuddyTestAuth(), req, opts); err == nil {
		t.Fatal("stream pre-read accepted empty stream")
	} else if typed, ok := err.(*helps.CodeBuddyError); !ok || typed.StatusCode() != http.StatusBadGateway {
		t.Fatalf("stream err = %#v", err)
	}
}

func TestCodeBuddyExecutorIdentity(t *testing.T) {
	t.Run("mismatch fails before emit", func(t *testing.T) {
		raw := "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"model\":\"other-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"evil\"}}]}\n\n"
		ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return codeBuddySSEResponse(raw), nil
		}))
		executor := NewCodeBuddyExecutor(&config.Config{})
		req, opts := codeBuddyOpenAIRequest(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		_, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts)
		if err == nil || !strings.Contains(err.Error(), "model_mismatch") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("absent model recorded absent", func(t *testing.T) {
		raw := "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return codeBuddySSEResponse(raw), nil
		}))
		executor := NewCodeBuddyExecutor(&config.Config{})
		req, opts := codeBuddyOpenAIRequest(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		resp, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts)
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
		if _, ok := resp.Metadata["codebuddy_returned_model"]; ok {
			t.Fatalf("metadata = %v", resp.Metadata)
		}
	})
}

func TestCodeBuddyExecutorNoSecretLogging(t *testing.T) {
	var buf bytes.Buffer
	previous := log.StandardLogger().Out
	log.SetOutput(&buf)
	defer log.SetOutput(previous)
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return codeBuddySSEResponse(readCodeBuddyHelperFixture(t, "text.sse")), nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	req, opts := codeBuddyOpenAIRequest(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := executor.Execute(ctx, codeBuddyTestAuth(), req, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	badCtx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 403, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"code":11140,"msg":"request illegal"}`))}, nil
	}))
	_, _ = executor.Execute(badCtx, codeBuddyTestAuth(), req, opts)
	out := buf.String()
	for _, secret := range []string{"SYNTHETIC_ACCESS", "SYNTHETIC_REFRESH", "SYNTHETIC_DEVICE"} {
		if strings.Contains(out, secret) {
			t.Fatalf("secret %s in logs", secret)
		}
	}
}

func TestCodeBuddyExecutorCountTokensUnsupported(t *testing.T) {
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("upstream request issued")
		return nil, nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	req, opts := codeBuddyOpenAIRequest(`{"model":"m","messages":[]}`)
	_, err := executor.CountTokens(ctx, codeBuddyTestAuth(), req, opts)
	typed, ok := err.(*helps.CodeBuddyError)
	if !ok || typed.StatusCode() != 501 || !typed.IsRequestScoped() {
		t.Fatalf("err = %v", err)
	}
}

func TestCodeBuddyExecutorHttpRequestHostBoundary(t *testing.T) {
	calls := 0
	ctx := codeBuddyTestContext(codeBuddyRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	executor := NewCodeBuddyExecutor(&config.Config{})
	auth := codeBuddyTestAuth()
	build := func(method, rawURL string) *http.Request {
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		return req
	}
	for name, req := range map[string]*http.Request{
		"attacker host":  build(http.MethodGet, "https://attacker.test/v3/config"),
		"userinfo":       build(http.MethodGet, "https://user@www.codebuddy.ai/v3/config"),
		"http downgrade": build(http.MethodGet, "http://www.codebuddy.ai/v3/config"),
		"refresh route":  build(http.MethodPost, "https://www.codebuddy.ai/v2/plugin/auth/token/refresh"),
		"chat get":       build(http.MethodGet, "https://www.codebuddy.ai/v2/chat/completions"),
		"catalog post":   build(http.MethodPost, "https://www.codebuddy.ai/v3/config"),
		"unknown path":   build(http.MethodGet, "https://www.codebuddy.ai/v2/admin"),
		"cn host":        build(http.MethodGet, "https://copilot.tencent.com/v3/config"),
	} {
		if _, err := executor.HttpRequest(ctx, auth, req); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := executor.HttpRequest(ctx, auth, nil); err == nil {
		t.Fatal("nil accepted")
	}
	if calls != 0 {
		t.Fatalf("calls = %d", calls)
	}
	resp, err := executor.HttpRequest(ctx, auth, build(http.MethodGet, "https://www.codebuddy.ai/v3/config"))
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	_ = resp.Body.Close()
	chatReq := build(http.MethodPost, "https://www.codebuddy.ai/v2/chat/completions")
	chatReq.Header.Set("Authorization", "Bearer EVIL")
	resp, err = executor.HttpRequest(ctx, auth, chatReq)
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	_ = resp.Body.Close()
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}
