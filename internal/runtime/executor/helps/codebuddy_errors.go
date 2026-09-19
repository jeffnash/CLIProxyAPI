package helps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CodeBuddyError is a typed CodeBuddy upstream failure with conductor-readable
// scope and retry accessors. Model-scoped throttling and upstream failures
// leave both scope flags false so the conductor can fail over to another
// eligible account for the same model.
type CodeBuddyError struct {
	Status           int
	Code             string
	Message          string
	RequestScoped    bool
	CredentialScoped bool
	RetryDelay       *time.Duration
}

// Error returns a structured JSON envelope so existing structured
// model-not-found detection keeps working.
func (e *CodeBuddyError) Error() string {
	if e == nil {
		return `{"error":{"type":"upstream_error","code":"upstream_error","message":"unknown codebuddy error"}}`
	}
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    codeBuddyErrorType(e.Code, e.Status),
			"code":    e.Code,
			"message": e.Message,
		},
	})
	if err != nil {
		return fmt.Sprintf(`{"error":{"type":"upstream_error","code":%q,"message":"codebuddy request failed"}}`, e.Code)
	}
	return string(payload)
}

// StatusCode returns the downstream HTTP status.
func (e *CodeBuddyError) StatusCode() int {
	if e == nil {
		return http.StatusBadGateway
	}
	return e.Status
}

// IsRequestScoped reports a request fault: no rotation or account penalty.
func (e *CodeBuddyError) IsRequestScoped() bool {
	return e != nil && e.RequestScoped
}

// IsCredentialScoped reports a credential fault eligible for refresh/cooldown.
func (e *CodeBuddyError) IsCredentialScoped() bool {
	return e != nil && e.CredentialScoped
}

// RetryAfter returns the upstream-advertised retry delay, if any.
func (e *CodeBuddyError) RetryAfter() *time.Duration {
	if e == nil {
		return nil
	}
	return e.RetryDelay
}

func codeBuddyErrorType(code string, status int) string {
	switch code {
	case "model_not_found":
		return "not_found_error"
	case "rate_limited", "too_many_requests", "account_quota":
		return "rate_limit_error"
	case "unauthorized", "session_expired", "account_forbidden", "trial_not_activated":
		return "authentication_error"
	case "insufficient_credits":
		return "quota_error"
	case "prompt_too_long", "bad_request", "content_policy", "reasoning_content_missing":
		return "invalid_request_error"
	default:
		if status == http.StatusNotFound {
			return "not_found_error"
		}
		return "upstream_error"
	}
}

// maxRetryDelay bounds accepted Retry-After values; larger values are treated
// as absent rather than honored.
const maxRetryDelay = 2 * time.Hour

// WrapCodeBuddyStreamError converts an untyped stream failure into a typed
// 502 upstream error so handlers and the conductor classify invalid SSE as
// an upstream failure instead of an internal error. Typed errors and nil
// pass through unchanged.
func WrapCodeBuddyStreamError(err error) error {
	if err == nil {
		return nil
	}
	var typed *CodeBuddyError
	if errors.As(err, &typed) {
		return err
	}
	return &CodeBuddyError{Status: http.StatusBadGateway, Code: "upstream_error", Message: "codebuddy stream failed: " + shortUpstreamMessage(err.Error())}
}

// ClassifyCodeBuddyError maps an upstream failure to a typed error. It
// classifies HTTP-200 business failures explicitly using business codes plus
// narrowly matched messages where a code is ambiguous.
func ClassifyCodeBuddyError(status int, headers http.Header, body []byte, now time.Time) error {
	code, hasCode, msg, hasEnvelope := parseCodeBuddyErrorBody(body)
	lower := strings.ToLower(msg)
	delay := parseCodeBuddyRetryDelay(headers, now)

	// Offline session is terminal for the credential: surface re-login, never
	// fake a refresh success.
	if code == 12153 || strings.Contains(lower, "offline user session") {
		return &CodeBuddyError{Status: http.StatusUnauthorized, Code: "session_expired", Message: "CodeBuddy session expired; re-login is required", CredentialScoped: true}
	}
	// Model unsupported stays model-scoped with an explicit identifier.
	if (hasCode && code == 11102) || strings.Contains(lower, "service info not found") {
		message := "model not available on this CodeBuddy account (upstream 11102)"
		if msg != "" {
			message = "model not available: " + msg
		}
		return &CodeBuddyError{Status: http.StatusNotFound, Code: "model_not_found", Message: message}
	}
	if (hasCode && code == 11115) || strings.Contains(lower, "prompt is too long") {
		return &CodeBuddyError{Status: http.StatusBadRequest, Code: "prompt_too_long", Message: "prompt is too long", RequestScoped: true}
	}
	if (hasCode && code == 11101) || strings.Contains(lower, "unmarshal chat params failed") {
		return &CodeBuddyError{Status: http.StatusBadRequest, Code: "bad_request", Message: "upstream rejected the request parameters", RequestScoped: true}
	}
	// 11155 reasoning_content_missing (Tencent business code, source-derived
	// from the sibling Tencent integration): the request omitted a prior
	// turn's reasoning trace. Replaying the identical request on another
	// account cannot help, so this stays request-scoped with no rotation.
	if (hasCode && code == 11155) || strings.Contains(lower, "reasoning_content_missing") {
		return &CodeBuddyError{Status: http.StatusBadRequest, Code: "reasoning_content_missing", Message: "upstream requires the prior reasoning trace: " + shortUpstreamMessage(msg), RequestScoped: true}
	}
	if (hasCode && code == 14017) || strings.Contains(lower, "trial not activated") || strings.Contains(lower, "trial version is not yet activated") {
		return &CodeBuddyError{Status: http.StatusForbidden, Code: "trial_not_activated", Message: "CodeBuddy trial is not activated for this account", CredentialScoped: true}
	}
	// 11140 is ambiguous: rate-limit text and account-forbidden text must stay
	// distinct. A bare code defaults to model-scoped throttling, which fails
	// over without penalizing the account.
	if hasCode && code == 11140 {
		if isCodeBuddyForbiddenText(lower) {
			return &CodeBuddyError{Status: http.StatusForbidden, Code: "account_forbidden", Message: "CodeBuddy account forbidden: " + shortUpstreamMessage(msg), CredentialScoped: true}
		}
		return &CodeBuddyError{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: "CodeBuddy rate limited: " + shortUpstreamMessage(msg), RetryDelay: delay}
	}
	if status != http.StatusTooManyRequests && (status == http.StatusPaymentRequired || isCodeBuddyCreditText(lower)) {
		return &CodeBuddyError{Status: http.StatusPaymentRequired, Code: "insufficient_credits", Message: "CodeBuddy account credits exhausted", CredentialScoped: true}
	}
	if isCodeBuddyPolicyText(lower) {
		mapped := http.StatusBadRequest
		if status == http.StatusForbidden {
			mapped = http.StatusForbidden
		}
		return &CodeBuddyError{Status: mapped, Code: "content_policy", Message: "upstream content policy rejection: " + shortUpstreamMessage(msg), RequestScoped: true}
	}
	if status == http.StatusUnauthorized || isCodeBuddyExpiredTokenText(lower) {
		return &CodeBuddyError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "CodeBuddy access token expired or invalid", CredentialScoped: true}
	}
	if status == http.StatusForbidden {
		if !hasEnvelope {
			return &CodeBuddyError{Status: http.StatusForbidden, Code: "waf_blocked", Message: "upstream access rejected (http 403)"}
		}
		return &CodeBuddyError{Status: http.StatusForbidden, Code: "forbidden", Message: "upstream forbade the request: " + shortUpstreamMessage(msg)}
	}
	if status == http.StatusTooManyRequests {
		if isCodeBuddyAccountQuotaText(lower) {
			return &CodeBuddyError{Status: http.StatusTooManyRequests, Code: "account_quota", Message: "CodeBuddy account quota exceeded", CredentialScoped: true, RetryDelay: delay}
		}
		return &CodeBuddyError{Status: http.StatusTooManyRequests, Code: "too_many_requests", Message: "CodeBuddy rate limited: " + shortUpstreamMessage(msg), RetryDelay: delay}
	}
	if status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge {
		return &CodeBuddyError{Status: status, Code: "bad_request", Message: "upstream rejected the request: " + shortUpstreamMessage(msg), RequestScoped: true}
	}
	if status >= 500 {
		return &CodeBuddyError{Status: status, Code: "upstream_error", Message: fmt.Sprintf("CodeBuddy upstream failure (http %d)", status)}
	}
	if status >= 200 && status < 300 && hasCode && code != 0 {
		return &CodeBuddyError{Status: http.StatusBadGateway, Code: "upstream_error", Message: fmt.Sprintf("CodeBuddy business failure (code %d): %s", code, shortUpstreamMessage(msg))}
	}
	if status >= 400 {
		return &CodeBuddyError{Status: status, Code: "upstream_error", Message: fmt.Sprintf("CodeBuddy request failed (http %d): %s", status, shortUpstreamMessage(msg))}
	}
	return &CodeBuddyError{Status: http.StatusBadGateway, Code: "upstream_error", Message: "CodeBuddy returned an unusable response"}
}

// parseCodeBuddyErrorBody extracts the business code and message from a
// Tencent envelope, an OpenAI-style error, or a bare message. It also reports
// whether the body carries a JSON business envelope at all.
func parseCodeBuddyErrorBody(body []byte) (code int, hasCode bool, msg string, hasEnvelope bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return 0, false, "", false
	}
	lowerBody := strings.ToLower(string(trimmed))
	if strings.Contains(lowerBody, "<html") || strings.Contains(lowerBody, "<!doctype") {
		return 0, false, "", false
	}
	var envelope struct {
		Code any    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err == nil && (envelope.Code != nil || envelope.Msg != "") {
		code, hasCode := businessCodeValue(envelope.Code)
		return code, hasCode, sanitizeUpstreamMessage(envelope.Msg), true
	}
	var openai struct {
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &openai); err == nil && (openai.Error.Message != "" || openai.Error.Type != "") {
		return openaiCode(openai.Error.Code), false, sanitizeUpstreamMessage(firstNonEmpty(openai.Error.Message, openai.Error.Type)), true
	}
	var bare struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(trimmed, &bare); err == nil && bare.Message != "" {
		return 0, false, sanitizeUpstreamMessage(bare.Message), true
	}
	if json.Valid(trimmed) {
		return 0, false, "", true
	}
	return 0, false, sanitizeUpstreamMessage(string(trimmed)), false
}

// businessCodeValue parses a Tencent business code in numeric or
// numeric-string form.
func businessCodeValue(raw any) (int, bool) {
	switch value := raw.(type) {
	case float64:
		return int(value), true
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0, true
		}
		return number, true
	case nil:
		return 0, false
	default:
		return 0, true
	}
}

// openaiCode maps an OpenAI-style error code to a Tencent business code when
// the value is already numeric; string codes stay unmatched.
func openaiCode(raw any) int {
	switch value := raw.(type) {
	case float64:
		return int(value)
	case string:
		number, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return 0
		}
		return number
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// sanitizeUpstreamMessage strips control characters and bounds the length of
// an upstream message echoed into a typed error.
func sanitizeUpstreamMessage(msg string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, msg)
	cleaned = strings.Join(strings.Fields(cleaned), " ")
	if len(cleaned) > 300 {
		cleaned = cleaned[:300]
	}
	return cleaned
}

// shortUpstreamMessage falls back to a neutral message when upstream supplied
// no usable text.
func shortUpstreamMessage(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return "no upstream detail"
	}
	return msg
}

func isCodeBuddyForbiddenText(lower string) bool {
	for _, marker := range []string{"request illegal", "account forbidden", "account_forbidden", "auth forbidden", "auth_forbidden"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isCodeBuddyCreditText(lower string) bool {
	for _, marker := range []string{
		"insufficient credit", "credit exhausted", "credits exhausted", "out of credit",
		"not enough credit", "credit not enough", "quota exceeded", "quota exhaust",
		"payment required", "积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isCodeBuddyPolicyText(lower string) bool {
	for _, marker := range []string{
		"blocked by security policy", "unapproved channel", "illegal api invocation",
		"security policy", "content policy", "content blocked", "sensitive content",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isCodeBuddyExpiredTokenText(lower string) bool {
	for _, marker := range []string{
		"token expired", "token has expired", "expired token", "expired access token",
		"access token expired", "invalid access token", "invalid token", "unauthorized",
	} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func isCodeBuddyAccountQuotaText(lower string) bool {
	for _, marker := range []string{"account quota", "accountquota", "account-level quota", "account level quota"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseCodeBuddyRetryDelay parses Retry-After (integer seconds or HTTP-date)
// and retry-after-ms. Negative, unparseable and over-ceiling values yield nil.
func parseCodeBuddyRetryDelay(headers http.Header, now time.Time) *time.Duration {
	if raw := strings.TrimSpace(headers.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if seconds >= 0 && seconds <= int64(maxRetryDelay/time.Second) {
				delay := time.Duration(seconds) * time.Second
				return &delay
			}
		} else if deadline, err := http.ParseTime(raw); err == nil {
			delay := deadline.Sub(now)
			if delay < 0 {
				delay = 0
			}
			if delay <= maxRetryDelay {
				return &delay
			}
		}
	}
	if raw := strings.TrimSpace(headers.Get("Retry-After-Ms")); raw != "" {
		if millis, err := strconv.ParseInt(raw, 10, 64); err == nil && millis >= 0 && millis <= int64(maxRetryDelay/time.Millisecond) {
			delay := time.Duration(millis) * time.Millisecond
			return &delay
		}
	}
	return nil
}
