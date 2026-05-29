package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
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
