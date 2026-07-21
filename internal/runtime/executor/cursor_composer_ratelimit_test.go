package executor

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// TestComposerBridgeRateLimitStatusError pins the client-facing 429 message: a bridge rate-limit/backoff (or
// capacity) 429 must surface as a typed 429 with clear "back off, don't spam retries" guidance, while a 410
// keeps its distinct lost-continuation message (regression guard). This pairs with the bridge-side per-key
// circuit breaker that returns the 429 when Cursor flood-protects the account (NGHTTP2_ENHANCE_YOUR_CALM).
func TestComposerBridgeRateLimitStatusError(t *testing.T) {
	e := &composerBridgeStatusError{status: http.StatusTooManyRequests, correlation: "corr123"}
	if got := e.StatusCode(); got != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want %d", got, http.StatusTooManyRequests)
	}
	msg := e.Error()
	for _, want := range []string{"rate-limiting", "back off", "corr123"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("429 Error() = %q, missing %q", msg, want)
		}
	}

	// A 410 reports the bridge's actual symbolic reason rather than falsely
	// labeling every missing-history/recovery case as round_lost.
	gone := (&composerBridgeStatusError{status: composerBridgeStatusGone, bridgeCode: "agent_missing_history_required", correlation: "c2"}).Error()
	if !strings.Contains(gone, "agent_missing_history_required") || strings.Contains(gone, "round_lost") {
		t.Fatalf("410 Error() = %q, want actual bridge code", gone)
	}

	// An arbitrary non-2xx keeps the generic status message.
	other := (&composerBridgeStatusError{status: http.StatusBadGateway, correlation: "c3"}).Error()
	if !strings.Contains(other, "status 502") {
		t.Fatalf("502 Error() = %q, missing generic status text", other)
	}

	conflict := (&composerBridgeStatusError{
		status:      http.StatusConflict,
		correlation: "c4",
		bridgeCode:  "client_message_delivery_uncertain",
	}).Error()
	for _, want := range []string{"status 409", "client_message_delivery_uncertain", "c4"} {
		if !strings.Contains(conflict, want) {
			t.Fatalf("409 Error() = %q, missing %q", conflict, want)
		}
	}
}

func TestParseComposerBridgeErrorCode(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{name: "typed continuation conflict", body: `{"error":{"code":"result_conflict","message":"details stay private"}}`, want: "result_conflict"},
		{name: "missing", body: `{"error":{"message":"no code"}}`, want: ""},
		{name: "uppercase rejected", body: `{"error":{"code":"RESULT_CONFLICT"}}`, want: ""},
		{name: "punctuation rejected", body: `{"error":{"code":"result conflict; secret"}}`, want: ""},
		{name: "oversize rejected", body: `{"error":{"code":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`, want: ""},
		{name: "malformed json", body: `{`, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseComposerBridgeErrorCode([]byte(tc.body)); got != tc.want {
				t.Fatalf("parseComposerBridgeErrorCode() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseComposerCapacityMetadataIsBoundedAndClientVisible(t *testing.T) {
	req, avail, priority := parseComposerCapacityMetadata([]byte(`{"error":{"requestedBytes":2097152,"availableBytes":1048576,"priority":40}}`))
	if req == nil || *req != 2097152 || avail == nil || *avail != 1048576 || priority == nil || *priority != 40 {
		t.Fatalf("capacity metadata = %v/%v/%v", req, avail, priority)
	}
	err := (&composerBridgeStatusError{
		status: http.StatusInsufficientStorage, bridgeCode: "durable_state_capacity", correlation: "corr",
		requestedBytes: req, availableBytes: avail, priority: priority,
	}).Error()
	for _, want := range []string{"requested_bytes=2097152", "available_bytes=1048576", "priority=40"} {
		if !strings.Contains(err, want) {
			t.Fatalf("capacity error %q missing %q", err, want)
		}
	}
	badReq, badAvail, badPriority := parseComposerCapacityMetadata([]byte(`{"error":{"requestedBytes":-1,"availableBytes":999999999999999999,"priority":101}}`))
	if badReq != nil || badAvail != nil || badPriority != nil {
		t.Fatalf("unbounded capacity metadata accepted: %v/%v/%v", badReq, badAvail, badPriority)
	}
	zeroReq, zeroAvail, zeroPriority := parseComposerCapacityMetadata([]byte(`{"error":{"requestedBytes":0,"availableBytes":0,"priority":0}}`))
	if zeroReq == nil || *zeroReq != 0 || zeroAvail == nil || *zeroAvail != 0 || zeroPriority == nil || *zeroPriority != 0 {
		t.Fatalf("meaningful zero capacity metadata lost: %v/%v/%v", zeroReq, zeroAvail, zeroPriority)
	}
}

// TestComposerBridgeStatusErrorAPIErrorBody pins P1.3: the structured, redacted JSON body a
// typed bridge error presents to the client — symbolic code + bounded capacity/retry fields —
// instead of a generic internal_server_error re-wrap. Raw bridge bodies never pass (M25): the
// message is the same sanitized string Error() produces.
func TestComposerBridgeStatusErrorAPIErrorBody(t *testing.T) {
	d := 2 * time.Second
	requested := int64(2097152)
	available := int64(0)
	priority := 0
	capacity := &composerBridgeStatusError{
		status: http.StatusInsufficientStorage, bridgeCode: "durable_state_capacity", correlation: "corr-cap",
		requestedBytes: &requested, availableBytes: &available, priority: &priority, retryAfter: &d,
	}
	var parsed struct {
		Error map[string]any `json:"error"`
	}
	body := capacity.APIErrorBody()
	if len(body) == 0 {
		t.Fatal("APIErrorBody returned empty body for a capacity error")
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("APIErrorBody is not valid JSON: %v (%s)", err, body)
	}
	if got := parsed.Error["code"]; got != "durable_state_capacity" {
		t.Fatalf("code = %v, want durable_state_capacity", got)
	}
	if got := parsed.Error["type"]; got != "capacity_error" {
		t.Fatalf("type = %v, want capacity_error", got)
	}
	for _, field := range []string{"requested_bytes", "available_bytes", "priority", "retry_after_ms"} {
		if _, ok := parsed.Error[field]; !ok {
			t.Fatalf("capacity body missing %q: %s", field, body)
		}
	}
	if parsed.Error["available_bytes"] != float64(0) || parsed.Error["priority"] != float64(0) {
		t.Fatalf("zero-valued capacity fields were not preserved: %v", parsed.Error)
	}
	if got := parsed.Error["retry_after_ms"]; got != float64(2000) {
		t.Fatalf("retry_after_ms = %v, want 2000", got)
	}
	if msg, _ := parsed.Error["message"].(string); !strings.Contains(msg, "corr-cap") || !strings.Contains(msg, "durable_state_capacity") {
		t.Fatalf("message lost the sanitized diagnostics: %q", msg)
	}

	gone := &composerBridgeStatusError{status: http.StatusGone, bridgeCode: "round_lost", correlation: "c2"}
	parsed.Error = nil
	if err := json.Unmarshal(gone.APIErrorBody(), &parsed); err != nil {
		t.Fatalf("410 APIErrorBody invalid: %v", err)
	}
	if parsed.Error["code"] != "round_lost" || parsed.Error["type"] != "invalid_request_error" {
		t.Fatalf("410 body wrong: %v", parsed.Error)
	}
	if _, ok := parsed.Error["requested_bytes"]; ok {
		t.Fatal("410 body must not carry capacity fields")
	}

	plain := &composerBridgeStatusError{status: http.StatusTooManyRequests, correlation: "c3"}
	parsed.Error = nil
	if err := json.Unmarshal(plain.APIErrorBody(), &parsed); err != nil {
		t.Fatalf("429 APIErrorBody invalid: %v", err)
	}
	if parsed.Error["code"] != "rate_limit_exceeded" || parsed.Error["type"] != "rate_limit_error" {
		t.Fatalf("429 defaults wrong: %v", parsed.Error)
	}
}

func TestComposerBridgeRateLimitRetryAfter(t *testing.T) {
	d := 52 * time.Second
	e := &composerBridgeStatusError{status: http.StatusTooManyRequests, correlation: "corr123", retryAfter: &d}
	if got := e.RetryAfter(); got == nil || *got != d {
		t.Fatalf("RetryAfter = %v, want %v", got, d)
	}

	if got := parseComposerRetryAfterBody([]byte(`{"error":"retry in ~43s"}`)); got == nil || *got != 43*time.Second {
		t.Fatalf("body RetryAfter = %v, want 43s", got)
	}

	h := http.Header{"Retry-After": []string{"7"}}
	if got := parseComposerRetryAfterHeader(h, time.Now()); got == nil || *got != 7*time.Second {
		t.Fatalf("header RetryAfter = %v, want 7s", got)
	}
}

func TestComposerBridgeRateLimitAttributionUsesStructuredCode(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code          string
		wantScope     cliproxyexecutor.RetryScope
		wantAuthBlame bool
	}{
		{name: "local admission", code: "local_admission_capacity", wantScope: cliproxyexecutor.RetryScopeSelectedExecution},
		{name: "local replay", code: "local_replay_capacity", wantScope: cliproxyexecutor.RetryScopeSelectedExecution},
		{name: "durable state", code: "durable_state_capacity", wantScope: cliproxyexecutor.RetryScopeSelectedExecution},
		{name: "session capacity", code: "session_capacity", wantScope: cliproxyexecutor.RetryScopeSelectedExecution},
		{name: "platform capacity", code: "platform_capacity", wantScope: cliproxyexecutor.RetryScopeSelectedExecution},
		{name: "session queue", code: "session_queue_capacity", wantScope: cliproxyexecutor.RetryScopeSelectedExecution},
		{name: "upstream account", code: "upstream_account_rate_limit", wantScope: cliproxyexecutor.RetryScopeDefault, wantAuthBlame: true},
		{name: "legacy untyped 429", code: "", wantScope: cliproxyexecutor.RetryScopeDefault, wantAuthBlame: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := &composerBridgeStatusError{status: http.StatusTooManyRequests, bridgeCode: tc.code}
			if got := err.RetryScope(); got != tc.wantScope {
				t.Fatalf("RetryScope = %v, want %v", got, tc.wantScope)
			}
			if got := err.AuthAttributable(); got != tc.wantAuthBlame {
				t.Fatalf("AuthAttributable = %v, want %v", got, tc.wantAuthBlame)
			}
		})
	}
}

func TestComposerAdmissionGateShedsFreshTurnsAndBypassesToolResults(t *testing.T) {
	t.Setenv("CURSOR_COMPOSER_GO_ADMISSION", "1")
	t.Setenv("CURSOR_COMPOSER_ADMISSION_MAX_ACTIVE_PER_KEY", "1")
	t.Setenv("CURSOR_COMPOSER_ADMISSION_MAX_QUEUE_PER_KEY", "0")
	t.Setenv("CURSOR_COMPOSER_ADMISSION_MIN_GAP_MS", "0")

	g := newComposerAdmissionGate()
	userBody := []byte(`{"input":{"type":"user"}}`)
	toolBody := []byte(`{"input":{"type":"tool_results"}}`)

	lease, err := g.acquire(context.Background(), "crsr_test_admission", userBody)
	if err != nil {
		t.Fatalf("first fresh turn admission failed: %v", err)
	}
	defer lease.release()

	_, err = g.acquire(context.Background(), "crsr_test_admission", userBody)
	var admissionErr *composerAdmissionError
	if err == nil {
		t.Fatal("second fresh turn should be shed by local admission")
	}
	if !strings.Contains(err.Error(), "local admission queue is full") || !strings.Contains(err.Error(), "retry in ~1s") {
		t.Fatalf("second fresh turn err = %T %v, want admission full", err, err)
	}
	if _, ok := err.(*composerAdmissionError); !ok {
		t.Fatalf("second fresh turn err = %T, want *composerAdmissionError", err)
	}
	admissionErr = err.(*composerAdmissionError)
	if admissionErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("admission status = %d, want 429", admissionErr.StatusCode())
	}
	if ra := admissionErr.RetryAfter(); ra == nil || *ra <= 0 {
		t.Fatalf("admission RetryAfter = %v, want positive", ra)
	}

	toolLease, err := g.acquire(context.Background(), "crsr_test_admission", toolBody)
	if err != nil {
		t.Fatalf("tool_results must bypass fresh-turn admission, got %v", err)
	}
	if toolLease != nil {
		t.Fatalf("tool_results admission lease = %#v, want nil bypass", toolLease)
	}
}

func TestComposerAdmissionGateDefaultsToAuthoritativeBridgeQueue(t *testing.T) {
	t.Setenv("CURSOR_COMPOSER_GO_ADMISSION", "")
	g := newComposerAdmissionGate()
	lease, err := g.acquire(context.Background(), "crsr_test_admission", []byte(`{"input":{"type":"user"}}`))
	if err != nil || lease != nil {
		t.Fatalf("default Go admission = lease %v, err %v; want bridge-owned bypass", lease, err)
	}
}
