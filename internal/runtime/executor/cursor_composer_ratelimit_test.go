package executor

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
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

	// A 410 keeps its lost-continuation guidance (must not collapse into the 429 branch). Per section 8, message now says round_lost.
	gone := (&composerBridgeStatusError{status: composerBridgeStatusGone, correlation: "c2"}).Error()
	if !strings.Contains(gone, "round_lost") {
		t.Fatalf("410 Error() = %q, missing round_lost", gone)
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

func TestComposerAdmissionGateShedsFreshTurnsAndBypassesToolResults(t *testing.T) {
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
