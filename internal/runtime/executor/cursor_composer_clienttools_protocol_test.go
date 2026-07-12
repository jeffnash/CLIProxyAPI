package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// This file holds the addendum-6 "Harness A" Go executor protocol/contract tests (RBT-011/012/013/035) plus
// the multi-replica integration test (Comment 2 / RBT-017) and a couple of verify-only pins. Each uses a
// programmable httptest.Server as the fake /agent/turn bridge and drives the REAL executor — never the live
// Cursor SDK. The shared contract: a misrouted/unknown/protocol-violating bridge response must DEGRADE with a
// typed error, NEVER a clean end_turn / empty completion / native success.

// composerProtoBridge is a scriptable fake bridge: it captures the last request body and replays a fixed
// status + headers + body. When sse is true it sets Content-Type: text/event-stream.
type composerProtoBridge struct {
	mu       sync.Mutex
	lastBody []byte
	status   int
	ctype    string
	body     string
	srv      *httptest.Server
}

func newComposerProtoBridge(t *testing.T, status int, ctype, body string) *composerProtoBridge {
	t.Helper()
	b := &composerProtoBridge{status: status, ctype: ctype, body: body}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.lastBody = raw
		status, ctype, body := b.status, b.ctype, b.body
		b.mu.Unlock()
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *composerProtoBridge) lastRequestBody() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.lastBody...)
}

func protoAuth(bridgeURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: "protoT", Attributes: map[string]string{"api_key": "k", "composer_client_tools_bridge_url": bridgeURL}}
}

func protoOpts(conv string) cliproxyexecutor.Options {
	return composerExecOpts("openai", conv)
}

func protoToolReq(text string) cliproxyexecutor.Request {
	payload := []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"` + text + `"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)
	return cliproxyexecutor.Request{Model: "composer-2.5", Payload: payload}
}

func TestClaudeHeaderlessAndInvalidWorkspaceReachBridgeAsNeutralTurns(t *testing.T) {
	body := "data: {\"type\":\"text\",\"delta\":\"repo summary\"}\n\ndata: {\"type\":\"turn_end\",\"stop_reason\":\"stop\"}\n\ndata: [DONE]\n\n"
	bridge := newComposerProtoBridge(t, http.StatusOK, "text/event-stream", body)
	executor := NewCursorExecutor(&config.Config{})
	auth := protoAuth(bridge.srv.URL)
	req := cliproxyexecutor.Request{
		Model: "composer-2.5",
		Payload: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"tell me about this repo"}],` +
			`"tools":[{"name":"Read","description":"read a local file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`),
	}

	cases := []struct {
		name string
		opts cliproxyexecutor.Options
	}{
		{
			name: "headerless",
			opts: func() cliproxyexecutor.Options {
				opts := composerExecOpts("claude", "headerless-claude")
				opts.Headers.Del("X-Cwd")
				opts.Headers.Del("X-Workspace-Path")
				return opts
			}(),
		},
		{
			name: "invalid path",
			opts: func() cliproxyexecutor.Options {
				opts := composerExecOpts("claude", "invalid-workspace-claude")
				opts.Headers.Set("X-Cwd", "relative/client/path")
				opts.Headers.Set("X-Workspace-Path", "/app/proxy-path-must-not-leak")
				return opts
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := executor.executeComposer(context.Background(), auth, "k", req, tc.opts)
			if err != nil {
				t.Fatalf("Claude turn failed: %v", err)
			}
			if !strings.Contains(gjson.GetBytes(resp.Payload, "content.0.text").String(), "repo summary") {
				t.Fatalf("Claude response lost model output: %s", resp.Payload)
			}
			got := bridge.lastRequestBody()
			if !gjson.GetBytes(got, "clientEnv.workspaceUnknown").Bool() {
				t.Fatalf("workspace did not degrade to neutral: %s", got)
			}
			for _, forbidden := range []string{`"processWorkingDirectory"`, `"workspacePaths"`, "/workspace", "/app/proxy-path-must-not-leak"} {
				if strings.Contains(string(got), forbidden) {
					t.Fatalf("neutral Claude request leaked %q: %s", forbidden, got)
				}
			}
		})
	}
}

func TestHarnessProtocolsRoundTripSignedClientToolResultsThroughContinue(t *testing.T) {
	const signedID = "cct1_AAAAAAAAAAAAAAAAAAAAAA_0_BBBBBBBBBBBBBBBBBBBBBB"
	cases := []struct {
		name         string
		source       string
		firstPayload string
		continuation string
	}{
		{
			name:   "OpenAI chat completions",
			source: "openai",
			firstPayload: `{"model":"composer-2.5","messages":[{"role":"user","content":"read README"}],` +
				`"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`,
			continuation: `{"model":"composer-2.5","messages":[` +
				`{"role":"user","content":"read README"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"` + signedID + `","type":"function","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]},` +
				`{"role":"tool","tool_call_id":"` + signedID + `","content":"README contents"}],` +
				`"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`,
		},
		{
			name:   "Anthropic messages",
			source: "claude",
			firstPayload: `{"model":"composer-2.5","max_tokens":512,"messages":[{"role":"user","content":"read README"}],` +
				`"tools":[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`,
			continuation: `{"model":"composer-2.5","max_tokens":512,"messages":[` +
				`{"role":"user","content":"read README"},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"` + signedID + `","name":"Read","input":{"path":"README.md"}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + signedID + `","content":"README contents"}]}],` +
				`"tools":[{"name":"Read","input_schema":{"type":"object"}}]}`,
		},
		{
			name:   "OpenAI Responses",
			source: "openai-response",
			firstPayload: `{"model":"composer-2.5","input":[{"role":"user","content":[{"type":"input_text","text":"read README"}]}],` +
				`"tools":[{"type":"function","name":"Read","description":"read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]}`,
			continuation: `{"model":"composer-2.5","input":[` +
				`{"role":"user","content":[{"type":"input_text","text":"read README"}]},` +
				`{"type":"function_call","id":"fc_1","call_id":"` + signedID + `","name":"Read","arguments":"{\"path\":\"README.md\"}","status":"completed"},` +
				`{"type":"function_call_output","call_id":"` + signedID + `","output":"README contents"}],` +
				`"tools":[{"type":"function","name":"Read","parameters":{"type":"object"}}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var paths []string
			var bodies [][]byte
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				mu.Lock()
				paths = append(paths, r.URL.Path)
				bodies = append(bodies, append([]byte(nil), raw...))
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if r.URL.Path == composerAgentContinuePath {
					_, _ = io.WriteString(w, "data: {\"type\":\"text\",\"delta\":\"continued after local tool\"}\n\n"+
						"data: {\"type\":\"turn_end\",\"stop_reason\":\"stop\"}\n\ndata: [DONE]\n\n")
					return
				}
				_, _ = io.WriteString(w, "data: {\"type\":\"tool_call\",\"id\":\""+signedID+"\",\"name\":\"Read\",\"input\":{\"path\":\"README.md\"}}\n\n"+
					"data: {\"type\":\"turn_end\",\"stop_reason\":\"tool_use\"}\n\ndata: [DONE]\n\n")
			}))
			defer bridge.Close()

			executor := NewCursorExecutor(&config.Config{})
			auth := protoAuth(bridge.URL)
			opts := composerExecOpts(tc.source, "roundtrip-"+strings.ReplaceAll(tc.name, " ", "-"))
			first, err := executor.executeComposer(context.Background(), auth, "k", cliproxyexecutor.Request{
				Model: "composer-2.5", Payload: []byte(tc.firstPayload),
			}, opts)
			if err != nil {
				t.Fatalf("first turn: %v", err)
			}
			if !strings.Contains(string(first.Payload), signedID) {
				t.Fatalf("harness response lost signed tool id: %s", first.Payload)
			}

			second, err := executor.executeComposer(context.Background(), auth, "k", cliproxyexecutor.Request{
				Model: "composer-2.5", Payload: []byte(tc.continuation),
			}, opts)
			if err != nil {
				t.Fatalf("continuation: %v", err)
			}
			if !strings.Contains(string(second.Payload), "continued after local tool") {
				t.Fatalf("final harness response lost resumed text: %s", second.Payload)
			}

			mu.Lock()
			gotPaths := append([]string(nil), paths...)
			gotBodies := append([][]byte(nil), bodies...)
			mu.Unlock()
			if len(gotPaths) != 2 || gotPaths[0] != composerAgentTurnPath || gotPaths[1] != composerAgentContinuePath {
				t.Fatalf("bridge paths = %v, want [/agent/turn /agent/continue]", gotPaths)
			}
			if got := gjson.GetBytes(gotBodies[1], "input.results.0.toolCallId").String(); got != signedID {
				t.Fatalf("continuation signed id = %q, want %q; body=%s", got, signedID, gotBodies[1])
			}
			if got := gjson.GetBytes(gotBodies[1], "input.results.0.content").String(); !strings.Contains(got, "README contents") {
				t.Fatalf("continuation lost local tool content: %s", gotBodies[1])
			}
		})
	}
}

// drainStreamErr collects a StreamResult and returns the first chunk error (or nil) plus the concatenated
// non-error payload. It never blocks indefinitely — the executor closes the channel on completion/error.
func drainStreamErr(sr *cliproxyexecutor.StreamResult) (error, string) {
	var firstErr error
	var b strings.Builder
	for chunk := range sr.Chunks {
		if chunk.Err != nil && firstErr == nil {
			firstErr = chunk.Err
		}
		if chunk.Err == nil {
			b.Write(chunk.Payload)
		}
	}
	return firstErr, b.String()
}

// RBT-011 — a bridge HTTP 410 (lost/expired continuation) must survive to the client as a typed status error
// preserving 410 (composerBridgeStatusError), NOT a generic 500 and NOT a clean success — on BOTH paths.
func TestRBT011_Bridge410PreservesStatus(t *testing.T) {
	br := newComposerProtoBridge(t, 410, "text/plain", "unknown or expired session")
	e := NewCursorExecutor(&config.Config{})
	auth := protoAuth(br.srv.URL)

	// Non-stream.
	_, err := e.executeComposer(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt011-ns"))
	if err == nil {
		t.Fatalf("RBT-011: a bridge 410 must surface as an error, not a clean success")
	}
	var se cliproxyexecutor.StatusError
	if !errors.As(err, &se) || se.StatusCode() != 410 {
		t.Fatalf("RBT-011: non-stream 410 must preserve status 410, got %T %v", err, err)
	}

	// Stream (the 410 is detected before the goroutine opens, so it is a direct typed error return).
	_, errS := e.executeComposerStream(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt011-s"))
	if errS == nil {
		t.Fatalf("RBT-011: stream 410 must surface as an error")
	}
	var seS cliproxyexecutor.StatusError
	if !errors.As(errS, &seS) || seS.StatusCode() != 410 {
		t.Fatalf("RBT-011: stream 410 must preserve status 410, got %T %v", errS, errS)
	}
}

// RBT-012 — a 2xx response that is NOT text/event-stream (HTML login page, JSON {}, empty body) must be a
// protocol error on BOTH paths, never a clean empty assistant completion.
func TestRBT012_Non2xxSSEIsProtocolError(t *testing.T) {
	e := NewCursorExecutor(&config.Config{})
	cases := []struct {
		name  string
		ctype string
		body  string
	}{
		{"html", "text/html", "<html>login</html>"},
		{"json", "application/json", "{}"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		br := newComposerProtoBridge(t, 200, c.ctype, c.body)
		auth := protoAuth(br.srv.URL)

		// Non-stream: must be a typed protocol error (502), never an empty 200 body.
		resp, err := e.executeComposer(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt012-ns-"+c.name))
		if err == nil {
			t.Fatalf("RBT-012/%s: non-stream non-SSE 2xx must be a protocol error, got clean response %q", c.name, string(resp.Payload))
		}
		var se cliproxyexecutor.StatusError
		if !errors.As(err, &se) || se.StatusCode() != http.StatusBadGateway {
			t.Fatalf("RBT-012/%s: non-stream must be a 502 protocol error, got %T %v", c.name, err, err)
		}
		var pe *composerBridgeProtocolError
		if !errors.As(err, &pe) {
			t.Fatalf("RBT-012/%s: non-stream must be a composerBridgeProtocolError, got %T", c.name, err)
		}

		// Stream: the content-type gate runs before the goroutine, so it is a direct typed error return.
		_, errS := e.executeComposerStream(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt012-s-"+c.name))
		if errS == nil {
			t.Fatalf("RBT-012/%s: stream non-SSE 2xx must be a protocol error, not a clean empty stream", c.name)
		}
		var peS *composerBridgeProtocolError
		if !errors.As(errS, &peS) {
			t.Fatalf("RBT-012/%s: stream must be a composerBridgeProtocolError, got %T %v", c.name, errS, errS)
		}
	}
}

// RBT-012b — a clean SSE EOF that never delivered a terminal turn_end must be a protocol error (the body
// closed with no terminal), not a synthetic [DONE] / empty completion.
func TestRBT012_EOFWithoutTerminalIsProtocolError(t *testing.T) {
	e := NewCursorExecutor(&config.Config{})
	// Valid SSE content type, a partial model frame + ping, then EOF — no
	// turn_end. The streaming client may see the comment, but never the partial
	// assistant bytes held behind the durable commit barrier.
	body := "data: {\"type\":\"session\",\"sessionId\":\"s\"}\n\ndata: {\"type\":\"text\",\"delta\":\"PARTIAL-MUST-NOT-COMMIT\"}\n\ndata: {\"type\":\"ping\"}\n\n"
	br := newComposerProtoBridge(t, 200, "text/event-stream", body)
	auth := protoAuth(br.srv.URL)

	_, err := e.executeComposer(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt012b-ns"))
	if err == nil {
		t.Fatalf("RBT-012b: non-stream EOF without terminal must be a protocol error")
	}
	var pe *composerBridgeProtocolError
	if !errors.As(err, &pe) || !strings.Contains(err.Error(), "without a terminal") {
		t.Fatalf("RBT-012b: non-stream must be a no-terminal protocol error, got %T %v", err, err)
	}

	sr, errS := e.executeComposerStream(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt012b-s"))
	if errS != nil {
		t.Fatalf("RBT-012b: stream setup must not pre-fail (the gate fires inside the stream): %v", errS)
	}
	streamErr, payload := drainStreamErr(sr)
	if streamErr == nil {
		t.Fatalf("RBT-012b: stream EOF without terminal must surface a chunk error, not a clean stream; payload=%q", payload)
	}
	var peS *composerBridgeProtocolError
	if !errors.As(streamErr, &peS) {
		t.Fatalf("RBT-012b: stream chunk error must be a protocol error, got %T %v", streamErr, streamErr)
	}
	if strings.Contains(payload, "PARTIAL-MUST-NOT-COMMIT") {
		t.Fatalf("RBT-012b: partial model output escaped before durable terminal: %q", payload)
	}
}

// RBT-013 — an unknown/non-benign bridge SSE event type (e.g. version drift renaming tool_call to "toolcall"),
// and an invalid-JSON frame, must each fail closed as a protocol error rather than being silently dropped.
func TestRBT013_UnknownEventIsProtocolError(t *testing.T) {
	e := NewCursorExecutor(&config.Config{})

	// (a) Unknown event type before any terminal.
	unknownBody := "data: {\"type\":\"toolcall\",\"id\":\"x\",\"name\":\"Read\",\"input\":{}}\n\ndata: [DONE]\n\n"
	br := newComposerProtoBridge(t, 200, "text/event-stream", unknownBody)
	auth := protoAuth(br.srv.URL)

	_, err := e.executeComposer(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt013-ns"))
	if err == nil {
		t.Fatalf("RBT-013: non-stream unknown event must be a protocol error, not silently dropped")
	}
	var pe *composerBridgeProtocolError
	if !errors.As(err, &pe) {
		t.Fatalf("RBT-013: non-stream must be a protocol error, got %T %v", err, err)
	}

	sr, errS := e.executeComposerStream(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt013-s"))
	if errS != nil {
		t.Fatalf("RBT-013: stream setup must not pre-fail: %v", errS)
	}
	streamErr, _ := drainStreamErr(sr)
	if streamErr == nil {
		t.Fatalf("RBT-013: stream unknown event must surface a chunk error")
	}
	if pe2 := (*composerBridgeProtocolError)(nil); !errors.As(streamErr, &pe2) {
		t.Fatalf("RBT-013: stream chunk error must be a protocol error, got %T %v", streamErr, streamErr)
	}

	// (b) Invalid-JSON SSE frame must also fail closed (not be dropped as benign).
	badJSON := "data: {\"type\":\"text\",\"delta\":\n\ndata: [DONE]\n\n"
	br.mu.Lock()
	br.body = badJSON
	br.mu.Unlock()
	_, errBad := e.executeComposer(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt013-bad-ns"))
	if errBad == nil {
		t.Fatalf("RBT-013: non-stream invalid-JSON frame must be a protocol error")
	}
	if peBad := (*composerBridgeProtocolError)(nil); !errors.As(errBad, &peBad) {
		t.Fatalf("RBT-013: invalid-JSON frame must be a protocol error, got %T %v", errBad, errBad)
	}

	// (c) A benign telemetry-only stream (session+ping) that DOES end with a real terminal is fine.
	okBody := "data: {\"type\":\"session\",\"sessionId\":\"s\"}\n\ndata: {\"type\":\"text\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"turn_end\",\"stop_reason\":\"stop\"}\n\ndata: [DONE]\n\n"
	br.mu.Lock()
	br.body = okBody
	br.mu.Unlock()
	resp, errOK := e.executeComposer(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt013-ok-ns"))
	if errOK != nil {
		t.Fatalf("RBT-013: a well-formed stream with a terminal must succeed, got %v", errOK)
	}
	if !strings.Contains(gjson.GetBytes(resp.Payload, "choices.0.message.content").String(), "hello") {
		t.Fatalf("RBT-013: a well-formed stream must keep its content, got %s", string(resp.Payload))
	}
}

// RBT-035 — a non-streaming composer response must carry a JSON content type and NONE of the bridge's SSE
// transport headers (text/event-stream), so a strict client SDK does not mis-parse the JSON body as SSE.
func TestRBT035_NonStreamResponseContentTypeIsJSON(t *testing.T) {
	body := "data: {\"type\":\"text\",\"delta\":\"ok\"}\n\ndata: {\"type\":\"turn_end\",\"stop_reason\":\"stop\"}\n\ndata: [DONE]\n\n"
	// The bridge answers with text/event-stream + a cache-control header that must NOT leak.
	br := &composerProtoBridge{status: 200, ctype: "text/event-stream", body: body}
	br.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(body))
	}))
	defer br.srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := protoAuth(br.srv.URL)

	resp, err := e.executeComposer(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("rbt035"))
	if err != nil {
		t.Fatalf("RBT-035: non-stream must succeed, got %v", err)
	}
	ct := resp.Headers.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("RBT-035: non-stream response Content-Type must be application/json, got %q", ct)
	}
	if strings.Contains(strings.ToLower(ct), "text/event-stream") {
		t.Fatalf("RBT-035: non-stream response must NOT carry the bridge's text/event-stream content type")
	}
	// The bridge's Cache-Control must not have been forwarded via the response headers.
	if resp.Headers.Get("Cache-Control") != "" {
		t.Fatalf("RBT-035: bridge transport headers (Cache-Control) must not leak to the client, got %q", resp.Headers.Get("Cache-Control"))
	}
}

// A continuation may land on any Go replica because Go owns no tool-call routing state. Both instances must
// forward the opaque signed id to /agent/continue unchanged; the bridge journal is the sole resolver.
func TestRBT017_MultiReplicaContinuationIsOpaqueBridgeRouting(t *testing.T) {
	const signedID = "cct1_AAAAAAAAAAAAAAAAAAAAAA_0_BBBBBBBBBBBBBBBBBBBBBB"
	// One fake bridge shared by both instances; it records the last path/body so we can assert the wire contract.
	var lastBody []byte
	var lastPath string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = raw
		lastPath = r.URL.Path
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = w.Write([]byte("data: " + s + "\n\n"))
			if fl != nil {
				fl.Flush()
			}
		}
		// On the first turn the bridge emits its own signed id. Go must not decode or remember it.
		if gjson.GetBytes(raw, "input.type").String() == "user" {
			write(`{"type":"tool_call","id":"` + signedID + `","name":"Read","input":{"path":"a.txt"}}`)
			write(`{"type":"turn_end","stop_reason":"tool_use"}`)
		} else {
			write(`{"type":"text","delta":"resumed"}`)
			write(`{"type":"turn_end","stop_reason":"stop"}`)
		}
		write("[DONE]")
	}))
	defer srv.Close()

	instanceA := NewCursorExecutor(&config.Config{})
	instanceB := NewCursorExecutor(&config.Config{})
	auth := protoAuth(srv.URL)
	conv := protoOpts("rbt017-conv")

	// Instance A emits a bridge-signed tool call.
	_, errA := instanceA.executeComposer(context.Background(), auth, "k", protoToolReq("start"), conv)
	if errA != nil {
		t.Fatalf("RBT-017: instance A emitter turn must succeed, got %v", errA)
	}
	sessA, _ := deriveComposerSessionID(auth, "k", protoToolReq("start").Payload, conv)

	// Instance B receives the continuation without any process-local ownership handoff.
	contPayload := []byte(`{"model":"composer-2.5","messages":[` +
		`{"role":"user","content":"read a.txt"},` +
		`{"role":"assistant","tool_calls":[{"id":"` + signedID + `","function":{"name":"Read"}}]},` +
		`{"role":"tool","tool_call_id":"` + signedID + `","content":"file contents"}]}`)
	contReq := cliproxyexecutor.Request{Model: "composer-2.5", Payload: contPayload}
	sessB, errSid := deriveComposerSessionID(auth, "k", contPayload, conv)
	if errSid != nil {
		t.Fatalf("RBT-017: instance B continuation must route, not error, got %v", errSid)
	}
	if !strings.HasPrefix(sessB, "sess_") {
		t.Fatalf("RBT-017: instance B must derive a real session id, got %q", sessB)
	}
	// The advisory session remains deterministic, but correctness is carried by the signed id.
	if sessB != sessA {
		t.Fatalf("RBT-017: advisory session should remain deterministic across replicas (A=%s B=%s)", sessA, sessB)
	}

	resp, errB := instanceB.executeComposer(context.Background(), auth, "k", contReq, conv)
	if errB != nil {
		t.Fatalf("RBT-017: instance B must forward the continuation without local routing state, got %v", errB)
	}
	if resp.Payload == nil {
		t.Fatalf("RBT-017: instance B must produce a response")
	}
	mu.Lock()
	gotBody := append([]byte(nil), lastBody...)
	gotPath := lastPath
	mu.Unlock()
	if gotPath != composerAgentContinuePath {
		t.Fatalf("RBT-017: continuation went to %q, want %q", gotPath, composerAgentContinuePath)
	}
	if it := gjson.GetBytes(gotBody, "input.type").String(); it != "tool_results" {
		t.Fatalf("RBT-017: B's continuation bridge body must be input.type=tool_results, got %q (%s)", it, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.results.0.toolCallId").String() != signedID {
		t.Fatalf("RBT-017: signed id was not forwarded byte-for-byte, got %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.history").String() == "" {
		t.Fatalf("RBT-017: bounded history must remain available for bridge restart recovery, got %s", string(gotBody))
	}
}

// ADD-105 (exec half, VERIFY-ONLY): resolveComposerBridgeURL honors CURSOR_AGENT_BRIDGE_URL, and
// buildComposerTurnURL rejects a non-local http:// base (no code change — this pins the existing behavior the
// bridge's host-binding half depends on). The executor side is verify-only per the cross-file contract.
func TestADD105_BridgeURLEnvHonoredAndNonLocalHTTPRejected(t *testing.T) {
	// Env is honored when no per-auth attribute overrides it.
	t.Setenv("CURSOR_AGENT_BRIDGE_URL", "https://bridge.env.example:8787")
	if got := resolveComposerBridgeURL(&cliproxyauth.Auth{}); got != "https://bridge.env.example:8787" {
		t.Fatalf("ADD-105: CURSOR_AGENT_BRIDGE_URL must be honored, got %q", got)
	}
	// Per-auth attribute still wins over env.
	authAttr := &cliproxyauth.Auth{Attributes: map[string]string{"composer_client_tools_bridge_url": "https://bridge.attr.example"}}
	if got := resolveComposerBridgeURL(authAttr); got != "https://bridge.attr.example" {
		t.Fatalf("ADD-105: per-auth bridge URL must win over env, got %q", got)
	}
	// A non-local http:// base is rejected at request-build time (credential-leak guard the remote bridge needs).
	t.Setenv("CURSOR_AGENT_BRIDGE_URL", "http://bridge.remote.example:8787")
	if _, err := buildComposerTurnURL(&cliproxyauth.Auth{}); err == nil {
		t.Fatalf("ADD-105: a non-local http:// bridge base must be rejected")
	}
}

// ADD-82 (exec half, VERIFY): a built-in tool that is the TARGET of a forced tool_choice is surfaced as
// forced-unavailable — composerConstraints flags the built-in as unsupported (it is never advertised, so the
// model cannot call it), and resolveComposerToolChoice still carries the forced choice. Together this is the
// honest "the forced tool is unavailable" degrade, never a silent drop of a required tool.
func TestADD82_ForcedBuiltinToolChoiceFlaggedUnavailable(t *testing.T) {
	oai := []byte(`{"tools":[{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`)
	c := composerConstraints(oai)
	notes, _ := c["unsupportedHardGuarantees"].([]string)
	joined := strings.Join(notes, " | ")
	if !strings.Contains(joined, "web_search") {
		t.Fatalf("ADD-82: a forced built-in tool must be flagged unsupported (forced-unavailable), got %q", joined)
	}
	// The built-in is not in the advertised function set (no function block), so advertise is empty for it.
	if adv := composerAdvertise(oai); len(adv) != 0 {
		t.Fatalf("ADD-82: a built-in tool must NOT be advertised as a function, got %#v", adv)
	}
}

// P0-1 (reseed-on-410) — per hardening section 8, reseed is DELETED. A LOST tool_results continuation
// that previously would reseed now returns round_lost 410 with message "submitted results were not applied"
// and exactly ONE POST (no automatic retry). This inverts the old reseed test.
func TestP01_Reseed410ContinuationCarryingOpener(t *testing.T) {
	var bodies [][]byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, raw)
		mu.Unlock()
		w.WriteHeader(410)
		_, _ = w.Write([]byte("unknown or expired session"))
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := protoAuth(srv.URL)
	cont := func() cliproxyexecutor.Request {
		return cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[` +
			`{"role":"user","content":"please read a.txt"},` +
			`{"role":"assistant","tool_calls":[{"id":"call_opener_1","function":{"name":"Read"}}]},` +
			`{"role":"tool","tool_call_id":"call_opener_1","content":"file contents"}],` +
			`"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)}
	}
	// Non-stream: must surface 410, not recover
	_, err := e.executeComposer(context.Background(), auth, "k", cont(), protoOpts("p01-ok-ns"))
	if err == nil {
		t.Fatalf("P0-1 inverted: a 410 must surface as error after deletion of reseed, got success")
	}
	var se *composerBridgeStatusError
	if !errors.As(err, &se) || se.StatusCode() != 410 {
		t.Fatalf("P0-1 inverted: expected typed 410, got %T %v", err, err)
	}
	if !strings.Contains(se.Error(), "round_lost") {
		t.Fatalf("P0-1 inverted: 410 message must contain round_lost, got %q", se.Error())
	}
	// Stream: must also surface 410
	_, errS := e.executeComposerStream(context.Background(), auth, "k", cont(), protoOpts("p01-ok-s"))
	if errS == nil {
		t.Fatalf("P0-1 inverted: stream 410 must surface as error")
	}
	if !errors.As(errS, &se) || se.StatusCode() != 410 {
		t.Fatalf("P0-1 inverted: stream expected typed 410, got %T %v", errS, errS)
	}
	// Must be exactly one POST per call (no retry)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("P0-1 inverted: expected exactly 2 POSTs (one non-stream, one stream) with no retry, got %d", len(bodies))
	}
}

// P0-1 (reseed-on-410) — a THIN continuation (first non-system message is assistant: only the tail tool
// exchange, no replayed opener) has no re-seedable context, so a 410 must SURFACE as the typed 410 on both paths
// (never a fabricated success) and must NOT trigger a retry (exactly one POST per call).
func TestP01_Reseed410ThinContinuationStays410(t *testing.T) {
	var posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&posts, 1)
		w.WriteHeader(410)
		_, _ = w.Write([]byte("unknown or expired session"))
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := protoAuth(srv.URL)
	thin := func() cliproxyexecutor.Request {
		return cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[` +
			`{"role":"assistant","tool_calls":[{"id":"call_thin_1","function":{"name":"Read"}}]},` +
			`{"role":"tool","tool_call_id":"call_thin_1","content":"x"}]}`)}
	}

	atomic.StoreInt32(&posts, 0)
	_, err := e.executeComposer(context.Background(), auth, "k", thin(), protoOpts("p01-thin-ns"))
	var se cliproxyexecutor.StatusError
	if !errors.As(err, &se) || se.StatusCode() != 410 {
		t.Fatalf("P0-1: a thin continuation 410 must surface as 410, got %T %v", err, err)
	}
	if n := atomic.LoadInt32(&posts); n != 1 {
		t.Fatalf("P0-1: a thin (unreseedable) continuation must not retry, got %d POSTs", n)
	}

	atomic.StoreInt32(&posts, 0)
	_, errS := e.executeComposerStream(context.Background(), auth, "k", thin(), protoOpts("p01-thin-s"))
	var seS cliproxyexecutor.StatusError
	if !errors.As(errS, &seS) || seS.StatusCode() != 410 {
		t.Fatalf("P0-1: a thin continuation 410 must surface as 410 on the stream path, got %T %v", errS, errS)
	}
	if n := atomic.LoadInt32(&posts); n != 1 {
		t.Fatalf("P0-1: a thin continuation must not retry on the stream path, got %d POSTs", n)
	}
}

// P0-1 recovery extension inverted per section 8: thin mixed no longer recovers; trailing user text is not
// processed after dropping results. Harness must re-send user text as new turn. Returns 410 round_lost.
func TestP01_Reseed410ThinMixedContinuationRecoversUserText(t *testing.T) {
	var bodies [][]byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, raw)
		mu.Unlock()
		w.WriteHeader(410)
		_, _ = w.Write([]byte("unknown or expired session"))
	}))
	defer srv.Close()
	e := NewCursorExecutor(&config.Config{})
	auth := protoAuth(srv.URL)
	thinMixed := func() cliproxyexecutor.Request {
		return cliproxyexecutor.Request{Model: "composer-2.5", Payload: []byte(`{"model":"composer-2.5","messages":[` +
			`{"role":"assistant","tool_calls":[{"id":"call_thin_mixed_1","function":{"name":"Read"}}]},` +
			`{"role":"tool","tool_call_id":"call_thin_mixed_1","content":"stale output"},` +
			`{"role":"user","content":"write the goal statement now"}],` +
			`"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)}
	}
	_, err := e.executeComposer(context.Background(), auth, "k", thinMixed(), protoOpts("p01-thin-mixed-ns"))
	if err == nil {
		t.Fatalf("P0-1 inverted: thin mixed 410 must surface as error, not recover")
	}
	var se *composerBridgeStatusError
	if !errors.As(err, &se) || se.StatusCode() != 410 {
		t.Fatalf("P0-1 inverted: expected 410, got %T %v", err, err)
	}
	_, errS := e.executeComposerStream(context.Background(), auth, "k", thinMixed(), protoOpts("p01-thin-mixed-s"))
	if errS == nil {
		t.Fatalf("P0-1 inverted: stream thin mixed 410 must surface as error")
	}
	if !errors.As(errS, &se) || se.StatusCode() != 410 {
		t.Fatalf("P0-1 inverted: stream expected 410, got %T %v", errS, errS)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("P0-1 inverted: expected 2 POSTs no retry, got %d", len(bodies))
	}
}

// P0-4 (bridge-down typed status) — a TRANSPORT failure dialing the bridge (the sidecar down / refusing
// connections) must surface as a typed 503 Service Unavailable on BOTH paths (a retryable upstream outage),
// never an opaque 500, and must produce no response (so no phantom response-session mapping is recorded —
// recordComposerResponseSession runs only after SSE acceptance, which a transport failure never reaches).
func TestP04_BridgeTransportFailureIsTyped503(t *testing.T) {
	t.Setenv("CURSOR_COMPOSER_BRIDGE_RECONNECT_MAX_MS", "0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // the port now refuses connections -> a transport failure on dial
	e := NewCursorExecutor(&config.Config{})
	auth := protoAuth(url)

	resp, err := e.executeComposer(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("p04-ns"))
	var se cliproxyexecutor.StatusError
	if !errors.As(err, &se) || se.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("P0-4: a bridge transport failure must map to a typed 503, got %T %v", err, err)
	}
	if resp.Payload != nil {
		t.Fatalf("P0-4: a transport failure must produce no response (no phantom mapping), got %s", string(resp.Payload))
	}

	sr, errS := e.executeComposerStream(context.Background(), auth, "k", protoToolReq("hi"), protoOpts("p04-s"))
	var seS cliproxyexecutor.StatusError
	if !errors.As(errS, &seS) || seS.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("P0-4: a bridge transport failure must map to a typed 503 on the stream path, got %T %v", errS, errS)
	}
	if sr != nil {
		t.Fatalf("P0-4: a pre-stream transport failure must return no StreamResult, got %#v", sr)
	}
}

func TestP04_BridgeRestartReconnectsReplaySafeTurnBeforeSurfacing503(t *testing.T) {
	t.Setenv("CURSOR_COMPOSER_BRIDGE_RECONNECT_MAX_MS", "3000")
	probe, errListen := net.Listen("tcp", "127.0.0.1:0")
	if errListen != nil {
		t.Fatalf("reserve bridge address: %v", errListen)
	}
	addr := probe.Addr().String()
	if errClose := probe.Close(); errClose != nil {
		t.Fatalf("release bridge address: %v", errClose)
	}

	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"session\",\"sessionId\":\"recovered\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"text\",\"delta\":\"seamless\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"turn_end\",\"stop_reason\":\"end_turn\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})}
	serveErr := make(chan error, 1)
	go func() {
		time.Sleep(250 * time.Millisecond)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			serveErr <- err
			return
		}
		serveErr <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		select {
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("recovery bridge serve: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("recovery bridge did not stop")
		}
	})

	e := NewCursorExecutor(&config.Config{})
	auth := protoAuth("http://" + addr)
	opts := protoOpts("p04-reconnect")
	opts.Headers.Set("Idempotency-Key", "inv1_p04-reconnect")
	resp, err := e.executeComposer(context.Background(), auth, "k", protoToolReq("recover without client-visible failure"), opts)
	if err != nil {
		t.Fatalf("replay-safe turn should survive a local sidecar restart: %v", err)
	}
	if !strings.Contains(string(resp.Payload), "seamless") {
		t.Fatalf("reconnected response lost content: %s", string(resp.Payload))
	}
}

func TestComposerBridgeRequestReplaySafeRequiresDurableIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{name: "fresh", body: `{"input":{"type":"user","clientMessageId":"ccm1_fresh","invocationId":"inv1_fresh"}}`, want: true},
		{name: "continuation", body: `{"input":{"type":"tool_results","clientMessageId":"ccm1_tools","invocationId":"inv1_tools"}}`, want: true},
		{name: "versioned stock-client identity", body: `{"input":{"type":"user","clientMessageId":"ccm2_fresh"}}`, want: true},
		{name: "versioned stock-client continuation", body: `{"input":{"type":"tool_results","clientMessageId":"ccm2_tools"}}`, want: true},
		{name: "unversioned semantic identity is ambiguous", body: `{"input":{"type":"user","clientMessageId":"ccm1_fresh"}}`, want: false},
		{name: "legacy missing identity", body: `{"input":{"type":"user","text":"ambiguous"}}`, want: false},
		{name: "blank identity", body: `{"input":{"type":"tool_results","clientMessageId":"ccm1_tools","invocationId":"  "}}`, want: false},
		{name: "unknown shape", body: `{"input":{"type":"other","clientMessageId":"ccm1_other","invocationId":"inv1_other"}}`, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := composerBridgeRequestReplaySafe([]byte(tc.body)); got != tc.want {
				t.Fatalf("replaySafe=%v, want %v", got, tc.want)
			}
		})
	}
}

// P1-1 (estimated usage marker) — the Cursor Composer path has no real token usage (the @cursor/sdk exposes
// none), so the proxy publishes a ~4-chars/token ESTIMATE. That published detail must carry Estimated=true so a
// usage sink can classify it as non-billing-grade and never meter money against it.
func TestP11_EstimatedUsageMarked(t *testing.T) {
	detail, ok := composerEstimatedUsageDetail(100, 50)
	if !ok {
		t.Fatalf("P1-1: a non-zero estimate must produce a usage detail")
	}
	if !detail.Estimated {
		t.Fatalf("P1-1: composer estimated usage must be marked Estimated (non-billing-grade), got %+v", detail)
	}
	if detail.TotalTokens != 150 {
		t.Fatalf("P1-1: the estimate must carry the summed tokens (150), got %d", detail.TotalTokens)
	}
	// A raw provider parse (the real-usage path) must NOT be marked estimated — the flag is composer-specific.
	if real, ok := helps.ParseOpenAIStreamUsage(composerEstimatedUsageJSON(100, 50)); !ok || real.Estimated {
		t.Fatalf("P1-1: a plain provider-usage parse must not be marked estimated (ok=%v estimated=%v)", ok, real.Estimated)
	}
}

// P0-3 (aggregate history cap) — renderComposerHistory per-message-truncates but must ALSO bound the AGGREGATE
// replayed transcript, or a long conversation re-seeds a multi-MB prompt (the ~1M-token runaway). The cap keeps
// the opener (head) + the recent tail and replaces the dropped middle with an explicit marker.
func TestP03_HistoryAggregateCap(t *testing.T) {
	// boundComposerHistoryLines unit: maxBytes<=0 disables the cap (everything kept, no marker).
	all := []string{"OPENER", "a", "b", "c", "TAILMSG"}
	if got := boundComposerHistoryLines(all, 0); got != strings.Join(all, "\n") {
		t.Fatalf("P0-3: maxBytes<=0 must disable the cap, got %q", got)
	}
	// Over cap: head + marker + tail, bounded near the cap, both ends preserved.
	big := strings.Repeat("x", 1000)
	lines := []string{"OPENER " + big}
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("MID%d %s", i, big))
	}
	lines = append(lines, "TAILMSG "+big)
	out := boundComposerHistoryLines(lines, 4096)
	if len(out) > 4096 {
		t.Fatalf("P0-3: bounded history must stay within the strict cap, got %d bytes", len(out))
	}
	if !strings.Contains(out, "OPENER") || !strings.Contains(out, "TAILMSG") {
		t.Fatalf("P0-3: bounded history must keep both the opener (head) and the recent tail")
	}
	if !strings.Contains(out, "omitted to bound the replayed history") {
		t.Fatalf("P0-3: bounded history must mark the dropped middle, got len=%d", len(out))
	}

	// renderComposerHistory integration: a long multi-message continuation history must respect the cap.
	saved := composerHistoryMaxBytes
	composerHistoryMaxBytes = 8192
	defer func() { composerHistoryMaxBytes = saved }()
	var sb strings.Builder
	sb.WriteString(`{"messages":[{"role":"user","content":"OPENERMSG ` + big + `"}`)
	for i := 0; i < 40; i++ {
		sb.WriteString(fmt.Sprintf(`,{"role":"user","content":"MID%d %s"}`, i, big))
	}
	sb.WriteString(`,{"role":"user","content":"LASTMSG ` + big + `"}]}`)
	msgs := gjson.Get(sb.String(), "messages").Array()
	rendered := renderComposerHistory(msgs, len(msgs))
	if len(rendered) > 8192 {
		t.Fatalf("P0-3: renderComposerHistory must respect the aggregate cap, got %d bytes", len(rendered))
	}
	if !strings.Contains(rendered, "OPENERMSG") || !strings.Contains(rendered, "LASTMSG") || !strings.Contains(rendered, "omitted to bound the replayed history") {
		t.Fatalf("P0-3: rendered long history must keep the opener + tail and mark the truncation")
	}

	// A tiny number of giant UTF-8 lines used to bypass the cap entirely because
	// the old whole-line selector always admitted one head and one tail line.
	giant := strings.Repeat("界", 300000)
	giantOut := boundComposerHistoryLines([]string{"OPENER " + giant, giant + " TAILMSG"}, 512<<10)
	if len(giantOut) > 512<<10 {
		t.Fatalf("P0-3: giant lines must not bypass the strict cap, got %d bytes", len(giantOut))
	}
	if !utf8.ValidString(giantOut) || !strings.Contains(giantOut, "OPENER") || !strings.Contains(giantOut, "TAILMSG") {
		t.Fatalf("P0-3: giant-line bounding must preserve valid UTF-8 head and tail")
	}
}

// P1-4 (lineage secret) — a configured CURSOR_COMPOSER_LINEAGE_SECRET yields a STABLE key (cross-restart/replica
// fork continuity); an unset secret yields a per-process RANDOM key (continuity only within one process, with a
// startup warning). This pins the determinism contract both ways.
func TestP14_LineageSecretDeterminism(t *testing.T) {
	hexSecret := strings.Repeat("deadbeef", 8) // 64 hex chars = 32 bytes
	set := func(string) string { return hexSecret }
	if string(loadComposerLineageSecret(set)) != string(loadComposerLineageSecret(set)) {
		t.Fatalf("P1-4: a configured lineage secret must yield a STABLE key across calls (cross-restart determinism)")
	}
	unset := func(string) string { return "" }
	if string(loadComposerLineageSecret(unset)) == string(loadComposerLineageSecret(unset)) {
		t.Fatalf("P1-4: an unset lineage secret must yield a per-process RANDOM key (non-deterministic across processes)")
	}
}

// P1-2 (fingerprint vs deep compact) — the 2-message head window is growth-stable but MISSES a compaction that
// preserves the first 2 messages verbatim and rewrites only deeper retained content (a documented residual edge).
// CURSOR_COMPOSER_FINGERPRINT_HEAD_MESSAGES widens the window to catch it. This pins BOTH: the edge at the default
// and the knob's effect when widened.
func TestP12_FingerprintHeadWindowTunable(t *testing.T) {
	base := gjson.Get(`{"messages":[`+
		`{"role":"user","content":"opener build the feature"},`+
		`{"role":"assistant","content":"reply ack"},`+
		`{"role":"user","content":"ORIGINAL deep body content"}]}`, "messages").Array()
	// First two messages preserved verbatim; only the DEEPER message (index 2) is rewritten by a compact.
	deepCompact := gjson.Get(`{"messages":[`+
		`{"role":"user","content":"opener build the feature"},`+
		`{"role":"assistant","content":"reply ack"},`+
		`{"role":"user","content":"[summary] deep body condensed"}]}`, "messages").Array()

	saved := composerHistoryFingerprintHeadMessages
	defer func() { composerHistoryFingerprintHeadMessages = saved }()

	// Default window (2): the deep compact slips through (fingerprint unchanged) — the documented residual edge.
	composerHistoryFingerprintHeadMessages = 2
	if composerHistoryFingerprint(base) != composerHistoryFingerprint(deepCompact) {
		t.Fatalf("P1-2: with head window 2 a deep compact preserving msgs[0:2] is expected to slip through (documented edge)")
	}
	// Widened window (3): the same deep compact now flips the fingerprint — the operator lever closes the edge.
	composerHistoryFingerprintHeadMessages = 3
	if composerHistoryFingerprint(base) == composerHistoryFingerprint(deepCompact) {
		t.Fatalf("P1-2: a widened head window must flip the fingerprint on a deeper-content compact")
	}
}
