package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestWriteErrorResponse_AddonHeadersDisabledByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon: http.Header{
			"Retry-After":  {"30"},
			"X-Request-Id": {"req-1"},
		},
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After should be empty when passthrough is disabled, got %q", got)
	}
	if got := recorder.Header().Get("X-Request-Id"); got != "" {
		t.Fatalf("X-Request-Id should be empty when passthrough is disabled, got %q", got)
	}
}

func TestWriteErrorResponse_AddonHeadersEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	c.Writer.Header().Set("X-Request-Id", "old-value")

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{PassthroughHeaders: true}, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusTooManyRequests,
		Error:      errors.New("rate limit"),
		Addon: http.Header{
			"Retry-After":  {"30"},
			"X-Request-Id": {"new-1", "new-2"},
		},
	})

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want %q", got, "30")
	}
	if got := recorder.Header().Values("X-Request-Id"); !reflect.DeepEqual(got, []string{"new-1", "new-2"}) {
		t.Fatalf("X-Request-Id = %#v, want %#v", got, []string{"new-1", "new-2"})
	}
}

func TestEnrichAuthSelectionError_DefaultsTo503WithContext(t *testing.T) {
	in := &coreauth.Error{Code: "auth_not_found", Message: "no auth available"}
	out := enrichAuthSelectionError(in, []string{"claude"}, "claude-sonnet-4-6")

	var got *coreauth.Error
	if !errors.As(out, &got) || got == nil {
		t.Fatalf("expected coreauth.Error, got %T", out)
	}
	if got.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", got.StatusCode(), http.StatusServiceUnavailable)
	}
	if !strings.Contains(got.Message, "providers=claude") {
		t.Fatalf("message missing provider context: %q", got.Message)
	}
	if !strings.Contains(got.Message, "model=claude-sonnet-4-6") {
		t.Fatalf("message missing model context: %q", got.Message)
	}
	if !strings.Contains(got.Message, "/v0/management/auth-files") {
		t.Fatalf("message missing management hint: %q", got.Message)
	}
}

func TestEnrichAuthSelectionError_PreservesExplicitStatus(t *testing.T) {
	in := &coreauth.Error{Code: "auth_unavailable", Message: "no auth available", HTTPStatus: http.StatusTooManyRequests}
	out := enrichAuthSelectionError(in, []string{"gemini"}, "gemini-2.5-pro")

	var got *coreauth.Error
	if !errors.As(out, &got) || got == nil {
		t.Fatalf("expected coreauth.Error, got %T", out)
	}
	if got.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", got.StatusCode(), http.StatusTooManyRequests)
	}
}

func TestEnrichAuthSelectionError_IgnoresOtherErrors(t *testing.T) {
	in := errors.New("boom")
	out := enrichAuthSelectionError(in, []string{"claude"}, "claude-sonnet-4-6")
	if out != in {
		t.Fatalf("expected original error to be returned unchanged")
	}
}

// P1.3: a typed executor error wrapped by the conductor's bootstrap wrapper must keep its
// status through errors.As (no 507→500 downgrade), and a structured APIErrorBody must reach
// the client verbatim with the error's Retry-After hint as a standard header.
type p13StatusError struct{ code int }

func (e *p13StatusError) Error() string   { return "typed boom" }
func (e *p13StatusError) StatusCode() int { return e.code }

type p13Wrapper struct{ inner error }

func (e *p13Wrapper) Error() string { return "wrapped: " + e.inner.Error() }
func (e *p13Wrapper) Unwrap() error { return e.inner }

func TestStatusFromErrorUnwrapsTypedStatus(t *testing.T) {
	if got := statusFromError(&p13Wrapper{inner: &p13StatusError{code: http.StatusInsufficientStorage}}); got != http.StatusInsufficientStorage {
		t.Fatalf("statusFromError(wrapped 507) = %d, want 507 (must unwrap, not downgrade to 500)", got)
	}
	if got := statusFromError(&p13Wrapper{inner: errors.New("plain")}); got != 0 {
		t.Fatalf("statusFromError(wrapped plain) = %d, want 0", got)
	}
}

type p13StructuredError struct {
	body       []byte
	retryAfter *time.Duration
}

func (e *p13StructuredError) Error() string              { return "plain text fallback" }
func (e *p13StructuredError) APIErrorBody() []byte       { return e.body }
func (e *p13StructuredError) RetryAfter() *time.Duration { return e.retryAfter }

func TestWriteErrorResponse_StructuredAPIErrorBodyAndRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	d := 2 * time.Second
	structured := &p13StructuredError{
		body:       []byte(`{"error":{"message":"shared durable capacity is occupied","type":"capacity_error","code":"durable_state_capacity","retry_after_ms":2000,"requested_bytes":2097152}}`),
		retryAfter: &d,
	}
	handler := NewBaseAPIHandlers(nil, nil)
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{
		StatusCode: http.StatusInsufficientStorage,
		Error:      &p13Wrapper{inner: structured}, // wrapped, like the conductor bootstrap path
	})

	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want 507", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"durable_state_capacity"`) || !strings.Contains(body, `"requested_bytes":2097152`) {
		t.Fatalf("structured body was re-wrapped instead of passed through: %s", body)
	}
	if strings.Contains(body, "internal_server_error") {
		t.Fatalf("structured body degraded to internal_server_error: %s", body)
	}
	if got := recorder.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want \"2\"", got)
	}
}
