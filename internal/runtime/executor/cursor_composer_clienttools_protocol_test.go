package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
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
	return cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), Headers: http.Header{"X-Conversation-Id": []string{conv}}}
}

func protoToolReq(text string) cliproxyexecutor.Request {
	payload := []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"` + text + `"}],"tools":[{"type":"function","function":{"name":"Read","parameters":{"type":"object"}}}]}`)
	return cliproxyexecutor.Request{Model: "composer-2.5", Payload: payload}
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
	// Valid SSE content type, a benign session + ping frame, then EOF — no turn_end.
	body := "data: {\"type\":\"session\",\"sessionId\":\"s\"}\n\ndata: {\"type\":\"ping\"}\n\n"
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

// RBT-017 (Comment 2 / ADD-39) — multi-replica: tool-call ownership is process-local. Two SEPARATE executor
// instances share the package-global ownership store ONLY within one process (this asserts the CURRENT
// single-instance behavior). The contract this test pins: a continuation that reaches an instance which did
// NOT record the emitting session must still degrade SAFELY — it routes via the stable-conv hash (deterministic
// across instances for a conversation that carries a stable id) and the bridge body is a tool_results RE-SEED
// (carrying history), NEVER a clean end_turn. It must never mis-route to a wrong session.
//
// NOTE: because composerOwnership is a package global, instance A and instance B in the SAME process DO share
// it (single-instance fidelity). To model a genuine cross-replica MISS (the multi-replica degrade), the
// continuation here uses a tool_call_id that was never recorded, proving the stable-conv fallback path.
func TestRBT017_MultiReplicaContinuationDegradesSafely(t *testing.T) {
	// One fake bridge shared by both instances; it records the last body so we can assert the re-seed shape.
	var lastBody []byte
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = raw
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
		// On the first (emitter) turn, emit a tool call so instance A records ownership; on any turn just end.
		if gjson.GetBytes(raw, "input.type").String() == "user" {
			write(`{"type":"tool_call","id":"tc_replicaA","name":"Read","input":{"path":"a.txt"}}`)
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

	// Instance A emits a tool call (records ownership of tc_replicaA in the package-global store).
	_, errA := instanceA.executeComposer(context.Background(), auth, "k", protoToolReq("start"), conv)
	if errA != nil {
		t.Fatalf("RBT-017: instance A emitter turn must succeed, got %v", errA)
	}
	sessA, _ := deriveComposerSessionID(auth, "k", protoToolReq("start").Payload, conv)

	// Instance B receives a continuation that echoes an id instance B NEVER recorded (model a cross-replica
	// MISS). It must NOT mis-route: with the stable conversation id present it routes via the stable-conv hash,
	// which is DETERMINISTIC across instances — so B derives the SAME sess_ id as A would for this conversation.
	contPayload := []byte(`{"model":"composer-2.5","messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"tc_cross_replica_unknown","function":{"name":"Read"}}]},` +
		`{"role":"tool","tool_call_id":"tc_cross_replica_unknown","content":"file contents"}]}`)
	contReq := cliproxyexecutor.Request{Model: "composer-2.5", Payload: contPayload}
	sessB, errSid := deriveComposerSessionID(auth, "k", contPayload, conv)
	if errSid != nil {
		t.Fatalf("RBT-017: instance B continuation must route, not error, got %v", errSid)
	}
	if !strings.HasPrefix(sessB, "sess_") {
		t.Fatalf("RBT-017: instance B must derive a real session id, got %q", sessB)
	}
	// The stable-conv hash is deterministic across instances: B's routing matches A's conversation session.
	if sessB != sessA {
		t.Fatalf("RBT-017: a continuation with a stable conv id must route deterministically across replicas (A=%s B=%s)", sessA, sessB)
	}

	resp, errB := instanceB.executeComposer(context.Background(), auth, "k", contReq, conv)
	if errB != nil {
		t.Fatalf("RBT-017: instance B continuation must degrade safely (re-seed), not error, got %v", errB)
	}
	if resp.Payload == nil {
		t.Fatalf("RBT-017: instance B must produce a response")
	}
	// The bridge body for B's continuation must be a tool_results RE-SEED carrying history — NEVER a clean
	// end_turn / fresh user turn that strands the tool output.
	mu.Lock()
	gotBody := append([]byte(nil), lastBody...)
	mu.Unlock()
	if it := gjson.GetBytes(gotBody, "input.type").String(); it != "tool_results" {
		t.Fatalf("RBT-017: B's continuation bridge body must be input.type=tool_results, got %q (%s)", it, string(gotBody))
	}
	if gjson.GetBytes(gotBody, "input.results.0.toolCallId").String() != "tc_cross_replica_unknown" {
		t.Fatalf("RBT-017: B's continuation must carry the echoed tool result, got %s", string(gotBody))
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

// P0-1 (reseed-on-410) — a LOST tool_results continuation that replays the conversation from the top (carries a
// user opener) gets ONE bounded retry, re-framed as a fresh type:"user" seed, and SUCCEEDS instead of
// dead-ending on the 410. The stub 410s any tool_results turn (the lost continuation) and accepts a user turn
// (the reseed), so BOTH the non-stream and stream paths must recover. The reseed body must be a clean user seed:
// input.type="user", NO input.results (the unanswerable tool calls are dropped), and the replayed history carried.
func TestP01_Reseed410ContinuationCarryingOpener(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	okBody := "data: {\"type\":\"text\",\"delta\":\"recovered\"}\n\ndata: {\"type\":\"turn_end\",\"stop_reason\":\"stop\"}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, raw)
		mu.Unlock()
		// The lost continuation (input.type=tool_results) is GONE; the reseed (input.type=user) is accepted.
		if gjson.GetBytes(raw, "input.type").String() == "tool_results" {
			w.WriteHeader(410)
			_, _ = w.Write([]byte("unknown or expired session"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(okBody))
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

	// Non-stream: the 410 must be recovered, not surfaced.
	resp, err := e.executeComposer(context.Background(), auth, "k", cont(), protoOpts("p01-ok-ns"))
	if err != nil {
		t.Fatalf("P0-1: a reseedable 410 must recover on the non-stream path, got %T %v", err, err)
	}
	if resp.Payload == nil {
		t.Fatalf("P0-1: non-stream reseed must produce a response")
	}

	// Stream: the 410 fires pre-stream, so the reseed retry happens before the goroutine opens; the recovered
	// text must reach the client with no chunk error.
	sr, errS := e.executeComposerStream(context.Background(), auth, "k", cont(), protoOpts("p01-ok-s"))
	if errS != nil {
		t.Fatalf("P0-1: a reseedable 410 must recover on the stream path, got %T %v", errS, errS)
	}
	streamErr, payload := drainStreamErr(sr)
	if streamErr != nil {
		t.Fatalf("P0-1: stream reseed must not surface a chunk error, got %v", streamErr)
	}
	if !strings.Contains(payload, "recovered") {
		t.Fatalf("P0-1: stream reseed must deliver the recovered turn, got %q", payload)
	}

	// Shape: the reseed body must be a clean user seed (type=user, no results, history carried).
	mu.Lock()
	defer mu.Unlock()
	sawLostContinuation, sawReseed := false, false
	for _, raw := range bodies {
		switch gjson.GetBytes(raw, "input.type").String() {
		case "tool_results":
			sawLostContinuation = true
		case "user":
			sawReseed = true
			if gjson.GetBytes(raw, "input.results").Exists() {
				t.Fatalf("P0-1: the reseed must DROP the unanswerable tool_results, got %s", string(raw))
			}
			if !gjson.GetBytes(raw, "input.history").Exists() {
				t.Fatalf("P0-1: the reseed must carry the replayed history, got %s", string(raw))
			}
		}
	}
	if !sawLostContinuation || !sawReseed {
		t.Fatalf("P0-1: expected a tool_results 410 then a user reseed (lost=%v reseed=%v)", sawLostContinuation, sawReseed)
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

// P0-1 recovery extension: a THIN continuation that also carries a fresh user instruction is recoverable. The
// stale tool_results are no longer answerable, but the latest user payload must not force a new Claude session;
// reframe it as a clean user turn and retry once.
func TestP01_Reseed410ThinMixedContinuationRecoversUserText(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	okBody := "data: {\"type\":\"text\",\"delta\":\"answered latest\"}\n\ndata: {\"type\":\"turn_end\",\"stop_reason\":\"stop\"}\n\ndata: [DONE]\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, raw)
		mu.Unlock()
		if gjson.GetBytes(raw, "input.type").String() == "tool_results" {
			w.WriteHeader(410)
			_, _ = w.Write([]byte("unknown or expired session"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(okBody))
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

	resp, err := e.executeComposer(context.Background(), auth, "k", thinMixed(), protoOpts("p01-thin-mixed-ns"))
	if err != nil {
		t.Fatalf("P0-1: a thin mixed 410 must recover on the non-stream path, got %T %v", err, err)
	}
	if resp.Payload == nil {
		t.Fatalf("P0-1: non-stream thin mixed reseed must produce a response")
	}

	sr, errS := e.executeComposerStream(context.Background(), auth, "k", thinMixed(), protoOpts("p01-thin-mixed-s"))
	if errS != nil {
		t.Fatalf("P0-1: a thin mixed 410 must recover on the stream path, got %T %v", errS, errS)
	}
	streamErr, payload := drainStreamErr(sr)
	if streamErr != nil {
		t.Fatalf("P0-1: stream thin mixed reseed must not surface a chunk error, got %v", streamErr)
	}
	if !strings.Contains(payload, "answered latest") {
		t.Fatalf("P0-1: stream thin mixed reseed must deliver the recovered turn, got %q", payload)
	}

	mu.Lock()
	defer mu.Unlock()
	sawLostContinuation, sawReseed := false, false
	for _, raw := range bodies {
		switch gjson.GetBytes(raw, "input.type").String() {
		case "tool_results":
			sawLostContinuation = true
		case "user":
			sawReseed = true
			if gjson.GetBytes(raw, "input.results").Exists() {
				t.Fatalf("P0-1: thin mixed reseed must DROP stale tool_results, got %s", string(raw))
			}
			if got := gjson.GetBytes(raw, "input.text").String(); got != "write the goal statement now" {
				t.Fatalf("P0-1: thin mixed reseed text = %q", got)
			}
		}
	}
	if !sawLostContinuation || !sawReseed {
		t.Fatalf("P0-1: expected a tool_results 410 then a user reseed (lost=%v reseed=%v)", sawLostContinuation, sawReseed)
	}
}

// P0-4 (bridge-down typed status) — a TRANSPORT failure dialing the bridge (the sidecar down / refusing
// connections) must surface as a typed 503 Service Unavailable on BOTH paths (a retryable upstream outage),
// never an opaque 500, and must produce no response (so no phantom response-session mapping is recorded —
// recordComposerResponseSession runs only after SSE acceptance, which a transport failure never reaches).
func TestP04_BridgeTransportFailureIsTyped503(t *testing.T) {
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
	if len(out) > 4096+256 {
		t.Fatalf("P0-3: bounded history must stay near the cap, got %d bytes", len(out))
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
	if len(rendered) > 8192+512 {
		t.Fatalf("P0-3: renderComposerHistory must respect the aggregate cap, got %d bytes", len(rendered))
	}
	if !strings.Contains(rendered, "OPENERMSG") || !strings.Contains(rendered, "LASTMSG") || !strings.Contains(rendered, "omitted to bound the replayed history") {
		t.Fatalf("P0-3: rendered long history must keep the opener + tail and mark the truncation")
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
