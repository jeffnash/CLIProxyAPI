package executor

import (
	"net/http"
	"strings"
	"testing"
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

	// A 410 keeps its lost-continuation guidance (must not collapse into the 429 branch).
	gone := (&composerBridgeStatusError{status: composerBridgeStatusGone, correlation: "c2"}).Error()
	if !strings.Contains(gone, "re-seed") {
		t.Fatalf("410 Error() = %q, missing re-seed guidance", gone)
	}

	// An arbitrary non-2xx keeps the generic status message.
	other := (&composerBridgeStatusError{status: http.StatusBadGateway, correlation: "c3"}).Error()
	if !strings.Contains(other, "status 502") {
		t.Fatalf("502 Error() = %q, missing generic status text", other)
	}
}
