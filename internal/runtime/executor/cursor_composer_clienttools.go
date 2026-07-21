package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/turnprovenance"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Cursor Composer Client-Tools is the DEFAULT, ToS-safe Cursor routing: requests go to the
// cursor-agent-bridge.mjs sidecar over HTTP/SSE (/agent/turn), where the official
// @cursor/sdk owns ALL Cursor API I/O and every tool executes on the client
// (Claude Code) through CLIProxy. The proxy never calls Cursor's API directly.
//
// The gated, ToS-exposed direct path (cursor_agent.go, which forges IDE identity
// headers) is only used when CURSOR_DIRECT=1 is set explicitly.
//
// Each /v1/messages request from the client maps to ONE /agent/turn call: a new
// user message starts/continues the Cursor agent run; a turn whose last message is
// a tool result resolves the bridge's pending tool calls and continues the run.
// The bridge holds the live SDK run across turns, keyed by sessionId.

const (
	defaultComposerBridgeURL  = "http://127.0.0.1:9798"
	composerAgentTurnPath     = "/agent/turn"
	composerAgentContinuePath = "/agent/continue"
)

// composerSSEMaxLineBytes bounds one bridge SSE data line. Node enforces the
// same CURSOR_COMPOSER_MAX_SSE_FRAME_BYTES contract before writing, so a frame
// can never be accepted by one side and fail later as bufio.ErrTooLong on the
// other. The default is one MiB above MAX_AGENT_TURN_BYTES for JSON/SSE framing
// overhead. This is a memory bound, not a wall-clock timeout.
var composerSSEMaxLineBytes = func() int {
	const defaultLimit = 65 << 20
	raw := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_MAX_SSE_FRAME_BYTES"))
	if raw == "" {
		return defaultLimit
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value < 1<<20 {
		log.Warnf("cursor composer: invalid CURSOR_COMPOSER_MAX_SSE_FRAME_BYTES=%q; using %d", raw, defaultLimit)
		return defaultLimit
	}
	return int(value)
}()

// composerBridgeMaxErrorBodyBytes bounds how much of a bridge non-2xx body is read before
// redaction/logging (ADD-46): a faulty or hostile bridge could return a multi-megabyte error page
// (stack trace, request echo, proxy error). The typed error returned to the client carries only a
// correlation id (never the body), so this bound is purely a memory guard on the diagnostic read.
const composerBridgeMaxErrorBodyBytes = 64 << 10

var composerStreamCommitMaxBytes = func() int {
	const defaultMax = 64 << 20
	raw := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_STREAM_COMMIT_MAX_BYTES"))
	if raw == "" {
		return defaultMax
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1<<20 {
		log.Warnf("cursor composer: invalid CURSOR_COMPOSER_STREAM_COMMIT_MAX_BYTES=%q; using %d", raw, defaultMax)
		return defaultMax
	}
	return value
}()

type composerAtomicCommitBudget struct {
	mu   sync.Mutex
	max  int
	used int
}

func (b *composerAtomicCommitBudget) reserve(bytes int) bool {
	if b == nil || bytes <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if bytes > b.max-b.used {
		return false
	}
	b.used += bytes
	return true
}

func (b *composerAtomicCommitBudget) release(bytes int) {
	if b == nil || bytes <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= bytes
	if b.used < 0 {
		b.used = 0
	}
	b.mu.Unlock()
}

var composerStreamCommitGlobalBudget = &composerAtomicCommitBudget{max: func() int {
	const defaultMax = 256 << 20
	raw := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_STREAM_COMMIT_GLOBAL_MAX_BYTES"))
	if raw == "" {
		return defaultMax
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1<<20 {
		log.Warnf("cursor composer: invalid CURSOR_COMPOSER_STREAM_COMMIT_GLOBAL_MAX_BYTES=%q; using %d", raw, defaultMax)
		return defaultMax
	}
	return value
}()}

// composerBridgeReconnectMaxElapsed is the bounded pre-response recovery window
// for the process-local Cursor bridge. A sidecar restart must not turn into a
// client-visible 503 when the exact request has a durable clientMessageId and
// can be replayed idempotently. This guard covers only connection establishment:
// once an HTTP response is established, the data path remains timeout-free.
func composerBridgeReconnectMaxElapsed() time.Duration {
	const defaultMax = 90 * time.Second
	raw := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_BRIDGE_RECONNECT_MAX_MS"))
	if raw == "" {
		return defaultMax
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		log.Warnf("cursor composer: invalid CURSOR_COMPOSER_BRIDGE_RECONNECT_MAX_MS=%q; using %s", raw, defaultMax)
		return defaultMax
	}
	return time.Duration(value) * time.Millisecond
}

func composerBridgeReconnectDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 5 {
		shift = 5
	}
	delay := 100 * time.Millisecond * time.Duration(1<<shift)
	if delay > 2*time.Second {
		return 2 * time.Second
	}
	return delay
}

func composerBridgeReconnectEnabled(turnURL string, body []byte) bool {
	return composerBridgeRequestReplaySafe(body) && composerBridgeReconnectMaxElapsed() > 0 &&
		(isLoopbackBridgeURL(turnURL) || composerEnvTruthy(os.Getenv("CURSOR_COMPOSER_BRIDGE_RECONNECT_REMOTE")))
}

func composerBridgeEstablishedReconnectMaxAttempts() int {
	return composerEnvInt("CURSOR_COMPOSER_STREAM_RECONNECT_MAX_ATTEMPTS", 3, 0)
}

func composerBridgeEstablishedReconnectAllowed(auth *cliproxyauth.Auth, body []byte) bool {
	endpoint := composerAgentTurnPath
	if gjson.GetBytes(body, "input.type").String() == "tool_results" {
		endpoint = composerAgentContinuePath
	}
	turnURL, err := buildComposerBridgeURL(auth, endpoint)
	return err == nil && composerBridgeReconnectEnabled(turnURL, body)
}

// composerBridgeRequestReplaySafe permits automatic transport replay only for
// the v2 request shapes whose semantic identity is durable at the bridge. Old
// or malformed clients without a clientMessageId retain the explicit 503 path;
// guessing there could duplicate a user instruction or tool result.
func composerBridgeRequestReplaySafe(body []byte) bool {
	inputType := gjson.GetBytes(body, "input.type").String()
	if inputType != "user" && inputType != "tool_results" {
		return false
	}
	invocationID := gjson.GetBytes(body, "input.invocationId")
	if invocationID.Exists() {
		return strings.TrimSpace(invocationID.String()) != ""
	}
	// Stock clients may not expose a provider-call id. The proxy's versioned
	// ccm2 identity is still replay-authoritative for an identical request: it
	// binds the current payload to bounded prior transcript context, and the
	// bridge durably freezes/replays its exact SDK envelope. Arbitrary legacy
	// ids remain ambiguous and are never transparently re-sent.
	return strings.HasPrefix(
		strings.TrimSpace(gjson.GetBytes(body, "input.clientMessageId").String()),
		"ccm2_",
	)
}

func waitComposerBridgeReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// composerBridgeStatusGone is the HTTP status the bridge returns for a lost tool-results continuation
// (the tool call this result answers was not issued by this bridge — restart/idle eviction/cap eviction).
// It is a REAL terminal error, distinct from a transient/retryable failure, and must reach the client
// with that meaning (ADD-59 / C-ADD59-TYPED-STATUS), never as an opaque 500 and never as a success.
const composerBridgeStatusGone = 410

// composerBridgeStatusInsufficientStorage is the HTTP status the bridge returns when shared durable
// capacity is occupied (durable_state_capacity / admission shed). It is retryable with backoff and
// must reach the client as a 507 with its typed metadata, never downgraded to an opaque 500 (P1.3).
const composerBridgeStatusInsufficientStorage = 507

// composerBridgeProtocolError is a typed executor error for a bridge SSE CONTRACT violation that is NOT a
// bridge HTTP non-2xx (ADD-88/96, Comment 1): a 2xx response whose Content-Type is not text/event-stream, an
// SSE payload that is not valid JSON, an unknown/non-benign event type (RBT-013), or a clean stream EOF with
// no terminal turn_end (RBT-012). Each of these would otherwise be mis-handled as a CLEAN EMPTY SUCCESS (an
// empty assistant message / synthetic [DONE]) — a silent false success. It carries ONLY a short reason + a
// correlation id (the redacted detail lives in the logs under the same corr), never the bridge body (M25). It
// maps to a 502-class status (the bridge spoke an invalid protocol), distinct from a 4xx client error and from
// a bridge-preserved status (composerBridgeStatusError) — so the client is told the upstream bridge failed.
type composerBridgeProtocolError struct {
	reason      string
	correlation string
}

func (e *composerBridgeProtocolError) Error() string {
	return fmt.Sprintf("cursor composer: bridge protocol violation (%s) (correlation %s)", e.reason, e.correlation)
}

// StatusCode maps a bridge protocol violation to 502 Bad Gateway (the upstream bridge returned an invalid
// response), so the conductor/handler does not collapse it to a generic 500 or retry it as a transient fault.
func (e *composerBridgeProtocolError) StatusCode() int { return http.StatusBadGateway }

func (e *composerBridgeProtocolError) RetryScope() cliproxyexecutor.RetryScope {
	return cliproxyexecutor.RetryScopeSelectedExecution
}

func (e *composerBridgeProtocolError) AuthAttributable() bool { return false }

type composerCommitCapacityError struct{ correlation string }

func (e *composerCommitCapacityError) Error() string {
	return fmt.Sprintf("cursor composer: local atomic response capacity is exhausted; retry after another turn completes (correlation %s)", e.correlation)
}
func (e *composerCommitCapacityError) StatusCode() int { return http.StatusServiceUnavailable }
func (e *composerCommitCapacityError) RetryScope() cliproxyexecutor.RetryScope {
	return cliproxyexecutor.RetryScopeSelectedExecution
}
func (e *composerCommitCapacityError) AuthAttributable() bool { return false }

// composerBridgeLogProtocol logs a protocol violation and returns its correlation id.
func composerBridgeLogProtocol(responseID, reason, detail string) string {
	corr := composerCorrelationID()
	log.Errorf("[composer %s] bridge PROTOCOL VIOLATION corr=%s reason=%s detail=%s", responseID, corr, reason, sanitizeBridgeBody([]byte(detail)))
	return corr
}

// composerBridgeLogTransport logs a transport failure and returns its correlation id.
func composerBridgeLogTransport(responseID string, cause error) string {
	corr := composerCorrelationID()
	log.Errorf("[composer %s] bridge TRANSPORT FAILURE corr=%s (-> 503): %s", responseID, corr, sanitizeBridgeBody([]byte(cause.Error())))
	return corr
}

// newComposerBridgeProtocolError builds the typed protocol error AND emits a single redacted diagnostic under
// the correlation id (never the bridge body to the client). corr ties the client-visible generic error to the
// server-side log line. The detail is sanitized via sanitizeBridgeBody so no credential leaks into logs.
func newComposerBridgeProtocolError(responseID, reason, detail string) *composerBridgeProtocolError {
	corr := composerBridgeLogProtocol(responseID, reason, detail)
	return &composerBridgeProtocolError{reason: reason, correlation: corr}
}

// composerResponseIsSSE reports whether a bridge /agent/turn 2xx response actually carries the SSE content
// type (ADD-88, Comment 1). A misconfigured remote bridge / reverse proxy / auth gateway / CDN can return a
// 2xx HTML login page or a JSON {"ok":true} health body; without this gate the scanner would silently ignore
// every non-"data:" line and emit a clean empty completion (a false success). Matches text/event-stream with
// any parameters (e.g. "; charset=utf-8"), case-insensitively.
func composerResponseIsSSE(resp *http.Response) bool {
	ct := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	return strings.HasPrefix(ct, "text/event-stream")
}

// composerEventIsBenignTelemetry reports whether an unknown-to-the-translator bridge SSE event type is a known
// benign telemetry frame that may be safely IGNORED rather than treated as a protocol violation (ADD-96): the
// session announcement and the keepalive ping. Any OTHER unknown type is a protocol drift (e.g. a renamed
// tool_call) that must fail closed so a dropped tool/content is never masked as success.
func composerEventIsBenignTelemetry(eventType string) bool {
	switch eventType {
	case "session", "ping":
		return true
	default:
		return false
	}
}

// composerBridgeTerminalReplaySafe recognizes a bridge terminal that explicitly
// says the identical, durably identified request may be replayed. The request
// itself is checked separately by composerBridgeRequestReplaySafe before any
// reconnect is attempted.
func composerBridgeTerminalReplaySafe(ev gjson.Result) bool {
	if ev.Get("stop_reason").String() != "error" || !ev.Get("retryable").Bool() {
		return false
	}
	switch ev.Get("retryMode").String() {
	case "identical":
		return true
	case "split", "repair", "none":
		return false
	}
	// Rolling-upgrade compatibility for bridge versions predating retryMode.
	// Keep this list narrow: retryable means the client may try again, not
	// necessarily that the identical body is safe or capable of progress.
	switch ev.Get("receipt").String() {
	case "completion_not_durable", "session_cancellation_terminal", "continuation_failure_after_headers":
		return true
	case "continuation_conflict_contained":
		return ev.Get("errorCode").String() == "journal_revision_conflict"
	default:
		return false
	}
}

// composerBridgeStatusError is a typed executor error that preserves the bridge's HTTP status so the
// conductor (sdk/cliproxy/auth/conductor.go reads cliproxyexecutor.StatusError) sets rerr.HTTPStatus and
// the API handler (sdk/api/handlers/handlers.go) maps it to the client response status — ADD-59 /
// C-ADD59-TYPED-STATUS. It carries ONLY a short generic message + a correlation id (the full, redacted
// body lives in the logs under the same corr), never the raw bridge body (M25). For a 410 specifically the
// message tells the client the tool-call continuation is gone and the turn must be re-seeded/restarted —
// distinguishable from "retry later". This mirrors the Codex precedent (classifyCodexStatusError).
type composerBridgeStatusError struct {
	status         int
	correlation    string
	retryAfter     *time.Duration
	bridgeCode     string
	requestedBytes *int64
	availableBytes *int64
	priority       *int
}

func (e *composerBridgeStatusError) Error() string {
	if e.status == composerBridgeStatusGone {
		code := e.bridgeCode
		if code == "" {
			code = "state_gone"
		}
		return fmt.Sprintf("cursor composer: bridge state is unavailable (%s); the request was not applied and must be re-seeded or re-driven by the harness (correlation %s)", code, e.correlation)
	}
	if e.status == http.StatusTooManyRequests {
		// 429: upstream rate-limit (Cursor HTTP/2 ENHANCE_YOUR_CALM, recycled connection + backoff) or proxy
		// capacity. Tell the client to back off — rapid retries re-trip the limit and prolong the outage.
		return fmt.Sprintf("cursor composer: upstream is rate-limiting this account or the proxy is at capacity; "+
			"back off and retry in a few seconds — rapid retries make it worse (correlation %s)", e.correlation)
	}
	if e.bridgeCode != "" {
		if e.requestedBytes != nil || e.availableBytes != nil || e.priority != nil {
			fields := make([]string, 0, 3)
			if e.requestedBytes != nil {
				fields = append(fields, fmt.Sprintf("requested_bytes=%d", *e.requestedBytes))
			}
			if e.availableBytes != nil {
				fields = append(fields, fmt.Sprintf("available_bytes=%d", *e.availableBytes))
			}
			if e.priority != nil {
				fields = append(fields, fmt.Sprintf("priority=%d", *e.priority))
			}
			return fmt.Sprintf("cursor composer: bridge request failed with status %d (%s; %s; correlation %s)",
				e.status, e.bridgeCode, strings.Join(fields, " "), e.correlation)
		}
		return fmt.Sprintf("cursor composer: bridge request failed with status %d (%s; correlation %s)", e.status, e.bridgeCode, e.correlation)
	}
	return fmt.Sprintf("cursor composer: bridge request failed with status %d (correlation %s)", e.status, e.correlation)
}

// StatusCode implements cliproxyexecutor.StatusError so the conductor/handler preserve the bridge status
// to the client (e.g. a 410 stays a 410, a 429 stays a 429) instead of collapsing every non-2xx to 500.
func (e *composerBridgeStatusError) StatusCode() int { return e.status }

// RetryAfter exposes bridge/local-admission retry hints to the conductor so 429 cooldowns use the provider's
// actual backoff window instead of a generic quota schedule.
func (e *composerBridgeStatusError) RetryAfter() *time.Duration { return e.retryAfter }

func (e *composerBridgeStatusError) RetryScope() cliproxyexecutor.RetryScope {
	switch e.bridgeCode {
	case "local_admission_capacity", "local_replay_capacity", "durable_state_capacity", "session_capacity", "platform_capacity", "session_queue_capacity":
		return cliproxyexecutor.RetryScopeSelectedExecution
	case "upstream_account_rate_limit":
		return cliproxyexecutor.RetryScopeDefault
	}
	switch e.status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return cliproxyexecutor.RetryScopeDefault
	default:
		return cliproxyexecutor.RetryScopeSelectedExecution
	}
}

func (e *composerBridgeStatusError) AuthAttributable() bool {
	switch e.bridgeCode {
	case "local_admission_capacity", "local_replay_capacity", "durable_state_capacity", "session_capacity", "platform_capacity", "session_queue_capacity":
		return false
	case "upstream_account_rate_limit":
		return true
	}
	switch e.status {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}

// APIErrorBody implements the API handlers' structured-error contract (P1.3): a REDACTED,
// OpenAI-compatible JSON error payload that preserves the bridge's symbolic code and the
// bounded capacity/retry fields all the way to the client, so a shed turn can be retried
// intelligently (which code, how much capacity was requested/available, when to retry)
// instead of arriving as a generic internal_server_error with no actionable metadata.
// The message text stays the same sanitized string as Error(); the raw bridge body is
// never forwarded (M25). Returns nil on any marshal failure so callers fall back to the
// generic body.
func (e *composerBridgeStatusError) APIErrorBody() []byte {
	errType := "server_error"
	code := e.bridgeCode
	switch {
	case e.status == http.StatusTooManyRequests:
		errType = "rate_limit_error"
		if code == "" {
			code = "rate_limit_exceeded"
		}
	case e.status == composerBridgeStatusInsufficientStorage:
		errType = "capacity_error"
		if code == "" {
			code = "durable_state_capacity"
		}
	case e.status == http.StatusGone, e.status == http.StatusBadRequest:
		errType = "invalid_request_error"
	case e.status >= http.StatusInternalServerError:
		if code == "" {
			code = "internal_server_error"
		}
	}
	detail := map[string]any{
		"message": e.Error(),
		"type":    errType,
	}
	if code != "" {
		detail["code"] = code
	}
	if e.retryAfter != nil && *e.retryAfter > 0 {
		detail["retry_after_ms"] = int64(*e.retryAfter / time.Millisecond)
	}
	if e.requestedBytes != nil {
		detail["requested_bytes"] = *e.requestedBytes
	}
	if e.availableBytes != nil {
		detail["available_bytes"] = *e.availableBytes
	}
	if e.priority != nil {
		detail["priority"] = *e.priority
	}
	body, err := json.Marshal(map[string]any{"error": detail})
	if err != nil {
		return nil
	}
	return body
}

var composerRetryInPattern = regexp.MustCompile(`(?i)retry\s+in\s+~?\s*(\d+)\s*s`)
var composerBridgeErrorCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// parseComposerBridgeErrorCode retains only the bridge's bounded symbolic code
// for the client-facing typed error. The raw JSON body and message stay in the
// redacted server log, so arguments, credentials, and upstream text cannot be
// reflected through the API error response.
func parseComposerBridgeErrorCode(body []byte) string {
	code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	if !composerBridgeErrorCodePattern.MatchString(code) {
		return ""
	}
	return code
}

func parseComposerCapacityMetadata(body []byte) (requestedBytes, availableBytes *int64, priority *int) {
	const maxReportedBytes = int64(1 << 50)
	boundedBytes := func(path string) *int64 {
		v := gjson.GetBytes(body, path)
		if !v.Exists() || v.Type != gjson.Number {
			return nil
		}
		n := v.Int()
		if n < 0 || n > maxReportedBytes {
			return nil
		}
		return &n
	}
	requestedBytes = boundedBytes("error.requestedBytes")
	availableBytes = boundedBytes("error.availableBytes")
	p := gjson.GetBytes(body, "error.priority")
	if p.Exists() && p.Type == gjson.Number && p.Int() >= 0 && p.Int() <= 100 {
		value := int(p.Int())
		priority = &value
	}
	return
}

func parseComposerRetryAfterHeader(h http.Header, now time.Time) *time.Duration {
	raw := strings.TrimSpace(h.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		d := time.Duration(seconds) * time.Second
		if d < 0 {
			d = 0
		}
		return &d
	}
	if t, err := http.ParseTime(raw); err == nil {
		d := t.Sub(now)
		if d < 0 {
			d = 0
		}
		return &d
	}
	return nil
}

func parseComposerRetryAfterBody(body []byte) *time.Duration {
	match := composerRetryInPattern.FindSubmatch(body)
	if len(match) != 2 {
		return nil
	}
	seconds, err := strconv.Atoi(string(match[1]))
	if err != nil {
		return nil
	}
	d := time.Duration(seconds) * time.Second
	return &d
}

// composerBridgeUnavailableError is a typed executor error for a TRANSPORT failure dialing the bridge's
// /agent/turn endpoint (connection refused, DNS, TLS, the sidecar process down or restarting) — distinct from a
// bridge non-2xx (composerBridgeStatusError) and an SSE contract violation (composerBridgeProtocolError, 502).
// The bridge is the single funnel for ALL composer traffic, so a restart/crash would otherwise surface as an
// opaque 500 for every concurrent request (P0-4). Mapping it to 503 Service Unavailable tells the client the
// upstream is temporarily unavailable (retryable) rather than a model/logic error. It carries only a correlation
// id + the wrapped cause (for logs/errors.Is), never credentials (the cause is sanitized before logging).
type composerBridgeUnavailableError struct {
	correlation string
	cause       error
}

func (e *composerBridgeUnavailableError) Error() string {
	return fmt.Sprintf("cursor composer: bridge unavailable (the /agent/turn sidecar is unreachable or restarting; "+
		"retry shortly) (correlation %s)", e.correlation)
}

// StatusCode maps a bridge transport failure to 503 Service Unavailable (a retryable upstream outage), distinct
// from the 502 protocol-violation class and from a bridge-preserved 4xx/5xx status.
func (e *composerBridgeUnavailableError) StatusCode() int { return http.StatusServiceUnavailable }

// Unwrap exposes the underlying transport cause for errors.Is/As chains (the cause is never shown to the client).
func (e *composerBridgeUnavailableError) Unwrap() error { return e.cause }

func (e *composerBridgeUnavailableError) RetryScope() cliproxyexecutor.RetryScope {
	return cliproxyexecutor.RetryScopeSelectedExecution
}

func (e *composerBridgeUnavailableError) AuthAttributable() bool { return false }

// newComposerBridgeUnavailableError builds the typed transport error and emits one redacted diagnostic under the
// correlation id (the cause may carry the bridge URL/host, so it is sanitized via sanitizeBridgeBody).
func newComposerBridgeUnavailableError(responseID string, cause error) *composerBridgeUnavailableError {
	corr := composerBridgeLogTransport(responseID, cause)
	return &composerBridgeUnavailableError{correlation: corr, cause: cause}
}

func composerBridgeTurnFailure(responseID string, ev gjson.Result) error {
	emsg := ev.Get("error").String()
	if emsg == "" {
		emsg = "upstream Cursor run failed"
	}
	if ev.Get("retryable").Bool() {
		code := ev.Get("errorCode").String()
		if composerBridgeErrorCodePattern.MatchString(code) {
			corr := composerCorrelationID()
			log.Errorf("[composer %s] bridge RETRYABLE TERMINAL corr=%s code=%s (-> 503)", responseID, corr, code)
			var retryAfter *time.Duration
			if millis := ev.Get("retryAfterMs").Int(); millis > 0 {
				duration := time.Duration(millis) * time.Millisecond
				retryAfter = &duration
			}
			return &composerBridgeStatusError{
				status:      http.StatusServiceUnavailable,
				correlation: corr,
				retryAfter:  retryAfter,
				bridgeCode:  code,
			}
		}
		return newComposerBridgeUnavailableError(responseID, errors.New(emsg))
	}
	return &composerSelectedExecutionError{cause: fmt.Errorf("cursor composer: bridge turn failed: %s", emsg)}
}

// composerSelectedExecutionError prevents the generic conductor from replaying
// an accepted or acceptance-unknown invocation through another credential or
// model. The selected executor owns any exact-body recovery.
type composerSelectedExecutionError struct {
	cause error
}

func (e *composerSelectedExecutionError) Error() string { return e.cause.Error() }
func (e *composerSelectedExecutionError) Unwrap() error { return e.cause }
func (e *composerSelectedExecutionError) RetryScope() cliproxyexecutor.RetryScope {
	return cliproxyexecutor.RetryScopeSelectedExecution
}
func (e *composerSelectedExecutionError) AuthAttributable() bool { return false }

func composerConstrainToSelectedExecution(err error) error {
	if err == nil {
		return nil
	}
	if disposition, ok := errors.AsType[cliproxyexecutor.ErrorDisposition](err); ok && disposition != nil &&
		disposition.RetryScope() == cliproxyexecutor.RetryScopeSelectedExecution && !disposition.AuthAttributable() {
		return err
	}
	return &composerSelectedExecutionError{cause: err}
}

// composerEnvTruthy reports whether a trimmed env/attribute value is truthy (1 or true, case-insensitive).
func composerEnvTruthy(v string) bool {
	v = strings.TrimSpace(v)
	return v == "1" || strings.EqualFold(v, "true")
}

// composerEnvTruthyRaw matches composerDebugEnabled's legacy check (no trim, no EqualFold).
func composerEnvTruthyRaw(v string) bool {
	return v == "1" || v == "true"
}

// cursorDirectEnabled reports whether the gated, ToS-exposed direct Cursor path
// is explicitly opted into. Default (unset) is the safe Cursor Composer Client-Tools sidecar path.
func cursorDirectEnabled() bool {
	return composerEnvTruthy(os.Getenv("CURSOR_DIRECT"))
}

// resolveComposerBridgeURL returns the agent-bridge base URL for the selected auth entry.
// Precedence: per-auth attribute (synthesized from cursor-api-key[].composer-client-tools-bridge-url) > env > default.
// It does NOT scan unrelated cfg.CursorKey entries — routing is per-request, keyed on the selected auth.
func resolveComposerBridgeURL(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["composer_client_tools_bridge_url"]); v != "" {
			return v
		}
	}
	if env := strings.TrimSpace(os.Getenv("CURSOR_AGENT_BRIDGE_URL")); env != "" {
		return env
	}
	return defaultComposerBridgeURL
}

// isLoopbackBridgeURL reports whether the bridge URL points at the local host (localhost / 127.0.0.0/8 /
// ::1). Loopback bridge calls must bypass any configured outbound proxy (see postAgentTurn).
func isLoopbackBridgeURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// isLocalBridgeHost reports whether the bridge host is on the local machine / link — loopback (127/8, ::1,
// "localhost") OR link-local (169.254/16, fe80::/10). Plaintext HTTP is only acceptable for such hosts: the
// credentials never leave the host/link, so ADD-41's HTTPS requirement is relaxed there. Any routable host
// (a real remote bridge) must use HTTPS unless insecure transport is explicitly opted into.
func isLocalBridgeHost(rawURL string) bool {
	if isLoopbackBridgeURL(rawURL) {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
	}
	return false
}

// composerBridgeInsecureAllowed reports whether plaintext HTTP to a NON-local bridge is explicitly opted
// into (ADD-41 development escape hatch). Honors the per-auth attribute
// `composer_client_tools_allow_insecure_bridge` (forward-compatible: synthesized from config if/when wired)
// AND the env var CURSOR_AGENT_BRIDGE_ALLOW_INSECURE. Default (unset) is SECURE: a non-local http:// bridge
// URL is rejected so a per-user Cursor key + bridge token never traverse a plaintext hop or an outbound proxy.
//
// CONTRACT NOTE: the addendum names the config-level flag `composer_client_tools_allow_insecure_bridge=1`.
// Wiring a new YAML/attribute field lives in internal/config + internal/watcher/synthesizer (not this file);
// the env var is fully functional today and the attribute lookup is forward-compatible with that wiring.
func composerBridgeInsecureAllowed(auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.Attributes != nil {
		v := strings.TrimSpace(auth.Attributes["composer_client_tools_allow_insecure_bridge"])
		if composerEnvTruthy(v) {
			return true
		}
	}
	return composerEnvTruthy(os.Getenv("CURSOR_AGENT_BRIDGE_ALLOW_INSECURE"))
}

// buildComposerTurnURL validates the configured bridge base URL and returns the fully-joined /agent/turn URL.
//
// ADD-47: it parses the base with net/url and joins the path STRUCTURALLY (u.Path = path.Join(...), clearing
// RawQuery) instead of string-concatenating, so a base carrying a path or query (e.g.
// https://bridge.example.com/cursor or https://bridge.example.com?token=abc) is not corrupted into a bogus
// URL. A base that carries userinfo or a query string is REJECTED (a typed error) — credentials must travel
// in headers (Authorization / X-Bridge-Auth), never the URL/logs.
//
// ADD-41: a non-local (routable) bridge URL MUST use https unless insecure transport is explicitly allowed.
// A plaintext non-local http:// base is rejected at request-build time (typed error, never a silent send)
// so the per-user Cursor key + bridge token cannot traverse a cleartext network hop or an outbound proxy.
func buildComposerTurnURL(auth *cliproxyauth.Auth) (string, error) {
	return buildComposerBridgeURL(auth, composerAgentTurnPath)
}

func buildComposerBridgeURL(auth *cliproxyauth.Auth, endpoint string) (string, error) {
	base := strings.TrimSpace(resolveComposerBridgeURL(auth))
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("cursor composer: invalid bridge URL configured")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("cursor composer: bridge URL must be http or https")
	}
	if u.Host == "" {
		return "", fmt.Errorf("cursor composer: bridge URL is missing a host")
	}
	// ADD-47: reject credential-in-URL and query-in-base — they would be mis-joined and/or logged.
	if u.User != nil {
		return "", fmt.Errorf("cursor composer: bridge URL must not embed credentials (userinfo); use headers")
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("cursor composer: bridge URL must not carry a query string")
	}
	// ADD-41: require HTTPS for any non-local host unless insecure transport is explicitly allowed.
	if u.Scheme == "http" && !isLocalBridgeHost(base) && !composerBridgeInsecureAllowed(auth) {
		return "", fmt.Errorf("cursor composer: refusing to send credentials to a non-local bridge over plain HTTP; " +
			"use https or set CURSOR_AGENT_BRIDGE_ALLOW_INSECURE=1 for a trusted dev setup")
	}
	// ADD-47: join structurally so a configured base path is preserved and a stray query is dropped. The
	// endpoint suffix is the single-source composerAgentTurnPath, joined onto any configured base path.
	u.Path = path.Join("/", strings.TrimRight(u.Path, "/"), endpoint)
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// resolveComposerBridgeToken returns the multi-tenant bridge auth token (sent as X-Bridge-Auth) for the
// selected auth entry, or "" for the default single-tenant setup. Precedence: per-auth attribute
// (cursor-api-key[].composer-client-tools-bridge-token) > env CURSOR_AGENT_BRIDGE_TOKEN. When empty, the bridge is in
// single-tenant mode and the forwarded Cursor key (Authorization bearer) doubles as the bridge gate.
func resolveComposerBridgeToken(auth *cliproxyauth.Auth) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["composer_client_tools_bridge_token"]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(os.Getenv("CURSOR_AGENT_BRIDGE_TOKEN"))
}

// composerClientToolRoutingTokenPrefix must equal ROUTING_TOKEN_VERSION in
// sidecars/cursor-bridge/tool-round.mjs followed by "_". Go only recognizes
// the opaque wire version; the bridge remains responsible for decoding it.
const composerClientToolRoutingTokenPrefix = "cct1_"

// composerContinuationHintFor classifies a continuation only from protocol-visible evidence. Signed
// client-tool ids are opaque to Go; the bridge verifies and routes them from its durable journal.
func composerContinuationHintFor(_ string, oai []byte) composerContinuationHint {
	return composerContinuationHint{
		hasPreviousResponseID: strings.TrimSpace(gjson.GetBytes(oai, "previous_response_id").String()) != "",
		hasClientToolID: func(id string) bool {
			return strings.HasPrefix(strings.TrimSpace(id), composerClientToolRoutingTokenPrefix)
		},
	}
}

// composerResponseSessions maps a tenant-scoped OUTWARD response id (the response.id the client sees on a
// turn) -> the bridge session that produced it. H16: a Responses/Codex follow-up that carries
// `previous_response_id` and no tools must resume the DURABLE agent, not get diverted to an ephemeral
// one-shot (utility) or hashed as a "stable conv id" (a previous_response_id changes every turn, so it is
// NOT stable). New IDs authenticate their session route directly; this bounded
// FIFO map is compatibility for IDs emitted before that contract. Key shape:
// tenant + "\x00resp:" + outwardResponseID.
//
// A legacy ID can still miss after restart and fall through to stable-
// conversation recovery. Newly emitted IDs do not depend on process memory.
// Tool-result routing never depends on either mechanism; signed ids always go
// to the bridge journal.
var (
	composerResponseSessionMu    sync.Mutex
	composerResponseSessions     = make(map[string]string)
	composerResponseSessionOrder []string
)

const composerResponseSessionCap = 20000

// recordComposerResponseSession remembers which bridge session produced an outward response id, so a later
// Responses/Codex follow-up that passes it back as previous_response_id resumes the same durable session.
func recordComposerResponseSession(tenant, outwardResponseID, sessionID string) {
	if outwardResponseID == "" || sessionID == "" {
		return
	}
	if strings.HasPrefix(outwardResponseID, composerRoutedResponseIDPrefix) {
		return // the authenticated ID is its own bounded routing record
	}
	key := tenant + "\x00resp:" + outwardResponseID
	composerResponseSessionMu.Lock()
	defer composerResponseSessionMu.Unlock()
	if _, ok := composerResponseSessions[key]; !ok {
		composerResponseSessionOrder = append(composerResponseSessionOrder, key)
		if len(composerResponseSessionOrder) > composerResponseSessionCap {
			oldest := composerResponseSessionOrder[0]
			composerResponseSessionOrder = composerResponseSessionOrder[1:]
			delete(composerResponseSessions, oldest)
		}
	}
	composerResponseSessions[key] = sessionID
}

// lookupComposerResponseSession verifies a restart-safe route embedded in a
// new response ID, then falls back to the legacy process-local map.
func lookupComposerResponseSession(tenant, apiKey, outwardResponseID string) string {
	return lookupComposerResponseRoute(tenant, apiKey, outwardResponseID).sessionID
}

type composerResponseRoute struct {
	sessionID           string
	conversationBinding string
}

func lookupComposerResponseRoute(tenant, apiKey, outwardResponseID string) composerResponseRoute {
	if outwardResponseID == "" {
		return composerResponseRoute{}
	}
	// New composer response ids carry an authenticated opaque session route.
	// This is the restart/multi-replica authority; the bounded process-local
	// map below remains only as backward compatibility for response ids emitted
	// before the routed-id contract existed.
	if route := composerRouteFromResponseID(tenant, apiKey, outwardResponseID); route.sessionID != "" {
		return route
	}
	composerResponseSessionMu.Lock()
	defer composerResponseSessionMu.Unlock()
	return composerResponseRoute{sessionID: composerResponseSessions[tenant+"\x00resp:"+outwardResponseID]}
}

// composerDebugEnabled gates the verbose per-turn [composer] diagnostic logs (session routing, dispatch).
// OFF by default so production logs stay clean; set CURSOR_COMPOSER_DEBUG=1 to enable. Error-level logs
// (bridge non-2xx, transport/scanner errors) are NOT gated — they always report.
var composerDebugEnabled = func() bool {
	return composerEnvTruthyRaw(os.Getenv("CURSOR_COMPOSER_DEBUG"))
}()

func composerDebugf(format string, args ...any) {
	if composerDebugEnabled {
		log.Infof(format, args...)
	}
}

// composerReplayReasoningEnabled gates whether prior assistant reasoning_content is replayed VERBATIM into a
// re-seed transcript (ADD-67). DEFAULT OFF: raw chain-of-thought is internal model state, not ordinary
// conversation text — replaying it as "[thinking: …]" prompt text leaks hidden reasoning across systems,
// bloats the single re-seed message, and differs from provider semantics that treat thinking specially.
// ADD-67 reverses the committed EX9 default (which preserved it). Set CURSOR_COMPOSER_REPLAY_REASONING=1 to
// restore the EX9 behavior if a regression appears. Either way the model still gets answer text + tool-call
// intent; only the raw thinking is omitted (replaced with a neutral marker) by default.
var composerReplayReasoningEnabled = func() bool {
	return composerEnvTruthy(os.Getenv("CURSOR_COMPOSER_REPLAY_REASONING"))
}()

// composerLiveUsageEnabled (CURSOR_COMPOSER_LIVE_USAGE, default OFF) forwards a RUNNING token ESTIMATE DURING the
// stream (throttled) so a composer (sub)agent's token counter grows live instead of sitting at 0 until the terminal
// estimate. The @cursor/sdk streams no usage, so this is the SAME ~4-chars/token estimate as the terminal one, just
// emitted incrementally. OFF by default until the interim message_delta.usage is confirmed to render cleanly in the
// client (it interleaves with content frames, which not every renderer expects mid-stream).
var composerLiveUsageEnabled = func() bool {
	return composerEnvTruthy(os.Getenv("CURSOR_COMPOSER_LIVE_USAGE"))
}()

// composerLiveUsageStepChars throttles the live estimate: emit one running-usage frame per this many new completion
// characters (~4 chars/token, so ~50 tokens) — frequent enough for a smooth counter, sparse enough that frame
// overhead stays negligible.
const composerLiveUsageStepChars = 200

type composerConversationIdentity struct {
	ID     string
	Source string
}

type composerConversationIdentityCandidate struct {
	id     string
	source string
}

type composerConversationIdentityConflictError struct {
	primarySource     string
	conflictingSource string
}

func (e *composerConversationIdentityConflictError) Error() string {
	return fmt.Sprintf("cursor composer: conflicting explicit conversation identities (%s versus %s); start the request with one canonical conversation id", e.primarySource, e.conflictingSource)
}

func (e *composerConversationIdentityConflictError) StatusCode() int { return http.StatusConflict }

func composerConversationIDsEqual(a, b string) bool {
	normalize := func(v string) string {
		v = strings.TrimSpace(v)
		return strings.TrimPrefix(v, "claude:")
	}
	return normalize(a) == normalize(b)
}

// composerConversationIdentityCandidates is the single parser for explicit
// body identities used by both durable routing and turn provenance. Every
// accepted alias participates in conflict detection; no secondary subsystem may
// discover an identity that routing did not validate.
func composerConversationIdentityCandidates(payload []byte) []composerConversationIdentityCandidate {
	if len(payload) == 0 {
		return nil
	}
	candidates := make([]composerConversationIdentityCandidate, 0, 10)
	if id := claudeSessionID(payload); id != "" {
		candidates = append(candidates, composerConversationIdentityCandidate{id: id, source: "metadata.user_id"})
	}
	for _, item := range []struct {
		path   string
		source string
	}{
		{path: "metadata.cliproxy.conversation_id", source: "body.metadata.cliproxy.conversation_id"},
		{path: "metadata.conversation_id", source: "body.metadata.conversation_id"},
		{path: "metadata.conversationId", source: "body.metadata.conversationId"},
		{path: "metadata.session_id", source: "body.metadata.session_id"},
		{path: "metadata.sessionId", source: "body.metadata.sessionId"},
		{path: "conversation_id", source: "body.conversation_id"},
	} {
		if value := strings.TrimSpace(gjson.GetBytes(payload, item.path).String()); value != "" {
			candidates = append(candidates, composerConversationIdentityCandidate{id: value, source: item.source})
		}
	}
	userID := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
	if strings.HasPrefix(userID, "{") {
		for _, key := range []string{"session_id", "conversation_id", "thread_id"} {
			if value := strings.TrimSpace(gjson.Get(userID, key).String()); value != "" {
				candidates = append(candidates, composerConversationIdentityCandidate{
					id: value, source: "metadata.user_id." + key,
				})
			}
		}
	}
	return candidates
}

// resolveComposerConversationIdentity resolves every explicit conversation signal once. Claude Code's
// per-conversation metadata.user_id is authoritative, matching the auth selector. Any contradictory explicit
// signal is rejected instead of silently attaching a /new request to a stale header or response lineage.
func resolveComposerConversationIdentity(opts cliproxyexecutor.Options) (composerConversationIdentity, error) {
	candidates := composerConversationIdentityCandidates(opts.OriginalRequest)
	if opts.Headers != nil {
		// Existing conversation/session headers first, then the additional real conv-id signals that
		// extractSessionIDs / copilot_headers honor (Codex Session_id, Amp thread id, a bare Conversation_id)
		// so a non-Anthropic agentic client keeps a stable session across its turns (EX5).
		//
		// ADD-48: X-Client-Request-Id is REMOVED from this list. By near-universal convention a "request id" is
		// a PER-REQUEST tracing id (unique every HTTP call), NOT a per-conversation id. Treating it as stable
		// minted a NEW bridge session every turn (durable agent / compaction continuity all drift) — a
		// silent multi-turn regression. Clients that genuinely have a stable id set a conversation/session/thread
		// header above (Amp still routes via X-Amp-Thread-Id); a request-id-only turn now degrades gracefully to
		// mint + history re-seed, which is correct, instead of churning sessions. Re-add behind a proven-stable
		// per-client allowlist (with a test that the client's field is conversation-stable), never generically.
		for _, h := range []string{
			"X-Conversation-Id", "X-Session-Id", "X-Cc-Conversation-Id",
			"Session_id", "Conversation_id", "X-Amp-Thread-Id",
		} {
			if v := strings.TrimSpace(opts.Headers.Get(h)); v != "" {
				candidates = append(candidates, composerConversationIdentityCandidate{id: v, source: "header." + h})
			}
		}
	}
	// Body signals that mirror extractSessionIDs steps 7+: a conversation_id is stable across a conversation's
	// turns and never derived from message content. Never derive from message text. H16: previous_response_id
	// is DELIBERATELY EXCLUDED here — it is NOT stable across a conversation (it changes every turn), so
	// hashing it would mint a NEW session every turn and lose context. It is instead resolved via the
	// authenticated routed response id in deriveComposerSessionID; the map is a
	// compatibility fallback for IDs emitted by an older process.
	//
	// ADD-78: prompt_cache_key is DELIBERATELY EXCLUDED too. It is a cache-locality HINT, not a conversation
	// identity: clients reuse a single coarse cache key across SEPARATE tasks that merely share a system
	// prompt / repo context. Hashing it as the stable session preimage merged independent conversations onto
	// one durable Cursor agent (prior tool state / seeded system bleeds across unrelated
	// requests). A turn whose ONLY id is prompt_cache_key now degrades to mint + history re-seed instead.
	if opts.Metadata != nil {
		// ADD-48: request_id/requestId are REMOVED here for the same reason as the header above — a CLIProxy
		// execution-metadata request id is per-call, not per-conversation; keying on it churned a fresh session
		// each turn. Only the explicit conversation/session metadata is conversation-stable.
		for _, k := range []string{"conversation_id", "conversationId", "session_id", "sessionId"} {
			if v, ok := opts.Metadata[k]; ok {
				if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
					candidates = append(candidates, composerConversationIdentityCandidate{id: s, source: "execution_metadata." + k})
				}
			}
		}
	}
	if len(candidates) == 0 {
		return composerConversationIdentity{}, nil
	}
	primary := candidates[0]
	for _, c := range candidates[1:] {
		if !composerConversationIDsEqual(primary.id, c.id) {
			return composerConversationIdentity{}, &composerConversationIdentityConflictError{
				primarySource: primary.source, conflictingSource: c.source,
			}
		}
	}
	return composerConversationIdentity{ID: primary.id, Source: primary.source}, nil
}

// stableConversationID is retained for narrow helpers/tests that only need the resolved value. Request routing
// must call resolveComposerConversationIdentity and propagate its typed conflict error.
func stableConversationID(opts cliproxyexecutor.Options) string {
	identity, err := resolveComposerConversationIdentity(opts)
	if err != nil {
		return ""
	}
	return identity.ID
}

// claudeSessionID extracts a stable per-conversation id from the inbound payload's metadata.user_id
// (Claude Code session format), or "" when absent. It mirrors sdk/cliproxy/auth.extractSessionIDs and
// is kept local to avoid importing the auth package from an executor. Only the per-conversation forms
// (a trailing _session_<uuid>, or a JSON object carrying session_id) are trusted — a bare user_id is
// ignored here because it is per-account, not per-conversation, and would collapse conversations.
func claudeSessionID(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	userID := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
	if userID == "" {
		return ""
	}
	if i := strings.LastIndex(userID, "_session_"); i >= 0 {
		if uuid := strings.TrimSpace(userID[i+len("_session_"):]); uuid != "" {
			return "claude:" + uuid
		}
	}
	if strings.HasPrefix(userID, "{") {
		if sid := strings.TrimSpace(gjson.Get(userID, "session_id").String()); sid != "" {
			return "claude:" + sid
		}
	}
	return ""
}

// composerInvocationID extracts a client-generated logical provider-call
// identity. Unlike clientMessageId (a semantic content hash), this value is
// unique for independently initiated turns and remains stable only while that
// exact turn is retried. Claude-shaped metadata.user_id JSON carries it across
// Anthropic-compatible gateways without a non-standard top-level field;
// generic headers/top-level metadata provide the same contract.
func composerInvocationID(oai []byte, opts cliproxyexecutor.Options) string {
	if id := cliproxyexecutor.InvocationIDFromMetadata(opts.Metadata); cliproxyexecutor.ValidInvocationID(id) {
		return id
	}
	candidates := make([]string, 0, 8)
	if opts.Headers != nil {
		candidates = append(candidates,
			opts.Headers.Get(cliproxyexecutor.HeaderIdempotencyKey),
			opts.Headers.Get(cliproxyexecutor.HeaderCLIProxyInvocationID),
			opts.Headers.Get(cliproxyexecutor.HeaderClientTurnID),
		)
	}
	for _, payload := range [][]byte{opts.OriginalRequest, oai} {
		if len(payload) == 0 {
			continue
		}
		candidates = append(candidates,
			gjson.GetBytes(payload, "metadata.turn_id").String(),
			gjson.GetBytes(payload, "metadata.invocation_id").String(),
		)
		userID := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
		if strings.HasPrefix(userID, "{") {
			candidates = append(candidates,
				gjson.Get(userID, "turn_id").String(),
				gjson.Get(userID, "invocation_id").String(),
			)
		}
	}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if cliproxyexecutor.ValidInvocationID(candidate) {
			return candidate
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Conversation-lineage registry (subordinate same-tenant splitter).
//
// PURPOSE (routing only): distinct logical conversations that SHARE one stable
// conversation id (subagents reusing the parent's metadata.user_id, parallel
// fan-out, branches) must NOT collapse onto a single durable bridge session.
// deriveComposerSessionID stays ID-AUTHORITATIVE; this registry only SPLITS a
// shared baseSid into per-divergent-head lineages and re-resolves each lineage
// to ONE stable sess_ across all of its own turns. It changes WHICH session a
// turn routes to; it adds NO new behavior to the tenant boundary.
//
// The key is (baseSid, headDigest): a SET of co-resident lineages under one
// baseSid, each addressable by its own growth-stable head. The base (parent)
// lineage is just the member whose head equals the parent opener; it is not
// privileged. A fork's id is PURE CONTENT (sha256 of baseSid + head), so the
// same subagent re-derives the same forkSid on every one of its new-user turns.
//
// TODO(identity-finalplan §5.1/§5.2): the per-conversation join here is keyed
// only on inbound content within a tenant. Binding a conversation id to the
// caller's resolved credential (a stored per-caller salt → token, and the same
// check on fresh-turn lineage lookup) is a SEPARATE sign-off decision and is NOT
// implemented here. This remains process-local fresh-turn state only.
const (
	// composerLineagePerTenantCap bounds the number of (baseSid,headDigest) lineages retained PER TENANT.
	// A noisy tenant only evicts its own oldest lineages.
	composerLineagePerTenantCap = 20000
	// composerLineagePerBaseCap bounds the co-resident lineages under ONE baseSid, so a pathological fan-out
	// of forks under a single shared conversation id cannot exhaust the per-tenant cap on its own.
	composerLineagePerBaseCap = 64
	// composerLineageEntryTTL is in-memory housekeeping, not a data-path network timeout.
	composerLineageEntryTTL = 30 * time.Minute
)

// serverSecret authenticates the response-route fallback used by tests and malformed internal calls that have
// no resolved Cursor API key. Production response routes use that request's API key. Lineage digests deliberately
// do not use this secret: their preimage is already tenant-scoped and must remain deterministic across restarts
// even when CURSOR_COMPOSER_LINEAGE_SECRET is unset.
var serverSecret = loadComposerLineageSecret(os.Getenv)

// loadComposerLineageSecret derives the 32-byte lineage HMAC key from the environment (testable via the getenv
// param). A configured CURSOR_COMPOSER_LINEAGE_SECRET (hex or raw text) yields a STABLE, deterministic key; an
// unset/empty value yields a per-process crypto/rand key (single-process sticky). A configured-but-short value
// is hashed to full width (still deterministic) rather than silently falling back to random — a misconfigured
// short secret must not break cross-replica determinism.
func loadComposerLineageSecret(getenv func(string) string) []byte {
	if v := strings.TrimSpace(getenv("CURSOR_COMPOSER_LINEAGE_SECRET")); v != "" {
		if raw, err := hex.DecodeString(v); err == nil && len(raw) >= 32 {
			return raw[:32]
		}
		// Non-hex, or fewer than 32 hex bytes: derive a fixed-width key deterministically from the raw value.
		sum := sha256.Sum256([]byte("cursor-composer-lineage-secret\x00" + v))
		return sum[:]
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure would leave keying uninitialised; fall back to a unique-enough seed rather than a
		// constant key. This path is effectively unreachable on supported platforms.
		log.Errorf("[composer] lineage serverSecret: crypto/rand failed (%v); using a degraded fallback key", err)
		fallback := sha256.Sum256([]byte(fmt.Sprintf("composer-lineage-fallback\x00%d\x00%p", time.Now().UnixNano(), &b)))
		copy(b, fallback[:])
	}
	return b
}

// lineageEntry is one co-resident lineage under a baseSid: the sess_ id this divergent head routes to plus
// the bookkeeping for re-resolution, retry tolerance, and the recorded identical-clone collision slot.
type lineageEntry struct {
	sid        string    // baseSid for the base lineage, forkSid for a fork
	headDigest string    // growth-stable SHA-256 of the retained non-system head
	openerFP   string    // diagnostic fingerprint; never authoritative over a divergent retained head
	slot       int       // recorded identical-head collision slot (0 = first claimant, omitted from the id)
	lastUsed   time.Time // LRU + TTL
}

// tenantLineage holds one tenant's lineages, keyed by baseSid + "\x00" + headDigest, with an LRU order of
// those keys. One mutex per tenant: contention is low and per-tenant
// isolation means a noisy tenant never evicts another's lineage.
type tenantLineage struct {
	mu         sync.Mutex
	byBaseHead map[string]*lineageEntry // baseSid + "\x00" + headDigest -> the co-resident lineage for THAT head
	order      []string                 // LRU of byBaseHead keys (front = oldest)
}

// composerLineageStore is the tenant-partitioned fresh-turn lineage registry; each tenant submap has its
// own mutex/LRU/caps.
type composerLineageStore struct {
	tenants  sync.Map // tenant -> *tenantLineage
	nowFn    func() time.Time
	perCap   int
	perBase  int
	entryTTL time.Duration
}

func newComposerLineageStore() *composerLineageStore {
	return &composerLineageStore{
		nowFn:    time.Now,
		perCap:   composerLineagePerTenantCap,
		perBase:  composerLineagePerBaseCap,
		entryTTL: composerLineageEntryTTL,
	}
}

var composerLineage = newComposerLineageStore()

func lineageKey(baseSid, headDigest string) string {
	return baseSid + "\x00" + headDigest
}

// sha256Sum returns the raw 32-byte SHA-256 of s as a slice (small helper so the stable-conv hashing sites
// read uniformly: "sess_" + hex(sha256Sum(preimage))[:32]).
func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// composerRandHex returns n random bytes as lowercase hex (session/response/correlation ids).
func composerRandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// composerStableBaseSessionID is the content-pure stable session id for a tenant+key+conversation.
func composerStableBaseSessionID(tenant, apiKey, convID string) string {
	return "sess_" + hex.EncodeToString(sha256Sum(composerStableSessionPreimage(tenant, apiKey, convID)))[:32]
}

// composerConversationBinding is the opaque, authenticated conversation epoch shared with the bridge. It is
// deliberately the full digest (rather than the truncated external session id) so ToolRounds can reject a
// signed old-conversation route carrying fresh post-/new user intent without learning the client identity.
func composerConversationBinding(tenant, apiKey, convID string) string {
	if tenant == "" || convID == "" {
		return ""
	}
	return hex.EncodeToString(sha256Sum(composerStableSessionPreimage(tenant, apiKey, convID)))
}

func composerConversationKey(opts cliproxyexecutor.Options, messages []gjson.Result) (string, error) {
	identity, err := resolveComposerConversationIdentity(opts)
	if err != nil {
		return "", err
	}
	if identity.ID != "" {
		return identity.ID, nil
	}
	return composerContentConvKey(messages), nil
}

// lineageHeadDigest is the growth-stable head used as a lineage key component (§1.2). It hashes the full text
// and tool-routing structure in the retained non-system head window, avoiding the old 256-rune-prefix alias.
// The bounded head window keeps the key stable as a transcript grows. Tenant scoping prevents cross-tenant
// aliases, and plain SHA-256 makes the same lineage deterministic across restarts even when no optional secret
// is configured. The registry retains only the digest, never raw conversation text.
func lineageHeadDigest(tenant, _ string, messages []gjson.Result) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(tenant))
	_, _ = digest.Write([]byte{0})
	head := composerNonSystemHeadMessages(messages)
	limit := len(head)
	if limit > composerHistoryFingerprintHeadMessages {
		limit = composerHistoryFingerprintHeadMessages
	}
	for i := 0; i < limit; i++ {
		m := head[i]
		_, _ = io.WriteString(digest, strings.ToLower(strings.TrimSpace(m.Get("role").String())))
		_, _ = digest.Write([]byte{0x1f})
		// Lineage isolation hashes full text and structural tool routing fields. The history fingerprint remains
		// prefix-bounded for cheap reseed detection, but two tasks sharing a 256-rune preamble must not collapse.
		_, _ = io.WriteString(digest, cursorMessageText(m))
		for _, tc := range m.Get("tool_calls").Array() {
			_, _ = digest.Write([]byte{0x1e})
			_, _ = io.WriteString(digest, tc.Get("id").String())
			_, _ = digest.Write([]byte{0x1f})
			_, _ = io.WriteString(digest, tc.Get("function.name").String())
		}
		_, _ = io.WriteString(digest, m.Get("tool_call_id").String())
		_, _ = digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))[:32]
}

// lineageHeadDigestFromMessages folds composerHistoryFingerprint + lineageHeadDigest for a message array.
func lineageHeadDigestFromMessages(tenant string, messages []gjson.Result) string {
	return lineageHeadDigest(tenant, composerHistoryFingerprint(messages), messages)
}

// lineageForkKeys returns the head digest and opener fingerprint for fork routing on messages.
func lineageForkKeys(tenant string, messages []gjson.Result) (headDigest, openerFP string) {
	headDigest = lineageHeadDigestFromMessages(tenant, messages)
	openerFP = lineageOpenerFingerprint(messages)
	return
}

// composerExplicitLineageID is an optional client contract for branches that can be byte-identical. Without
// such an identity no server can later prove whether a fresh identical request owns collision slot 0 or 1.
func composerExplicitLineageID(opts cliproxyexecutor.Options) string {
	if opts.Headers != nil {
		if v := strings.TrimSpace(opts.Headers.Get("X-CLIProxy-Lineage-ID")); v != "" {
			return v
		}
	}
	for _, path := range []string{"metadata.cliproxy.lineage_id", "metadata.lineage_id", "metadata.branch_id"} {
		if v := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, path).String()); v != "" {
			return v
		}
	}
	if opts.Metadata != nil {
		for _, key := range []string{"lineage_id", "lineageId", "branch_id", "branchId"} {
			if v, ok := opts.Metadata[key]; ok {
				if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func lineageForkKeysForRequest(tenant string, messages []gjson.Result, opts cliproxyexecutor.Options) (headDigest, openerFP string) {
	headDigest, openerFP = lineageForkKeys(tenant, messages)
	if lineageID := composerExplicitLineageID(opts); lineageID != "" {
		// A client lineage id is authoritative branch identity, not merely extra entropy for one content head.
		// Do not mix the growing transcript into it: doing so would move the same branch to a new durable agent
		// when turn two adds the first assistant message or when compaction rewrites retained history.
		sum := sha256.Sum256([]byte(tenant + "\x00lineage-id:" + lineageID))
		headDigest = hex.EncodeToString(sum[:])[:32]
	}
	return
}

// forkSessionID derives a fork's stable sess_ id from baseSid + the fork's growth-stable head (§1.3c). slot 0
// is OMITTED for back-compat (the common single-claimant case); a recorded slot N>0 splits a byte-identical
// concurrent clone that shares both id and head. The id is pure content + a RECORDED slot — never re-minted.
func forkSessionID(baseSid, headDigest string, slot int) string {
	pre := baseSid + "\x00fork:" + headDigest
	if slot > 0 {
		pre = pre + "\x00slot:" + fmt.Sprint(slot)
	}
	sum := sha256.Sum256([]byte(pre))
	return "sess_" + hex.EncodeToString(sum[:])[:32]
}

// stableLineageSessionID removes process arrival order from durable routing. The bounded two-message head is
// growth-stable while distinguishing branches/subagents that share the same opener. A compact that rewrites
// that head may re-seed onto a new deterministic agent after process-state loss; isolation is safer than
// guessing that a same-opener divergent head belongs to the parent.
func stableLineageSessionID(baseSid, headDigest, _ string) string {
	return forkSessionID(baseSid, "lineage:"+headDigest, 0)
}

// expireLocked drops lineages older than entryTTL for a tenant. Caller holds to.mu.
func (s *composerLineageStore) expireLocked(tenant string, to *tenantLineage) {
	if s.entryTTL <= 0 {
		return
	}
	cutoff := s.nowFn().Add(-s.entryTTL)
	kept := to.order[:0]
	for _, key := range to.order {
		e, ok := to.byBaseHead[key]
		if !ok {
			continue
		}
		if e.lastUsed.Before(cutoff) && !composerSessionIsLive(tenant, e.sid) {
			delete(to.byBaseHead, key)
			continue
		}
		kept = append(kept, key)
	}
	to.order = kept
}

// enforceTenantCapLocked evicts the oldest non-live lineage. The cap is soft when every entry is live: reusing
// a live collision slot would route two logical runs onto one serial bridge session.
func (to *tenantLineage) enforceTenantCapLocked(tenant string, cap int) {
	for len(to.order) > cap {
		evicted := false
		for i, key := range to.order {
			e := to.byBaseHead[key]
			if e != nil && composerSessionIsLive(tenant, e.sid) {
				continue
			}
			delete(to.byBaseHead, key)
			to.order = append(to.order[:i], to.order[i+1:]...)
			evicted = true
			break
		}
		if !evicted {
			return
		}
	}
}

// moveToTail moves an existing LRU key to the tail (most-recently-used). Caller holds to.mu.
func (to *tenantLineage) moveToTail(key string) {
	for i, k := range to.order {
		if k == key {
			to.order = append(to.order[:i], to.order[i+1:]...)
			to.order = append(to.order, key)
			return
		}
	}
}

// countBaseLocked returns how many co-resident lineages currently exist under baseSid. Caller holds to.mu.
func (to *tenantLineage) countBaseLocked(baseSid string) int {
	n := 0
	prefix := baseSid + "\x00"
	for k := range to.byBaseHead {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// evictOldestForBaseLocked drops the oldest co-resident NON-LIVE lineage under baseSid (front-most in LRU order)
// and returns true. #19 (review): it SKIPS any lineage whose session is still LIVE (a held logical-run lease or
// live logical-run lease) so a concurrent fork's continuity is never evicted just to make room. If EVERY
// co-resident lineage is live, it evicts nothing and returns false — the caller then exceeds the per-base cap
// SOFTLY (the global perCap still bounds total) rather than dropping a live fork and stranding its continuation.
// Used to bound a pathological fan-out under one conversation id by composerLineagePerBaseCap. Caller holds to.mu.
func (to *tenantLineage) evictOldestForBaseLocked(tenant, baseSid string) bool {
	prefix := baseSid + "\x00"
	for i, key := range to.order {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if e := to.byBaseHead[key]; e != nil && composerSessionIsLive(tenant, e.sid) {
			continue // #19: this fork is still running -> do not evict its lineage
		}
		delete(to.byBaseHead, key)
		to.order = append(to.order[:i], to.order[i+1:]...)
		return true
	}
	return false
}

// enforcePerBaseCapLocked evicts NON-LIVE co-resident lineages under baseSid until the per-base count is below
// perBase, stopping early if every remaining co-resident lineage is live (#19: never evict a live fork —
// exceed the cap softly; the global perCap still bounds total). Caller holds to.mu.
func (to *tenantLineage) enforcePerBaseCapLocked(tenant, baseSid string, perBase int) {
	for to.countBaseLocked(baseSid) >= perBase {
		if !to.evictOldestForBaseLocked(tenant, baseSid) {
			break
		}
	}
}

// tenantLineageFor returns the tenant submap, creating it on demand.
func (s *composerLineageStore) tenantLineageFor(tenant string) *tenantLineage {
	if v, ok := s.tenants.Load(tenant); ok {
		return v.(*tenantLineage)
	}
	created := &tenantLineage{byBaseHead: map[string]*lineageEntry{}}
	actual, _ := s.tenants.LoadOrStore(tenant, created)
	return actual.(*tenantLineage)
}

// deleteTenantIfEmptyLocked removes the tenant submap when it holds no lineages (mirrors forgetSession). The
// caller holds to.mu; the sync.Map delete is safe to interleave with that per-tenant lock.
func (s *composerLineageStore) deleteTenantIfEmptyLocked(tenant string, to *tenantLineage) {
	if len(to.byBaseHead) == 0 {
		s.tenants.Delete(tenant)
	}
}

// putForkSlotted records a FIRST-CLASS fork under (baseSid, headDigest), resolving the identical-clone
// collision slot (§1.3c). The first claimant of a head gets slot 0 (id omits the slot, back-compat). A
// SECOND concurrent claimant of the SAME head within the live window — signalled by collide=true — gets the
// next recorded integer slot, disambiguating byte-identical openings without collision risk. A client-provided
// stable lineage id is still required to prove which identical slot owns a later turn. Returns the fork's sid.
func (s *composerLineageStore) putForkSlotted(tenant, baseSid, headDigest string, collide bool) string {
	if tenant == "" {
		// Empty tenant never reaches here (deriveComposerSessionID mints first); return a deterministic id.
		return forkSessionID(baseSid, headDigest, 0)
	}
	to := s.tenantLineageFor(tenant)
	to.mu.Lock()
	defer to.mu.Unlock()
	s.expireLocked(tenant, to)
	now := s.nowFn()
	key := lineageKey(baseSid, headDigest)
	if e, ok := to.byBaseHead[key]; ok {
		if !collide {
			// Re-attach to the existing fork for this head (the common multi-turn re-resolve).
			e.lastUsed = now
			to.moveToTail(key)
			return e.sid
		}
		// A genuinely concurrent identical-head clone: allocate the next slot and record it as its own
		// lineage keyed by the SAME (baseSid, headDigest) but stored under a slot-suffixed map key so it is
		// independently addressable. #10 (review): allocate the SMALLEST UNUSED slot (>=1) under (baseSid,
		// headDigest) DETERMINISTICALLY, under the tenant lock — never a random 16-bit slot, which could
		// birthday-collide at the per-base cap (64) and OVERWRITE a live sibling's lineage. A monotonic slot also
		// makes slot selection reproducible while the registry is present and reuses slots freed by eviction.
		// The external session id remains an opaque SHA-256-derived value.
		slot := 1
		slotKey := key + "\x00slot:" + fmt.Sprint(slot)
		for {
			if _, exists := to.byBaseHead[slotKey]; !exists {
				break
			}
			slot++
			slotKey = key + "\x00slot:" + fmt.Sprint(slot)
		}
		forkSid := forkSessionID(baseSid, headDigest, slot)
		to.enforcePerBaseCapLocked(tenant, baseSid, s.perBase) // #19: evict only NON-live forks (else exceed softly)
		ne := &lineageEntry{sid: forkSid, headDigest: headDigest, slot: slot, lastUsed: now}
		to.byBaseHead[slotKey] = ne
		to.order = append(to.order, slotKey)
		to.enforceTenantCapLocked(tenant, s.perCap)
		return forkSid
	}
	// First claimant of this head: slot 0, id omits the slot for back-compat.
	to.enforcePerBaseCapLocked(tenant, baseSid, s.perBase) // #19: evict only NON-live forks (else exceed softly)
	forkSid := forkSessionID(baseSid, headDigest, 0)
	e := &lineageEntry{sid: forkSid, headDigest: headDigest, lastUsed: now}
	to.byBaseHead[key] = e
	to.order = append(to.order, key)
	to.enforceTenantCapLocked(tenant, s.perCap)
	return forkSid
}

// resolveStableSession is the authoritative branch-3 resolver (identity-finalplan §1.3), executed ATOMICALLY
// under the tenant lock so concurrent turns of one conversation cannot race into divergent sids. It returns
// the bridge session id for a new-user turn that carries a stable conversation id (baseSid), splitting
// distinct divergent contexts that share that id into per-lineage sessions and re-resolving each lineage to
// ONE stable sess_ across all of its own turns.
//
// Resolution order:
//
//	(a) EXACT head match (base OR a prior fork) -> CONTINUE that lineage's recorded sid (the steady-state
//	    fast path: turns 3,5,7… of any conversation/fork whose head has stabilised at 2 messages).
//	(b) OPENER bridge -> a co-resident lineage whose recorded opener fingerprint matches the current opener
//	    but whose head changed is the SAME conversation/fork with a rewritten or GROWN body (a /compact, OR
//	    the unavoidable turn-1→turn-3 growth from a 1-message head to a 2-message head). Re-key it to the new
//	    head and CONTINUE its recorded sid — so a fork's forkSid is computed ONCE at establishment and never
//	    recomputed (multi-turn fork stability). The base lineage is preferred over a fork on an opener tie.
//	(c) No co-resident lineage shares this opener -> a genuinely new divergent context. If NO base exists yet
//	    for baseSid, this head ESTABLISHES the base (sid == baseSid, the legacy single-conversation path).
//	    Otherwise it is a FIRST-CLASS fork (subagent / branch / parallel fan-out) recorded under
//	    (baseSid, headDigest) so its later new-user turns re-resolve via (a)/(b).
func (s *composerLineageStore) resolveStableSession(tenant, baseSid, headDigest, openerFP string) string {
	return s.resolveStableSessionForTurn(tenant, baseSid, headDigest, openerFP, "")
}

func (s *composerLineageStore) resolveStableSessionForTurn(tenant, baseSid, headDigest, openerFP, turnDiscriminator string) string {
	if tenant == "" {
		return baseSid
	}
	to := s.tenantLineageFor(tenant)
	to.mu.Lock()
	defer to.mu.Unlock()
	s.expireLocked(tenant, to)
	now := s.nowFn()
	// (a) exact retained-head match, co-resident under this baseSid.
	key := lineageKey(baseSid, headDigest)
	if e, ok := to.byBaseHead[key]; ok {
		if turnDiscriminator != "" {
			prefix := key + "\x00slot:"
			for siblingKey := range to.byBaseHead {
				if strings.HasPrefix(siblingKey, prefix) {
					// The client supplied no stable branch identity and multiple identical-head owners exist.
					// Never guess slot 0: isolate this exact retry deterministically and rely on bounded replay.
					return forkSessionID(baseSid, headDigest+"\x00ambiguous-turn:"+turnDiscriminator, 0)
				}
			}
		}
		e.lastUsed = now
		to.moveToTail(key)
		return e.sid
	}
	// A matching opener with a different retained head is not authoritative. It can mean compaction, but it can
	// equally mean a divergent branch/subagent. Treating it as continuation collapses those lineages and can
	// attach a child to its parent. Route by the deterministic retained head and let bounded history faithfully
	// re-seed compaction; never trade isolation for an unprovable same-opener guess.

	// (c) genuinely new divergent context for this baseSid. Every lineage, including the parent, uses a
	// content-pure id. The former "first claimant gets baseSid" rule was process-order-dependent: after a
	// restart/TTL expiry, a subagent arriving first could resume the parent's durable agent.
	to.enforcePerBaseCapLocked(tenant, baseSid, s.perBase) // #19: evict only NON-live forks (else exceed softly)
	sid := stableLineageSessionID(baseSid, headDigest, openerFP)
	e := &lineageEntry{sid: sid, headDigest: headDigest, openerFP: openerFP, lastUsed: now}
	to.byBaseHead[key] = e
	to.order = append(to.order, key)
	to.enforceTenantCapLocked(tenant, s.perCap)
	return sid
}

func lineageTurnDiscriminator(messages []gjson.Result) string {
	h := sha256.New()
	for _, m := range messages {
		_, _ = io.WriteString(h, m.Raw)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// forget drops every lineage routing to sid for a tenant (terminal-stop release). Best-effort cleanup; the
// TTL+LRU age entries out regardless, so this is an optimization, not correctness-critical.
func (s *composerLineageStore) forget(tenant, sid string) {
	if tenant == "" || sid == "" {
		return
	}
	v, ok := s.tenants.Load(tenant)
	if !ok {
		return
	}
	to := v.(*tenantLineage)
	to.mu.Lock()
	defer to.mu.Unlock()
	kept := to.order[:0]
	for _, key := range to.order {
		e, ok := to.byBaseHead[key]
		if !ok {
			continue
		}
		if e.sid == sid {
			delete(to.byBaseHead, key)
			continue
		}
		kept = append(kept, key)
	}
	to.order = kept
	s.deleteTenantIfEmptyLocked(tenant, to)
}

// lineageForget drops a tenant's lineages that route to sid (terminal-stop cleanup helper).
func lineageForget(tenant, sid string) {
	composerLineage.forget(tenant, sid)
}

// ---------------------------------------------------------------------------
// Concurrency-fork in-flight registry (ISOLATION invariant).
//
// A cursor/bridge session is inherently SERIAL: the bridge runs ONE /agent/turn per session at a time and
// FIFO-queues the rest. So two NEW-USER turns that resolve to the SAME session while one is already in flight
// are, by construction, DIFFERENT logical agents (subagents reusing the parent metadata.user_id, parallel
// fan-out) — their content is byte-identical at the opener so the lineage splitter cannot tell them apart, but
// their CONCURRENCY can. This registry tracks which (tenant, sessionID) currently hold an in-flight turn so the
// derive path can FORK a colliding concurrent new-user turn onto a distinct recorded slot session (the
// previously-dead putForkSlotted collide path), letting the siblings run in PARALLEL instead of serializing
// through one bridge session (the proven sess_ collapse).
const (
	// composerInflightStaleTTL bounds a hold: a leaked acquire (a turn that aborts/panics without releasing)
	// self-heals after this so a session does not fork forever. Longer than any real turn, shorter than the
	// bridge session TTL.
	composerInflightStaleTTL = 15 * time.Minute
	// composerForkAcquireMaxAttempts bounds the (vanishingly rare) retry when a freshly-minted fork slot is
	// itself held by an earlier concurrent sibling.
	composerForkAcquireMaxAttempts = 8
	// Bridge pings are time-based, so lease refresh must be time-based too. An
	// event-count cadence can exceed the TTL by hours on a quiet reasoning run.
	composerInflightHeartbeatInterval = composerInflightStaleTTL / 3
)

func composerLeaseHeartbeatDue(last, now time.Time) bool {
	return last.IsZero() || !now.Before(last.Add(composerInflightHeartbeatInterval))
}

// composerLeaseEntry is one held session lease (a logical run in flight on this session).
type composerLeaseEntry struct {
	owner     uint64    // unique per logical request — only its run may touch/release (review P0-1: no clobber)
	since     time.Time // last activity, for the staleness self-heal across tool pauses
	requestID string    // durable clientMessageId; an exact retry reattaches instead of concurrency-forking
}

type composerTenantInflight struct {
	mu   sync.Mutex
	held map[string]composerLeaseEntry // sessionID -> the logical-run lease currently on it
}

// composerLeaseSeq mints owner tokens GLOBALLY (across all tenants/sessions), not per-tenant. release() GCs an
// empty tenant struct (s.tenants.Delete), which would reset a per-tenant counter and RECYCLE owner tokens — a
// stale late-release from a superseded run could then match a fresh claim's recycled token and clobber its lease
// (the exact P0-1 hazard the token defends against). A process-global monotonic counter is immune to tenant GC.
var composerLeaseSeq atomic.Uint64

// composerInflightStore is the tenant-partitioned per-session logical-run lease (mirrors composerLineage's
// shape). A lease is the SERIAL-SESSION invariant made explicit: at most one logical run holds a session at a
// time. Crucially the lease spans an entire logical run — it is claimed when a new-user turn starts and released
// only when a turn on that session reaches a TERMINAL end (end_turn/error), NOT at the tool_use pause between
// HTTP turns (Comment 1). So a concurrent new-user turn arriving mid-tool-loop still sees the session held and
// FORKS, instead of collapsing onto the paused run.
type composerInflightStore struct {
	tenants sync.Map // tenant -> *composerTenantInflight
	nowFn   func() time.Time
	ttl     time.Duration
}

func newComposerInflightStore() *composerInflightStore {
	return &composerInflightStore{nowFn: time.Now, ttl: composerInflightStaleTTL}
}

var composerInflight = newComposerInflightStore()

func (s *composerInflightStore) tenantFor(tenant string) *composerTenantInflight {
	if v, ok := s.tenants.Load(tenant); ok {
		return v.(*composerTenantInflight)
	}
	created := &composerTenantInflight{held: map[string]composerLeaseEntry{}}
	actual, _ := s.tenants.LoadOrStore(tenant, created)
	return actual.(*composerTenantInflight)
}

// claim acquires the logical-run lease on (tenant, sid). Returns an OWNER TOKEN and true if the caller now holds
// it (it was free or a prior lease went stale past the TTL), or (0, false) if another logical run currently
// holds it (the caller must fork). The token is REQUIRED to touch/release (review P0-1): a stale takeover mints
// a NEW owner, so a previous run's late release can never evict a successor's lease. Empty tenant/sid are never
// tracked (mints are unique by construction) and always claim with token 0 (release/touch then no-op).
func (s *composerInflightStore) claimForRequest(tenant, sid, requestID string) (uint64, bool) {
	if tenant == "" || sid == "" {
		return 0, true
	}
	requestID = strings.TrimSpace(requestID)
	to := s.tenantFor(tenant)
	to.mu.Lock()
	defer to.mu.Unlock()
	now := s.nowFn()
	if e, ok := to.held[sid]; ok && now.Sub(e.since) < s.ttl {
		// clientMessageId is the end-to-end idempotency identity. A second
		// delivery of the same logical request must stay on the original session
		// (where the bridge can hand off the live response or replay its durable
		// receipt), never fork to a sibling and execute twice.
		if requestID != "" && e.requestID == requestID {
			e.since = now
			to.held[sid] = e
			return e.owner, true
		}
		return 0, false
	}
	owner := composerLeaseSeq.Add(1) // global monotonic mint — survives tenant GC (see composerLeaseSeq)
	to.held[sid] = composerLeaseEntry{owner: owner, since: now, requestID: requestID}
	return owner, true
}

// claim preserves the original anonymous-claim contract for tests and the few
// internal callers that do not have a durable request identity. Anonymous
// concurrent claims remain distinct logical runs and therefore must fork.
func (s *composerInflightStore) claim(tenant, sid string) (uint64, bool) {
	return s.claimForRequest(tenant, sid, "")
}

// touch refreshes a held lease's activity time — keeps the logical run alive across a long stream or multi-round
// tool loop so the lease does not expire mid-run. No-op unless the caller still OWNS the lease (token matches).
func (s *composerInflightStore) touch(tenant, sid string, owner uint64) {
	if tenant == "" || sid == "" || owner == 0 {
		return
	}
	v, ok := s.tenants.Load(tenant)
	if !ok {
		return
	}
	to := v.(*composerTenantInflight)
	to.mu.Lock()
	defer to.mu.Unlock()
	if e, ok := to.held[sid]; ok && e.owner == owner {
		e.since = s.nowFn()
		to.held[sid] = e
	}
}

// release frees the lease on (tenant, sid) — a turn reached a TERMINAL end, errored, or its dispatch definitely
// did not create a bridge run. No-op unless the caller still OWNS the lease (token matches), so a stale/late
// release from a superseded run cannot evict the successor that took over the session (review P0-1).
func (s *composerInflightStore) release(tenant, sid string, owner uint64) {
	if tenant == "" || sid == "" || owner == 0 {
		return
	}
	v, ok := s.tenants.Load(tenant)
	if !ok {
		return
	}
	to := v.(*composerTenantInflight)
	to.mu.Lock()
	defer to.mu.Unlock()
	if e, ok := to.held[sid]; ok && e.owner == owner {
		delete(to.held, sid)
	}
	if len(to.held) == 0 {
		s.tenants.Delete(tenant)
	}
}

// held reports whether (tenant, sid) currently has a NON-stale logical-run lease — i.e. a run is live on that
// session. #19: lineage eviction consults this so it never drops the lineage of a fork whose run is still going.
func (s *composerInflightStore) held(tenant, sid string) bool {
	if tenant == "" || sid == "" {
		return false
	}
	v, ok := s.tenants.Load(tenant)
	if !ok {
		return false
	}
	to := v.(*composerTenantInflight)
	to.mu.Lock()
	defer to.mu.Unlock()
	e, ok := to.held[sid]
	return ok && s.nowFn().Sub(e.since) < s.ttl
}

// composerSessionIsLive reports whether a fresh logical turn still holds the
// local serialization lease. Tool-result ownership lives only in the bridge.
func composerSessionIsLive(tenant, sid string) bool {
	return composerInflight.held(tenant, sid)
}

// composerAcquireOrFork claims the content-resolved session `sid` for a new-user turn, or — if `sid` is already
// running a logical run (a concurrent sibling) — allocates a DISTINCT fork session (the putForkSlotted collide
// path) and claims THAT, so the siblings run in PARALLEL. Returns the session id to use AND the lease owner
// token the caller MUST pass to touch/release. baseSid+headDigest key the slot.
func composerAcquireOrFork(tenant, sid, baseSid, headDigest, requestID string) (string, uint64) {
	if owner, ok := composerInflight.claimForRequest(tenant, sid, requestID); ok {
		return sid, owner
	}
	for attempt := 0; attempt < composerForkAcquireMaxAttempts; attempt++ {
		forkSid := composerLineage.putForkSlotted(tenant, baseSid, headDigest, true)
		if forkSid == "" || forkSid == sid {
			break
		}
		if owner, ok := composerInflight.claimForRequest(tenant, forkSid, requestID); ok {
			composerDebugf("[composer] concurrency-fork: %s in-flight -> sibling sessionID=%s", sid, forkSid)
			return forkSid, owner
		}
	}
	// Pathological: could not allocate a free sibling. Degrade honestly — route to the original sid (the bridge
	// serializes it) rather than dropping the turn; logged so the cap can be raised if it ever recurs.
	composerDebugf("[composer] concurrency-fork: no free sibling for %s; serializing", sid)
	return sid, 0
}

// compactionSignal is a diagnostic/test signal only. Same opener plus changed head is compatible with both
// compaction and a divergent branch, so it is deliberately not routing authority.
func compactionSignal(oldHead, newHead, recordedOpenerFP, currentOpenerFP string) bool {
	if oldHead == newHead {
		return false // the recorded base head did not change at all
	}
	if recordedOpenerFP == "" || currentOpenerFP == "" {
		return false // cannot confirm opener preservation -> do not bridge (fork instead)
	}
	return recordedOpenerFP == currentOpenerFP
}

// lineageOpenerFingerprint returns a 32-hex diagnostic fingerprint of the first non-system message. It helps
// observe compaction-like rewrites but is never sufficient to merge lineages. Empty when no such message exists.
func lineageOpenerFingerprint(messages []gjson.Result) string {
	head := composerNonSystemHeadMessages(messages)
	if len(head) == 0 {
		return ""
	}
	m := head[0]
	sum := sha256.Sum256([]byte(m.Get("role").String() + "\x1f" + composerHeadMessageText(m)))
	return hex.EncodeToString(sum[:])[:32]
}

// composerContentConvKey derives a CLIENT-AGNOSTIC, turn-stable conversation key for a turn that carries no
// EXPLICIT client conversation id (no conv/session/thread header, no metadata.user_id, no conversation_id).
// Both the OpenAI chat and Anthropic messages APIs are STATELESS — the client resends the full transcript
// every turn — so the conversation OPENER (the first user message) is byte-identical on every turn, including
// new-user follow-ups (the first user message never changes as history grows). lineageOpenerFingerprint
// hashes ONLY that first non-system message, so a volatile system prompt (timestamps/cwd/git status) never
// breaks turn stability. This is the built-in equivalent of the Anthropic path's metadata.user_id for clients
// that send no id (e.g. opendesign, raw OpenAI/SDK callers, simple UIs). The result is namespaced so it can
// never alias a real client conversation id, and "" when there is no opener at all. Unlike the ADD-78
// prompt_cache_key (a coarse key SHARED across separate tasks), the opener IS the task, so distinct tasks key
// distinctly; the lineage head-digest still SPLITS two conversations that share an opener once their histories
// diverge, and deriveComposerSessionIDLive forks concurrent same-opener turns — so the only residual cost is a
// brief turn-1 overlap for two conversations whose FIRST user message is byte-identical.
func composerContentConvKey(messages []gjson.Result) string {
	if fp := lineageOpenerFingerprint(messages); fp != "" {
		return "openerfp:" + fp
	}
	return ""
}

// callerCredential returns the inbound CLIProxy client credential for tenant scoping, covering every
// credential form the access layer accepts (Authorization / X-Goog-Api-Key / X-Api-Key headers and the
// ?key= / ?auth_token= query params), so callers authenticating via any of them are isolated. A given
// client always sends the same form, so whichever is present is stable across the conversation's turns.
func callerCredential(opts cliproxyexecutor.Options) string {
	if opts.Headers != nil {
		for _, h := range []string{"Authorization", "X-Goog-Api-Key", "X-Api-Key"} {
			if v := strings.TrimSpace(opts.Headers.Get(h)); v != "" {
				return v
			}
		}
	}
	if opts.Query != nil {
		for _, q := range []string{"key", "auth_token"} {
			if v := strings.TrimSpace(opts.Query.Get(q)); v != "" {
				return v
			}
		}
	}
	return ""
}

// composerTenant returns the credential+caller scope that isolates one user's sessions and tool-call
// mappings from another's: auth.ID (or api_key) folded with the inbound caller credential. auth.ID is
// per-Cursor-key, not per-end-user, so folding in the caller credential separates distinct users who
// share one upstream Cursor key. Only ever used inside a sha256 pre-image / map key, never logged.
func composerTenant(auth *cliproxyauth.Auth, opts cliproxyexecutor.Options) string {
	tenant := ""
	if auth != nil {
		tenant = auth.ID
		if tenant == "" && auth.Attributes != nil {
			tenant = auth.Attributes["api_key"]
		}
	}
	if caller := callerCredential(opts); caller != "" {
		tenant = tenant + "\x00caller:" + caller
	}
	return tenant
}

// composerKeyFingerprint returns a NON-REVERSIBLE 16-hex fingerprint of the upstream Cursor API key (ADD-79,
// exec half). It is folded into the stable-conversation session-id preimage so a ROTATED Cursor key yields a
// DIFFERENT sess_ id for the same conversation — the rotated turn lands on a fresh durable agent (re-seeded
// from history) rather than continuing under the old key's stale/revoked credentials and wrong billing
// attribution. The bridge half (rotateForKeyChange on REUSE) is independently safe; both halves agree the
// preimage shape is tenant + "\x00key:" + fp + "\x00conv:" + id. Empty key -> "" (no fingerprint folded), so
// the default single-tenant path (key equals tenant id anyway) is unchanged. Only ever used inside a sha256
// preimage, never logged.
func composerKeyFingerprint(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cursor-key\x00" + apiKey))
	return hex.EncodeToString(sum[:])[:16]
}

// composerStableSessionPreimage builds the deterministic stable-conversation session-id preimage shared by
// every stable-conv hashing site in deriveComposerSessionID (ADD-79): tenant + the non-reversible Cursor-key
// fingerprint + the conversation id. Folding the key fingerprint in means a key rotation re-routes the same
// conversation to a fresh sess_ id (and thus a fresh durable agent), matching the bridge's rotateForKeyChange.
func composerStableSessionPreimage(tenant, apiKey, convID string) string {
	if fp := composerKeyFingerprint(apiKey); fp != "" {
		return tenant + "\x00key:" + fp + "\x00conv:" + convID
	}
	return tenant + "\x00conv:" + convID
}

func composerHasReplayableHistory(oai []byte) bool {
	count := 0
	for _, message := range gjson.GetBytes(oai, "messages").Array() {
		switch message.Get("role").String() {
		case "system", "developer", "":
			continue
		default:
			count++
			if count >= 2 {
				return true
			}
		}
	}
	return false
}

// deriveComposerSessionID returns the bridge session id for this turn, scoped to the selected credential
// (tenant boundary) so different users never share a bridge session / SDK stateRoot. When a stable
// conversation identifier is present (request header, inbound metadata.user_id, body conv-id/cache-key, or
// CLIProxy session metadata) the id is derived deterministically from it. Signed tool-call IDs remain opaque
// here and are resolved by the bridge's /agent/continue endpoint. It NEVER routes by message content. When
// nothing stable is available it DEGRADES GRACEFULLY by minting a fresh session
// (the continuation carries history+system so the bridge re-seeds) instead of failing a routine turn — the
// error return is retained in the signature for callers but is no longer produced on a routine turn.
//
// ADD-79 (exec half): apiKey is the resolved upstream Cursor key. A non-reversible fingerprint of it is folded
// into the stable-conv preimage (composerStableSessionPreimage) so a ROTATED key re-routes the conversation to
// a fresh sess_ id — defense in depth alongside the bridge's rotateForKeyChange-on-reuse.
//
// C05 (too-long recovery): the EXTERNAL session id this function returns is the routing key only; it stays
// STABLE across a conversation so continuations keep routing here. The decoupling between this external id
// and the DURABLE Cursor agentId lives entirely in the bridge: on ERROR_CONVERSATION_TOO_LONG the bridge
// tombstones the poisoned durable agent and rotates session.agentId (e.g. <sid>_r2), forcing a re-seed —
// WITHOUT changing the external id. The executor must NOT mint a rotated id (that would fork routing and
// orphan continuations); it only needs to keep returning the same external id, which it does. The
// continuation carries history+system (composerInput) so the rotated durable agent re-seeds bounded context.
func deriveComposerSessionID(auth *cliproxyauth.Auth, apiKey string, oai []byte, opts cliproxyexecutor.Options) (string, error) {
	tenant := composerTenant(auth, opts)
	identity, identityErr := resolveComposerConversationIdentity(opts)
	if identityErr != nil {
		return "", identityErr
	}
	explicitConversationID := identity.ID
	explicitBinding := composerConversationBinding(tenant, apiKey, explicitConversationID)
	// Empty-tenant fail-closed guard (identity-finalplan §1.4): with no auth.ID/api_key AND no caller
	// credential there is no isolation boundary, so a stable id from one anonymous caller could be replayed
	// by another. NEVER share a "" bucket and NEVER consult the lineage registry — mint a fresh session
	// immediately. The continuation carries history+system so the bridge re-seeds rather than losing the turn.
	if tenant == "" {
		sid := mintComposerSessionID()
		composerDebugf("[composer] deriveSessionID BRANCH=mint(empty tenant, fail-closed) -> sessionID=%s", sid)
		return sid, nil
	}
	// Isolate non-agentic utility one-shots BEFORE any stable routing. Clients such as Claude Code fire
	// background requests — title generation, topic detection, quota probes — CONCURRENTLY with the real
	// turn and tagged with the SAME conversation id. Routing them to the conversation's stable session
	// collides with the in-flight real turn (the bridge serializes turns per session, so the loser of the
	// race gets a 409 that surfaces as a 500) and, even when serialized, the throwaway prompt pollutes the
	// agent history. Such a request advertises no tools and carries no assistant/tool history, so route it
	// to a fresh ephemeral session: it is collision-free and side-effect-free (nothing to continue, no
	// context to preserve), and the idle session is later evicted.
	contHint := composerContinuationHintFor(tenant, oai)
	if isComposerUtilityOneShot(oai, opts, contHint) {
		sid := mintComposerSessionID()
		composerDebugf("[composer] deriveSessionID BRANCH=ephemeral(utility one-shot) -> sessionID=%s", sid)
		if composerDebugEnabled {
			// Debug routing shape without logging prompt content. Railway commonly runs
			// with debug enabled, so even a short text prefix would be production data.
			msgs := gjson.GetBytes(oai, "messages").Array()
			userTextBytes := 0
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Get("role").String() == "user" {
					userTextBytes = len(cursorMessageText(msgs[i]))
					break
				}
			}
			composerDebugf("[composer] one-shot DIAG sid=%s bodyKeys=[%s] metaKeys=[%s] userTextBytes=%d", sid,
				composerJSONKeys(opts.OriginalRequest), composerJSONKeys([]byte(gjson.GetBytes(opts.OriginalRequest, "metadata").Raw)), userTextBytes)
		}
		return sid, nil
	}

	// Tool-result routing is bridge-owned. The session id in this body is advisory recovery context only;
	// `/agent/continue` resolves the actual paused round from the opaque signed tool-call ids.
	messages := gjson.GetBytes(oai, "messages").Array()
	currentConversationBinding := explicitBinding
	if currentConversationBinding == "" {
		if convKey := composerContentConvKey(messages); convKey != "" {
			currentConversationBinding = composerConversationBinding(tenant, apiKey, convKey)
		}
	}
	if _, _, isCont := composerToolResultsHinted(messages, contHint); isCont {
		if pid := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String()); pid != "" {
			if route := lookupComposerResponseRoute(tenant, apiKey, pid); route.sessionID != "" {
				if currentConversationBinding != "" && route.conversationBinding != currentConversationBinding {
					if route.conversationBinding != "" {
						return "", &composerConversationRouteConflictError{}
					}
					if explicitBinding != "" {
						// A legacy response route cannot prove an explicit conversation scope. Fall through
						// to the signed tool result or bounded-history recovery instead of guessing.
					} else {
						return route.sessionID, nil // legacy no-explicit-id compatibility
					}
				} else {
					return route.sessionID, nil
				}
			}
		}
		if explicitConversationID != "" {
			baseSid := composerStableBaseSessionID(tenant, apiKey, explicitConversationID)
			headDigest, openerFP := lineageForkKeysForRequest(tenant, messages, opts)
			return composerLineage.resolveStableSessionForTurn(tenant, baseSid, headDigest, openerFP, lineageTurnDiscriminator(messages)), nil
		}
		if convKey := composerContentConvKey(messages); convKey != "" {
			baseSid := composerStableBaseSessionID(tenant, apiKey, convKey)
			headDigest, openerFP := lineageForkKeysForRequest(tenant, messages, opts)
			return composerLineage.resolveStableSessionForTurn(tenant, baseSid, headDigest, openerFP, lineageTurnDiscriminator(messages)), nil
		}
		return mintComposerSessionID(), nil
	}

	// H16 (C-RESPID): a Responses/Codex follow-up that carries previous_response_id resumes the DURABLE agent
	// via its authenticated embedded route (or the legacy response-id map). This is
	// BEFORE the stable-conv hash because previous_response_id is intentionally NOT in stableConversationID's
	// list (it is not stable — it changes every turn). On an unknown legacy ID we fall
	// through to the stable-conv hash and finally to a fresh mint, so a routine follow-up never errors.
	if pid := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String()); pid != "" {
		if route := lookupComposerResponseRoute(tenant, apiKey, pid); route.sessionID != "" {
			if currentConversationBinding != "" && route.conversationBinding != currentConversationBinding {
				if route.conversationBinding != "" {
					return "", &composerConversationRouteConflictError{}
				}
				if explicitBinding != "" {
					// A pre-binding response id cannot prove an explicit scope. Never resume it directly; bounded
					// history can faithfully re-seed, while a thin request gets the normal 410 below.
				} else {
					return route.sessionID, nil // legacy no-explicit-id compatibility; no scope exists to compare
				}
			} else {
				composerDebugf("[composer] deriveSessionID BRANCH=previous_response_id(durable resume) -> sessionID=%s", route.sessionID)
				return route.sessionID, nil
			}
		}
		if !composerHasReplayableHistory(oai) {
			composerDebugf("[composer] deriveSessionID previous_response_id present but unmapped and replay history absent -> 410")
			return "", &composerContinuityGoneError{}
		}
		composerDebugf("[composer] deriveSessionID previous_response_id present but unmapped; bounded replay history is present -> faithful re-seed")
	}
	if id := explicitConversationID; id != "" {
		// ADD-62 invariant (C-ADD62-MODEL-ROTATE): the BASE session id is derived from tenant + conversation
		// (+ the ADD-79 Cursor-key fingerprint), NEVER the model. A model change on the same conversation MUST
		// keep the same external session id so the BRIDGE can detect the change (session.model != requested)
		// and rotate/re-seed the durable agent. Folding the model into this hash would fork routing and orphan
		// in-flight continuations. ADD-79: the KEY fingerprint IS folded (a key rotation should re-route).
		baseSid := composerStableBaseSessionID(tenant, apiKey, id)
		// identity-finalplan §1.3: a SUBORDINATE same-tenant splitter. baseSid stays ID-authoritative; the
		// lineage registry only splits distinct divergent contexts that share this id (subagents reusing the
		// parent metadata.user_id, parallel fan-out, branches) into per-lineage sessions and re-resolves each
		// to ONE stable sess_ across all of its own turns. The head reuses the growth-stable
		// composerHistoryFingerprint window, so a fork's recorded sid is computed once and re-resolved
		// turn-to-turn. A changed retained head isolates and faithfully re-seeds instead of guessing from opener.
		headDigest, openerFP := lineageForkKeysForRequest(tenant, messages, opts)
		sid := composerLineage.resolveStableSessionForTurn(tenant, baseSid, headDigest, openerFP, lineageTurnDiscriminator(messages))
		composerDebugf("[composer] deriveSessionID BRANCH=stable convID=%q -> sessionID=%s (baseSid=%s)", id, sid, baseSid)
		return sid, nil
	}
	// New user turn with no EXPLICIT client conversation id. CLIENT-AGNOSTIC KEYING: rather than mint a fresh
	// random session every turn — which loses durable continuity for any client that is not Claude Code
	// (opendesign, raw OpenAI/SDK, simple UIs) and degrades to lossy history re-seeding — derive the key from the
	// turn-stable conversation opener (composerContentConvKey). The SAME lineage machinery as the explicit-id
	// BRANCH=stable path then resolves ONE durable session across all of this conversation's turns and tool loops
	// (the opener is byte-identical on every replayed turn, and the head-digest splits divergent conversations
	// that happen to share an opener). composerContentConvKey returns "" only when there is no opener at all,
	// where a fresh mint remains the floor — and a continuation still re-seeds from its own replayed history, so a
	// routine turn never errors.
	if convKey := composerContentConvKey(messages); convKey != "" {
		baseSid := composerStableBaseSessionID(tenant, apiKey, convKey)
		headDigest, openerFP := lineageForkKeysForRequest(tenant, messages, opts)
		sid := composerLineage.resolveStableSessionForTurn(tenant, baseSid, headDigest, openerFP, lineageTurnDiscriminator(messages))
		composerDebugf("[composer] deriveSessionID BRANCH=content-key(client-agnostic opener) -> sessionID=%s (baseSid=%s)", sid, baseSid)
		return sid, nil
	}
	sid := mintComposerSessionID()
	composerDebugf("[composer] deriveSessionID BRANCH=mint(no opener) -> sessionID=%s", sid)
	return sid, nil
}

// deriveComposerSessionIDLive is the live-executor session resolver. It first resolves the content-pure session
// (deriveComposerSessionID), then — ONLY for a NEW-USER turn on a stable conversation id (the lone branch where
// concurrent siblings sharing one conversation id collide) — CLAIMS that session's logical-run lease, or FORKS
// onto a distinct sibling session if it is already running (ISOLATION invariant). The caller MUST keep the
// lease alive across the run (composerInflight.touch on a tool_use pause) and free it at the run's terminal end
// (composerInflight.release). The branch-4 predicate below MIRRORS deriveComposerSessionID's guards (kept in
// lockstep) so only a turn that genuinely lands on the shared stable session participates. Tool-result
// continuations bypass this fresh-turn lease entirely because the bridge routes them by signed round id.
func deriveComposerSessionIDLive(auth *cliproxyauth.Auth, apiKey string, oai []byte, opts cliproxyexecutor.Options, requestIDs ...string) (string, uint64, error) {
	sid, err := deriveComposerSessionID(auth, apiKey, oai, opts)
	if err != nil {
		return "", 0, err
	}
	tenant := composerTenant(auth, opts)
	if tenant == "" {
		return sid, 0, nil
	}
	contHint := composerContinuationHintFor(tenant, oai)
	if isComposerUtilityOneShot(oai, opts, contHint) {
		return sid, 0, nil
	}
	if _, _, isCont := composerToolResultsHinted(gjson.GetBytes(oai, "messages").Array(), contHint); isCont {
		return sid, 0, nil // /agent/continue is bridge-routed and owns no Go fresh-turn lease
	}
	requestID := ""
	if len(requestIDs) > 0 {
		requestID = strings.TrimSpace(requestIDs[0])
	}
	if pid := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String()); pid != "" {
		if lookupComposerResponseSession(tenant, apiKey, pid) != "" {
			// P0-5: a previous_response_id resume starts a NEW logical run on a durable session — it must take part
			// in the lease, not bypass it. Without a claim, two concurrent resumes of the SAME response id both land
			// on the pinned session and the bridge rejects the second concurrent turn (serial session -> 500), the
			// exact collision the lease prevents. Claim the pinned session; if it is already running (a concurrent
			// resume, or an abandoned paused run still holding the lease) fork onto a distinct sibling. The common
			// case — sequential resumes — always re-claims the pinned session and preserves its durable context;
			// only the rare concurrent-resume race degrades to a fresh (context-light) fork instead of a 500.
			messages := gjson.GetBytes(oai, "messages").Array()
			headDigest, _ := lineageForkKeysForRequest(tenant, messages, opts)
			finalSid, owner := composerAcquireOrFork(tenant, sid, sid, headDigest, requestID)
			return finalSid, owner, nil
		}
	}
	// Branch-4 new-user turn: claim its session or fork onto a free sibling. The base session id derives from
	// the EXPLICIT client conversation id when present, else the CLIENT-AGNOSTIC content key (the conversation
	// opener) — kept in LOCKSTEP with deriveComposerSessionID so the fork slots off the same lineage entry it
	// just recorded. A truly openerless no-id turn keeps the stateless-mint shortcut (unique by construction).
	messages := gjson.GetBytes(oai, "messages").Array()
	id := stableConversationID(opts)
	var baseSid string
	if id != "" {
		baseSid = composerStableBaseSessionID(tenant, apiKey, id)
	} else if convKey := composerContentConvKey(messages); convKey != "" {
		baseSid = composerStableBaseSessionID(tenant, apiKey, convKey)
	}
	if baseSid == "" {
		return sid, 0, nil // stateless mint with no opener: unique by construction, cannot collide
	}
	headDigest, _ := lineageForkKeysForRequest(tenant, messages, opts)
	finalSid, owner := composerAcquireOrFork(tenant, sid, baseSid, headDigest, requestID)
	if finalSid != sid {
		composerDebugf("[composer] deriveSessionID concurrency-fork convID=%q content-sid=%s busy -> sessionID=%s", id, sid, finalSid)
	}
	return finalSid, owner, nil
}

// mintComposerSessionID returns a fresh random session id (never derived from request content).
func mintComposerSessionID() string {
	return "sess_" + composerRandHex(16)
}

// isComposerUtilityOneShot reports whether this request is a non-agentic, single-shot completion that
// must be isolated from the conversation's stable Cursor session. It is a STRUCTURAL, schema-neutral rule —
// not keyed on client identity: a request that advertises NO tools, carries NO continuation/assistant-tool
// history, and references NO durable conversation cannot participate in an agentic thread, so it is a
// standalone text completion. This generalizes across clients: title generation, topic/"isNewTopic"
// detection, quota probes, and conversation summaries (Claude Code, OpenCode, Crush, Gemini CLI, …) are all
// tool-less calls that some clients fire CONCURRENTLY with the real turn under the same conversation id;
// routing them to the conversation's stable session would collide with (and pollute) the in-flight turn.
//
// EXCLUSIONS (a tool-less turn that is NOT a one-shot — it must route to durable state, not be isolated):
//   - a tool_results continuation (carries no advertised tools yet MUST route to its emitting session);
//   - H16 / L35: a request that carries an EXPLICIT durable-conversation reference in the BODY
//     (previous_response_id or conversation_id) — a Responses/Codex (or any) follow-up that relies on the
//     durable agent for context instead of resending tools/history. previous_response_id is then resolved via
//     the response-id map in deriveComposerSessionID; conversation_id via the stable-conv hash.
//
// L35 / concurrency NOTE (deliberate scope): the finding asks to also un-isolate a tool-less turn merely
// because it carries assistant/tool HISTORY. That signal is intentionally NOT used here: a HEADER-only
// conversation id with replayed history is exactly the shape of a CONCURRENT throwaway (title/topic/summary
// fired in parallel under the conversation's id), and routing that to the stable session reintroduces the
// per-session 409→500 collision the one-shot isolation exists to prevent (proven regression). The durable
// follow-up need that L35 targets is met by the BODY signals above (previous_response_id/conversation_id),
// which a client genuinely relying on durable state sets explicitly. History-only un-isolation is therefore
// declined as too weak to distinguish a real follow-up from a concurrent side-call — documented as a
// best-effort limitation per the audit's own "Low/debatable" classification of L35.
func isComposerUtilityOneShot(oai []byte, opts cliproxyexecutor.Options, hint composerContinuationHint) bool {
	if len(composerToolDefs(oai)) > 0 {
		return false // an agentic turn advertises tools — never isolate it
	}
	// ADD-65: use the hinted continuation check so a Responses server-side-chained [..., tool, user] turn (and a
	// signed-client-tool continuation) is recognized as a continuation — NOT isolated as a fresh one-shot, which
	// would strand its tool output.
	if _, _, ok := composerToolResultsHinted(gjson.GetBytes(oai, "messages").Array(), hint); ok {
		return false // a tool_results continuation must route to its emitting session, not a fresh one
	}
	// H16/L35: an explicit body-level durable reference means the client expects context to be preserved.
	if len(opts.OriginalRequest) > 0 {
		for _, k := range []string{"previous_response_id", "conversation_id"} {
			if v := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, k).String()); v != "" {
				return false
			}
		}
	}
	return true
}

// composerJSONKeys returns the top-level object keys of raw JSON, comma-joined (debug diagnostics only — values
// are never logged). Empty for non-objects. Surfaces whether a structured marker (e.g. a Claude Code
// workflow-keyword side-channel) reaches the proxy on an otherwise-indistinguishable 0-tool one-shot.
func composerJSONKeys(raw []byte) string {
	res := gjson.ParseBytes(raw)
	if !res.IsObject() {
		return ""
	}
	var keys []string
	res.ForEach(func(k, _ gjson.Result) bool {
		keys = append(keys, k.String())
		return true
	})
	return strings.Join(keys, ",")
}

// cursorMessageText extracts the text content of an OpenAI-shape message whose
// content may be a string or an array of content parts.
// cursorMessageText delegates to the canonical turnprovenance.MessageText (P2.6) so routing,
// history rendering, and provenance fingerprinting all see identical text for the same
// message. The canonical parser joins multiple text parts with a newline (M29: adjacent
// blocks are distinct segments — bare concat corrupts code/JSON boundaries) and whitelists
// genuine text part types, so non-standard parts cannot diverge the two subsystems.
func cursorMessageText(m gjson.Result) string {
	return turnprovenance.MessageText(m)
}

var composerSystemMaxBytes = func() int {
	const def = 512 << 10
	v := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_SYSTEM_MAX_BYTES"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 4<<10 {
		return def
	}
	return n
}()

func composerUTF8Prefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(s[:end]) {
		end--
	}
	return s[:end]
}

func composerUTF8Suffix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

// boundComposerText preserves a UTF-8-safe head and recent tail under a strict
// aggregate byte ceiling. The caller keeps the current user instruction outside
// this bounded context, so truncation can never alter the active request.
func boundComposerText(text string, maxBytes int, marker string) string {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text
	}
	framedMarker := "\n" + marker + "\n"
	if len(framedMarker) >= maxBytes {
		return composerUTF8Prefix(marker, maxBytes)
	}
	remaining := maxBytes - len(framedMarker)
	headBytes := remaining / 2
	tailBytes := remaining - headBytes
	return composerUTF8Prefix(text, headBytes) + framedMarker + composerUTF8Suffix(text, tailBytes)
}

const (
	composerSystemAggregateOmission = "[... unique system/developer instruction block(s) omitted to bound the aggregate prompt ...]"
	composerSystemBlockOmission     = "[... content omitted inside this system/developer instruction block ...]"
)

type composerSystemCandidate struct {
	role string
	text string
}

type composerSystemBlock struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

func composerSystemBlockID(role, text string) string {
	sum := sha256.Sum256([]byte(role + "\x00" + text))
	return "csb1_" + hex.EncodeToString(sum[:20])
}

func composerSystemBlocksText(blocks []composerSystemBlock) string {
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// boundComposerSystemCandidates preserves complete instruction blocks whenever
// possible. An oversized individual block is bounded internally before whole
// middle blocks are omitted, so the delivered block IDs describe exact model
// input boundaries and remain stable across ordinary transcript growth.
func boundComposerSystemCandidates(candidates []composerSystemCandidate, maxBytes int) []composerSystemBlock {
	if len(candidates) == 0 || maxBytes <= 0 {
		return nil
	}
	recordCap := maxBytes
	if len(candidates) > 1 {
		recordCap = max(256, (maxBytes-len(composerSystemAggregateOmission)-4)/2)
	}
	blocks := make([]composerSystemBlock, 0, len(candidates))
	for _, candidate := range candidates {
		text := boundComposerText(candidate.text, recordCap, composerSystemBlockOmission)
		if text == "" {
			continue
		}
		blocks = append(blocks, composerSystemBlock{
			ID:   composerSystemBlockID(candidate.role, text),
			Text: text,
		})
	}
	if len(composerSystemBlocksText(blocks)) <= maxBytes {
		return blocks
	}

	markerText := composerSystemAggregateOmission
	if len(markerText) > maxBytes {
		markerText = composerUTF8Prefix(markerText, maxBytes)
	}
	marker := composerSystemBlock{
		ID:   composerSystemBlockID("omission", markerText),
		Text: markerText,
	}
	available := maxBytes - len(markerText)
	if available <= 2 {
		return []composerSystemBlock{marker}
	}

	// Each retained block costs its text plus one two-newline separator next
	// to the marker/another retained block. Split the budget between a stable
	// opener and the newest tail; unused head budget naturally flows to tail.
	headBudget := available / 2
	headEnd, headCost := 0, 0
	for headEnd < len(blocks) {
		cost := len(blocks[headEnd].Text) + 2
		if headCost+cost > headBudget {
			break
		}
		headCost += cost
		headEnd++
	}
	tailStart, tailCost := len(blocks), 0
	for tailStart > headEnd {
		cost := len(blocks[tailStart-1].Text) + 2
		if headCost+tailCost+cost > available {
			break
		}
		tailCost += cost
		tailStart--
	}
	out := make([]composerSystemBlock, 0, headEnd+1+len(blocks)-tailStart)
	out = append(out, blocks[:headEnd]...)
	out = append(out, marker)
	out = append(out, blocks[tailStart:]...)
	return out
}

// extractComposerSystemBlocks returns normalized, exact-deduplicated context.
// Agent harnesses may persist the same hidden hook after every turn;
// retaining explicit block identity prevents that standing context from being
// appended to a warm SDK conversation again on every request.
func extractComposerSystemBlocks(messages []gjson.Result, reminderText string) []composerSystemBlock {
	candidates := make([]composerSystemCandidate, 0)
	seen := make(map[string]struct{})
	for _, m := range messages {
		r := m.Get("role").String()
		if r != "system" && r != "developer" {
			continue
		}
		t := strings.TrimSpace(strings.ReplaceAll(cursorMessageText(m), "\r\n", "\n"))
		if t == "" {
			continue
		}
		key := r + "\x00" + t
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, composerSystemCandidate{role: r, text: t})
	}
	if reminder := strings.TrimSpace(strings.ReplaceAll(reminderText, "\r\n", "\n")); reminder != "" {
		key := "reminder\x00" + reminder
		if _, duplicate := seen[key]; !duplicate {
			candidates = append(candidates, composerSystemCandidate{role: "reminder", text: reminder})
		}
	}
	blocks := boundComposerSystemCandidates(candidates, composerSystemMaxBytes)
	if blocks == nil {
		return []composerSystemBlock{}
	}
	return blocks
}

func extractComposerSystem(messages []gjson.Result) string {
	return composerSystemBlocksText(extractComposerSystemBlocks(messages, ""))
}

// composerHistoryFingerprintHeadMessages bounds how many leading non-system messages feed the fingerprint
// (H13). It is 2 — the opener PLUS the message immediately after it — which is the SWEET SPOT for the two
// competing requirements:
//   - GROWTH-STABLE: appending turns at the tail leaves the first 2 messages untouched, so the hash is
//     constant turn-to-turn for any conversation of ≥2 non-system messages (the common case, and exactly
//     when ERROR_CONVERSATION_TOO_LONG matters). Hashing more leading messages would break this for short
//     conversations (an append that still lands within a larger bound would flip the hash — a false reseed).
//   - REWRITE-SENSITIVE: a /compact typically PRESERVES the first user message and REPLACES the body with a
//     summary; that summary surfaces as the message right after the opener (message index 1), so a 2-message
//     bound flips the hash exactly when the body is compacted while the opener is preserved — the case the
//     old "first message only" anchor MISSED (H13). Best-effort: a compact that preserves BOTH the first two
//     messages and rewrites only deeper content slips through (an explicit compact-epoch signal would be
//     preferred, but no inbound schema exposes one) — documented limitation.
//
// P1-2: 2 is the default sweet spot; CURSOR_COMPOSER_FINGERPRINT_HEAD_MESSAGES widens it for deep-/compact-heavy
// workloads that must catch a compaction which preserves the first 2 messages VERBATIM while rewriting only
// DEEPER retained content (the residual edge the 2-message window misses) — at the documented cost of a false
// reseed on each of a short conversation's first (N-1) growth turns. Kept at 2 by default so growth-stability
// (a tail append never flips the hash for a >=2-message head) and its tests are preserved.
var composerHistoryFingerprintHeadMessages = func() int {
	v := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_FINGERPRINT_HEAD_MESSAGES"))
	if v == "" {
		return 2
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 2
	}
	return n
}()

// composerHistoryFingerprintPrefixRunes bounds how much of each head message's text contributes, so a huge
// pasted opener does not dominate and a later in-place EDIT of an early message still flips the hash.
const composerHistoryFingerprintPrefixRunes = 256

func composerExcludedRole(role string) bool {
	return role == "system" || role == "developer"
}

// composerAmbiguousTrailingUserSegments detects a provenance-erasing wire
// shape: two non-empty user-role records at the current-turn suffix. The proxy
// cannot know which is human intent and which is injected context after a
// client flattened both to the same role. Failing closed is safer than silently
// selecting either segment and executing tools for the wrong instruction.
func composerAmbiguousTrailingUserSegments(messages []gjson.Result) bool {
	if len(messages) < 2 {
		return false
	}
	for _, m := range messages[len(messages)-2:] {
		if m.Get("role").String() != "user" {
			return false
		}
		if strings.TrimSpace(cursorMessageText(m)) == "" && !messageHasImageParts(m) {
			return false
		}
	}
	return true
}

// composerHeadMessageText returns the bounded text prefix of a message for lineage/fingerprint hashing.
func composerHeadMessageText(m gjson.Result) string {
	text := cursorMessageText(m)
	if r := []rune(text); len(r) > composerHistoryFingerprintPrefixRunes {
		text = string(r[:composerHistoryFingerprintPrefixRunes])
	}
	return text
}

// composerHistoryFingerprint returns a 32-hex fingerprint that is GROWTH-STABLE (a normal multi-turn
// conversation only APPENDS turns at the tail, so it stays constant) BUT REWRITE-SENSITIVE: it changes when
// the client REWRITES EARLIER RETAINED history — e.g. a /compact summary supplanting the conversation body
// even when the original first user message is PRESERVED verbatim at the head (H13). That is exactly when
// the bridge must re-seed (C2/H12); the old "first non-system message only" anchor MISSED such a compact.
//
// It hashes a BOUNDED RETAINED PREFIX: the first N non-system messages' role + a short text prefix each.
// Appending turns at the TAIL adds messages PAST the head bound, so none of the hashed inputs change —
// growth-stable. A /compact that REWRITES the body (messages 2..N) flips the hash even when message 1 (the
// opener) is preserved verbatim — rewrite-sensitive, which the old "first message only" anchor MISSED.
//
// The total message COUNT is deliberately NOT hashed: a count would change on every normal tail append and
// reintroduce the very per-turn re-seed (and ERROR_CONVERSATION_TOO_LONG race) this avoids. Best-effort:
// a /compact that rewrites ONLY content BEYOND the head bound while preserving the first N messages verbatim
// can still slip through; an explicit compact-epoch signal (if any inbound schema exposed one) would be
// preferred, but none does today — flagged as best-effort. Empty when there is no non-system content.
// composerNonSystemHeadMessages returns conversation messages with system/developer roles removed,
// preserving order. Shared by lineage/fingerprint helpers that use the same head window.
func composerNonSystemHeadMessages(messages []gjson.Result) []gjson.Result {
	out := make([]gjson.Result, 0, len(messages))
	for _, m := range messages {
		if composerExcludedRole(m.Get("role").String()) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// composerWriteFingerprintHead writes the bounded retained-prefix head (role + text prefix per message)
// into w. The byte layout matches composerHistoryFingerprint and lineageHeadDigest.
func composerWriteFingerprintHead(w io.Writer, head []gjson.Result) {
	limit := len(head)
	if limit > composerHistoryFingerprintHeadMessages {
		limit = composerHistoryFingerprintHeadMessages
	}
	for i := 0; i < limit; i++ {
		m := head[i]
		_, _ = io.WriteString(w, m.Get("role").String())
		_, _ = w.Write([]byte{0x1f})
		_, _ = io.WriteString(w, composerHeadMessageText(m))
		_, _ = w.Write([]byte{0})
	}
}

func composerHistoryFingerprint(messages []gjson.Result) string {
	head := composerNonSystemHeadMessages(messages)
	if len(head) == 0 {
		return ""
	}
	h := sha256.New()
	composerWriteFingerprintHead(h, head)
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// extractComposerImages pulls image parts from a message's multimodal content. A base64 data: URI is
// emitted in the SDK inline shape {data, mimeType} (C4); an http(s) URL is emitted in the SDK URL shape
// {url[, mimeType]} (C4) so non-base64 images survive to the SDK. Degenerate entries (empty data, empty
// mime on the inline form, or empty url) are skipped so one bad attachment never fails the whole turn (EX12).
func composerImageFromPart(part gjson.Result) (map[string]any, bool) {
	// OpenAI image_url accepts both object and scalar forms.
	iu := part.Get("image_url")
	u := iu.Get("url").String()
	if u == "" && iu.Type == gjson.String {
		u = iu.String()
	}
	if u == "" {
		u = part.Get("url").String()
	}
	if strings.HasPrefix(u, "data:") {
		idx := strings.Index(u, ",")
		if idx <= 5 {
			return nil, false
		}
		meta, data := u[5:idx], u[idx+1:]
		mime := meta
		if semi := strings.Index(meta, ";"); semi >= 0 {
			mime = meta[:semi]
		}
		if strings.TrimSpace(data) == "" || strings.TrimSpace(mime) == "" {
			return nil, false
		}
		return map[string]any{"data": data, "mimeType": mime}, true
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		img := map[string]any{"url": u}
		if mime := imageMimeFromURL(u); mime != "" {
			img["mimeType"] = mime
		}
		if detail := part.Get("image_url.detail").String(); detail != "" {
			img["detail"] = detail
		} else if detail := part.Get("detail").String(); detail != "" {
			img["detail"] = detail
		}
		return img, true
	}
	// Anthropic base64 image source and the bridge's internal image envelope.
	data := part.Get("source.data").String()
	if data == "" {
		data = part.Get("data").String()
	}
	mime := part.Get("source.media_type").String()
	if mime == "" {
		mime = part.Get("source.mimeType").String()
	}
	if mime == "" {
		mime = part.Get("mimeType").String()
	}
	if data != "" && mime != "" {
		return map[string]any{"data": data, "mimeType": mime}, true
	}
	return nil, false
}

func extractComposerImages(m gjson.Result) []map[string]any {
	c := m.Get("content")
	if !c.IsArray() {
		return nil
	}
	var out []map[string]any
	for _, part := range c.Array() {
		if image, ok := composerImageFromPart(part); ok {
			out = append(out, image)
		}
	}
	return out
}

func decodeComposerJSON(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func composerJSONUnsafeNumber(value any) bool {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil || math.IsInf(parsed, 0) || math.IsNaN(parsed) {
			return true
		}
		return math.Trunc(parsed) == parsed && math.Abs(parsed) > 9007199254740991
	case []any:
		for _, child := range typed {
			if composerJSONUnsafeNumber(child) {
				return true
			}
		}
	case map[string]any:
		for _, child := range typed {
			if composerJSONUnsafeNumber(child) {
				return true
			}
		}
	}
	return false
}

func composerJSONComparable(value any) any {
	switch typed := value.(type) {
	case json.Number:
		parsed, _ := typed.Float64()
		return parsed
	case []any:
		out := make([]any, len(typed))
		for index, child := range typed {
			out[index] = composerJSONComparable(child)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			out[key] = composerJSONComparable(child)
		}
		return out
	default:
		return value
	}
}

func composerJSONEquivalent(left, right any) bool {
	return reflect.DeepEqual(composerJSONComparable(left), composerJSONComparable(right))
}

func composerToolResultHasMalformedImage(m gjson.Result) bool {
	content := m.Get("content")
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		kind := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
		looksLikeImage := kind == "image" || kind == "image_url"
		if !looksLikeImage {
			looksLikeImage = part.Get("image_url").Exists() || part.Get("source").Exists()
		}
		if looksLikeImage {
			if _, ok := composerImageFromPart(part); !ok {
				return true
			}
		}
	}
	return false
}

func composerToolResultBlocks(m gjson.Result) ([]map[string]any, string) {
	content := m.Get("content")
	if content.Type == gjson.String {
		return []map[string]any{{"type": "text", "text": content.String()}}, ""
	}
	if !content.IsArray() {
		if !content.Exists() {
			return []map[string]any{{"type": "text", "text": ""}}, ""
		}
		if value, err := decodeComposerJSON(content.Raw); err == nil {
			if composerJSONUnsafeNumber(value) {
				return nil, "typed tool result contains an integer that cannot be represented safely by the SDK runtime"
			}
			return []map[string]any{{"type": "json", "value": value}}, ""
		}
		return []map[string]any{{"type": "text", "text": content.String()}}, ""
	}
	blocks := make([]map[string]any, 0, len(content.Array()))
	for _, part := range content.Array() {
		if text := part.Get("text"); text.Exists() {
			blocks = append(blocks, map[string]any{"type": "text", "text": text.String()})
			continue
		}
		if image, ok := composerImageFromPart(part); ok {
			blocks = append(blocks, map[string]any{"type": "image", "image": image})
			continue
		}
		if part.Raw != "" {
			if value, err := decodeComposerJSON(part.Raw); err == nil {
				if composerJSONUnsafeNumber(value) {
					return nil, "typed tool result contains an integer that cannot be represented safely by the SDK runtime"
				}
				blocks = append(blocks, map[string]any{"type": "json", "value": value})
			}
		}
	}
	return blocks, ""
}

func composerToolResultText(blocks []map[string]any) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block["type"] == "text" {
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// imageMimeFromURL derives a best-effort image MIME type from a URL's trailing file extension, or "" when
// the extension is absent/unknown. Query strings and fragments are stripped before inspecting the path.
func imageMimeFromURL(u string) string {
	path := u
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	dot := strings.LastIndex(path, ".")
	if dot < 0 {
		return ""
	}
	switch strings.ToLower(path[dot+1:]) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	}
	return ""
}

// composerImagePlaceholder returns a short "[image xN: <mime>]" placeholder for a message whose content
// carries image parts (EX15), or "" when there are none. It keeps positional context for the model on a
// re-seed without embedding the (potentially huge) base64 payload in the rendered transcript.
func composerImagePlaceholder(m gjson.Result) string {
	imgs := extractComposerImages(m)
	if len(imgs) == 0 {
		return ""
	}
	mime, _ := imgs[0]["mimeType"].(string)
	if mime == "" {
		mime = "image"
	}
	return fmt.Sprintf("[image x%d: %s]", len(imgs), mime)
}

// composerImageInvalidPlaceholder is the model-visible note inserted when a message carried image part(s) but
// all of them were degenerate/unsupported and discarded (ADD-56). It tells the model an attachment existed
// and was invalid, rather than silently pretending the turn had no image (which would mislead the model).
const composerImageInvalidPlaceholder = "[an image attachment was provided but could not be processed (invalid or unsupported); it was not included]"

// messageHasImageParts reports whether a message's content array carries ANY image part — by content-part
// type ("image"/"input_image"/"image_url") or by a recognized image field (image_url / source). It detects
// the ORIGINAL intent regardless of whether the part is well-formed, so ADD-56 can tell an image-only turn
// whose single image was malformed apart from a genuinely text-only turn.
func messageHasImageParts(m gjson.Result) bool {
	c := m.Get("content")
	if !c.IsArray() {
		return false
	}
	for _, part := range c.Array() {
		switch strings.ToLower(part.Get("type").String()) {
		case "image", "input_image", "image_url":
			return true
		}
		if part.Get("image_url").Exists() || part.Get("source").Exists() || part.Get("file_id").Exists() {
			return true
		}
	}
	return false
}

// errComposerImageOnlyInvalid is returned when the request's final user turn is IMAGE-ONLY and every image
// part is malformed/unsupported (ADD-56). Such a turn would otherwise become an empty text turn the model
// answers with irrelevant output. Surfacing a typed validation error is the honest degrade — never a silent
// empty turn, never a fake answer.
var errComposerImageOnlyInvalid = &composerInvalidRequestError{
	msg: "cursor composer: the request's image-only turn has no valid image (all attachments were malformed or unsupported) and no text",
}

// composerInvalidRequestError is a typed 400-class executor error for a client-side malformed request, so the
// handler returns a 4xx (not a generic 500) and the conductor does not retry it as a transient upstream fault.
type composerInvalidRequestError struct{ msg string }

func (e *composerInvalidRequestError) Error() string   { return e.msg }
func (e *composerInvalidRequestError) StatusCode() int { return http.StatusBadRequest }
func (e *composerInvalidRequestError) RetryScope() cliproxyexecutor.RetryScope {
	return cliproxyexecutor.RetryScopeSelectedExecution
}
func (e *composerInvalidRequestError) AuthAttributable() bool { return false }

// composerContinuityGoneError refuses a state-dependent follow-up whose
// legacy previous_response_id cannot be resolved and whose request carries no
// replayable prior transcript. Starting a blank agent would return a plausible
// answer from the wrong context—the incident class this path must prevent.
type composerContinuityGoneError struct{}

func (e *composerContinuityGoneError) Error() string {
	return "cursor composer: prior response continuity is unavailable; resend bounded conversation history or start a new conversation"
}
func (e *composerContinuityGoneError) StatusCode() int { return http.StatusGone }

type composerConversationRouteConflictError struct{}

func (e *composerConversationRouteConflictError) Error() string {
	return "cursor composer: previous response belongs to a different or unverifiable conversation; do not reuse it after starting a new conversation"
}

func (e *composerConversationRouteConflictError) StatusCode() int { return http.StatusConflict }

// composerForceStoreTrue coerces the (translated) request's `store` field to true. Cursor Composer is an
// inherently DURABLE agent — it persists/resumes state via resumeAgent on a stable session id, so there is no
// ephemeral no-store mode. Rather than 400-reject a client that defaults to store:false (e.g. the
// pi/openai-completions client), OVERRIDE store to true so the request proceeds on the durable path it would
// take anyway. (Supersedes the ADD-94 reject: a client's store:false DEFAULT must not block composer. The flag
// is moot for the bridge body, which never carries it; normalizing it just keeps the request internally
// consistent.) store:true / absent are left as-is (already durable).
func composerForceStoreTrue(oai []byte) []byte {
	if v := gjson.GetBytes(oai, "store"); v.Exists() && v.Bool() {
		return oai
	}
	out, err := sjson.SetBytes(oai, "store", true)
	if err != nil {
		return oai // malformed body — proceed; store is moot for the durable bridge path
	}
	return out
}

// lastUserTurnImageOnlyInvalid reports whether the request's LAST user message had image part(s), produced no
// valid extracted image, and carries no text (ADD-56). Continuations are out of scope here (a tool_results
// turn is classified/handled elsewhere and must never be rejected for a degenerate image) — this guards only
// the new-user turn path.
//
// Comment 5: it takes the SAME composerContinuationHint that deriveComposerSessionID / composerInputHinted use
// and classifies via composerToolResultsHinted (NOT the hintless composerToolResults). Otherwise a branch-(c)
// continuation — a Responses previous_response_id-chained [..., tool, user] turn, or a signed-client-tool
// continuation — whose trailing user image is malformed would be REJECTED here as an invalid image-only turn
// BEFORE the hinted classifier ever runs, stranding the tool output behind a 400.
func lastUserTurnImageOnlyInvalid(messages []gjson.Result, hint composerContinuationHint) bool {
	// Never reject a continuation: its tool output is the load-bearing content, not the (possibly empty) text.
	if _, _, ok := composerToolResultsHinted(messages, hint); ok {
		return false
	}
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Get("role").String() == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return false
	}
	m := messages[lastUserIdx]
	if !messageHasImageParts(m) {
		return false
	}
	if len(extractComposerImages(m)) > 0 {
		return false // at least one image survived — not degenerate
	}
	return strings.TrimSpace(cursorMessageText(m)) == "" // had images, none valid, and no text
}

// renderComposerHistory renders prior conversation turns (everything before the last user message,
// excluding system) as a transcript, used to seed the SDK session on its first turn so a
// mid-conversation first contact (e.g. after a bridge restart) does not lose earlier context.
// composerHistoryMaxBytes bounds the AGGREGATE rendered size of the replayed transcript renderComposerHistory
// produces for a re-seed (P0-3). Per-message truncation alone does NOT bound a long conversation: a 60-turn
// session of bounded-but-nonzero results still renders a multi-hundred-KB→MB transcript (the observed ~1M-token
// runaway). Past the cap, renderComposerHistory keeps the head (opener/early context) + the recent tail and drops
// the middle with an explicit marker, so a re-seed prompt stays bounded regardless of turn count. Generous
// default; override with CURSOR_COMPOSER_HISTORY_MAX_BYTES (a 4 KiB floor keeps it from being set uselessly small).
var composerHistoryMaxBytes = func() int {
	const def = 512 << 10
	v := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_HISTORY_MAX_BYTES"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 4<<10 {
		return def
	}
	return n
}()

// boundComposerHistoryLines preserves complete rendered records. Oversized
// individual records are bounded internally first; only then are whole middle
// records omitted. This prevents a crash reseed from beginning inside a
// TOOL[id], JSON argument, or assistant record while retaining opener + tail.
func boundComposerHistoryLines(lines []string, maxBytes int) string {
	if len(lines) == 0 {
		return ""
	}
	if maxBytes <= 0 {
		return strings.Join(lines, "\n")
	}
	marker := "[... earlier complete conversation record(s) omitted to bound the replayed history ...]"
	recordCap := maxBytes
	if len(lines) > 1 {
		recordCap = max(256, (maxBytes-len(marker)-2)/2)
	}
	records := make([]string, len(lines))
	for i, line := range lines {
		records[i] = boundComposerText(line, recordCap, "[... content omitted inside this conversation record ...]")
	}
	joined := strings.Join(records, "\n")
	if len(joined) <= maxBytes {
		return joined
	}
	available := maxBytes - len(marker) - 2
	if available <= 0 {
		return composerUTF8Prefix(marker, maxBytes)
	}
	headBudget := available / 2
	headEnd, headBytes := 0, 0
	for headEnd < len(records) {
		cost := len(records[headEnd])
		if headEnd > 0 {
			cost++
		}
		if headBytes+cost > headBudget && headEnd > 0 {
			break
		}
		headBytes += cost
		headEnd++
	}
	tailStart, tailBytes := len(records), 0
	tailBudget := available - headBytes
	for tailStart > headEnd {
		cost := len(records[tailStart-1])
		if tailStart < len(records) {
			cost++
		}
		if tailBytes+cost > tailBudget && tailStart < len(records) {
			break
		}
		tailBytes += cost
		tailStart--
	}
	out := make([]string, 0, headEnd+1+len(records)-tailStart)
	out = append(out, records[:headEnd]...)
	out = append(out, marker)
	out = append(out, records[tailStart:]...)
	return strings.Join(out, "\n")
}

func renderComposerHistory(messages []gjson.Result, lastUserIdx int) string {
	if lastUserIdx <= 0 {
		return ""
	}
	var lines []string
	appendLine := func(s string) {
		if s == "" {
			return
		}
		lines = append(lines, s)
	}
	for i := 0; i < lastUserIdx; i++ {
		m := messages[i]
		r := m.Get("role").String()
		switch r {
		case "system", "developer":
			continue
		case "assistant":
			// Preserve tool-call INTENT (name + args), not just text, so a re-seeded stateless session knows
			// what it called ("edit the file you just read"). A bare assistant tool_calls turn has empty text.
			txt := cursorMessageText(m)
			var calls []string
			for _, tc := range m.Get("tool_calls").Array() {
				name := tc.Get("function.name").String()
				if name == "" {
					continue
				}
				call := name
				if args := tc.Get("function.arguments").String(); args != "" {
					// M31: bound replayed tool-call ARGUMENTS with the same truncation as tool RESULTS. A prior
					// assistant call can carry huge arguments (a large patch, base64 payload, embedded file
					// content); replaying them verbatim into the re-seed prompt can blow Cursor's per-message
					// limit — the very ERROR_CONVERSATION_TOO_LONG the history truncation exists to prevent.
					call += "(" + truncateCursorToolResultForHistory(args) + ")"
				}
				if id := tc.Get("id").String(); id != "" {
					call = id + ":" + call
				}
				calls = append(calls, call)
			}
			// ADD-67 (reverses EX9): by default DO NOT replay raw prior reasoning as ordinary prompt text. A
			// present reasoning_content becomes a neutral "[assistant reasoning omitted]" marker so positional
			// context survives without leaking hidden chain-of-thought into Cursor. The EX9 verbatim behavior is
			// restorable behind CURSOR_COMPOSER_REPLAY_REASONING=1.
			hadReasoning := strings.TrimSpace(m.Get("reasoning_content").String()) != ""
			reasoning := ""
			if hadReasoning && composerReplayReasoningEnabled {
				// Compat: bounded so a long chain of thought cannot blow the per-message limit.
				reasoning = truncateCursorToolResultForHistory(m.Get("reasoning_content").String())
			}
			if txt == "" && !hadReasoning && len(calls) == 0 {
				continue
			}
			line := "ASSISTANT:"
			if txt != "" {
				line += " " + txt
			}
			if reasoning != "" {
				line += " [thinking: " + reasoning + "]"
			} else if hadReasoning {
				line += " [assistant reasoning omitted]"
			}
			if len(calls) > 0 {
				line += " [tool_calls: " + strings.Join(calls, "; ") + "]"
			}
			appendLine(line)
		case "tool":
			// Tag tool results with their tool_call_id so they associate with the assistant call above.
			// EX13: bound replayed tool output so a re-seed cannot push the single accepted bubble past
			// Cursor's per-message limit (ERROR_CONVERSATION_TOO_LONG).
			label := "TOOL"
			if id := m.Get("tool_call_id").String(); id != "" {
				label = "TOOL[" + id + "]"
			}
			appendLine(label + ": " + truncateCursorToolResultForHistory(cursorMessageText(m)))
		default:
			if t := cursorMessageText(m); t != "" {
				appendLine(strings.ToUpper(r) + ": " + t)
			} else if ph := composerImagePlaceholder(m); ph != "" {
				// EX15: keep positional context for an image-only turn instead of silently dropping it.
				appendLine(strings.ToUpper(r) + ": " + ph)
			}
		}
	}
	return boundComposerHistoryLines(lines, composerHistoryMaxBytes)
}

// extractComposerToolChoice returns the normalized tool_choice ("auto"/"none"/"required"/"specific:<name>").
func extractComposerToolChoice(oai []byte) string {
	tc := gjson.GetBytes(oai, "tool_choice")
	if !tc.Exists() {
		// Legacy OpenAI function_call: "none"/"auto" or {"name":"X"}.
		if fc := gjson.GetBytes(oai, "function_call"); fc.Exists() {
			if fc.Type == gjson.String {
				return fc.String()
			}
			if name := fc.Get("name").String(); name != "" {
				return "specific:" + name
			}
		}
		return ""
	}
	if tc.Type == gjson.String {
		return tc.String()
	}
	if name := tc.Get("function.name").String(); name != "" {
		return "specific:" + name
	}
	return ""
}

// resolveComposerToolChoice extracts tool_choice and, for specific:<name>, resolves the name through the
// client's tools + caller aliases — so a forced choice expressed via a generic/aliased name (e.g.
// tool_choice {function:{name:"shell"}} with alias shell=RunCommand) instructs the bridge to require the
// client's ACTUAL tool name. Falls through to the raw name when nothing matches (parity with emitted calls).
func resolveComposerToolChoice(oai []byte, defs []cursorToolDefinition, overrides map[string]string) string {
	tc := extractComposerToolChoice(oai)
	if name, ok := strings.CutPrefix(tc, "specific:"); ok {
		if spec := resolveToolSpec(name, defs, overrides); spec != nil {
			return "specific:" + spec.Name
		}
	}
	return tc
}

// extractComposerResponseFormat returns the OpenAI response_format object (or nil), e.g.
// {"type":"json_object"} or {"type":"json_schema","json_schema":{...}}. The bridge turns it into a
// model instruction (the SDK has no first-class response-format param).
func extractComposerResponseFormat(oai []byte) map[string]any {
	rf := gjson.GetBytes(oai, "response_format")
	if !rf.Exists() || !rf.IsObject() {
		return nil
	}
	var out map[string]any
	decoded, err := decodeComposerJSON(rf.Raw)
	if err != nil {
		return nil
	}
	var ok bool
	out, ok = decoded.(map[string]any)
	if !ok {
		return nil
	}
	return out
}

// extractComposerStop returns the stop sequences (accepts the string or array form), or nil.
func extractComposerStop(oai []byte) []string {
	s := gjson.GetBytes(oai, "stop")
	if !s.Exists() {
		return nil
	}
	if s.Type == gjson.String {
		if v := s.String(); v != "" {
			return []string{v}
		}
		return nil
	}
	if s.IsArray() {
		var out []string
		for _, e := range s.Array() {
			if v := e.String(); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	return nil
}

// extractComposerMaxTokens returns the output token limit (max_tokens or max_completion_tokens), or 0.
func extractComposerMaxTokens(oai []byte) int {
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if v := gjson.GetBytes(oai, k); v.Exists() && v.Int() > 0 {
			return int(v.Int())
		}
	}
	return 0
}

// composerConstraints collects the response constraints as explicit bridge fields. tool_choice is carried
// separately (it also gates tool advertisement).
//
// H20/H21 (owner caveat): the composer path CANNOT truly enforce stop sequences, a hard max_tokens cap,
// response_format, or parallel_tool_calls:false SERVER-SIDE — Cursor generates the tokens and the bridge
// only relays them; there is no API seam to clip output, cap tokens, validate a schema, or limit parallel
// emission. So these are carried as a best-effort PROMPT INSTRUCTION (the bridge renders them into model
// guidance) and, when the client requested a HARD guarantee we cannot honor, we ALSO surface an explicit
// `unsupported` signal so the client/model is told the guarantee is not enforced — we never PRETEND hard
// enforcement (e.g. we never clip at a stop sequence and report finish=length as if the API enforced it).
func composerConstraints(oai []byte) map[string]any {
	c := map[string]any{}
	var unsupported []string
	if rf := extractComposerResponseFormat(oai); rf != nil {
		c["responseFormat"] = rf
		// A strict json_schema is a hard parser guarantee we cannot validate/enforce here.
		if t := strings.ToLower(gjson.GetBytes(oai, "response_format.type").String()); t == "json_schema" {
			unsupported = append(unsupported, "response_format=json_schema (best-effort prompt only; output is not schema-validated server-side)")
		}
	}
	if stop := extractComposerStop(oai); len(stop) > 0 {
		c["stop"] = stop
		unsupported = append(unsupported, "stop sequences (best-effort prompt only; output is not clipped server-side)")
	}
	if mt := extractComposerMaxTokens(oai); mt > 0 {
		c["maxTokens"] = mt
		unsupported = append(unsupported, "max_tokens hard cap (best-effort prompt only; output length is not capped server-side)")
	}
	// ADD-83/87: the Claude/Gemini/Responses request translators normalize a requested thinking budget /
	// reasoning.effort into reasoning_effort, but the composer path cannot honor it: the @cursor/sdk agent.send
	// takes no reasoning-effort knob. The signal is surfaced SOLELY via unsupportedHardGuarantees (rendered by
	// the bridge's constraintInstructions) — NOT as a dedicated body field. The bridge's constraints allowlist
	// (cursor-agent-bridge.mjs handleTurn) does not read a reasoningEffort key, so carrying one would be a dead
	// wire field; this matches the honest-degrade contract (best-effort advisory only, never an enforced control).
	if re := strings.TrimSpace(gjson.GetBytes(oai, "reasoning_effort").String()); re != "" {
		unsupported = append(unsupported, fmt.Sprintf("reasoning_effort=%s best-effort; Cursor composer does not accept a reasoning-effort param", re))
	}
	// H20: parallel_tool_calls:false is a documented zero-or-one-tool-per-turn limit. We cannot hard-cap
	// Cursor's emission and the bridge does not read a parallelToolCalls key, so the signal is surfaced SOLELY
	// via unsupportedHardGuarantees (the bridge renders it as best-effort advisory text) — NOT as a dedicated
	// body field that would fall on the floor. Only the explicit `false` is load-bearing; absent / true needs no signal.
	if v := gjson.GetBytes(oai, "parallel_tool_calls"); v.Exists() && !v.Bool() {
		unsupported = append(unsupported, "parallel_tool_calls=false (best-effort prompt only; parallel tool emission is not hard-capped server-side)")
	}
	// ADD-72 (owner caveat): the composer path has NO surface to pass sampling/candidate controls to Cursor —
	// agent.send takes only {model, apiKey, cwd}. So when a client sends temperature/top_p/n (Gemini
	// candidateCount is normalized to `n` by the Gemini request translator), we do NOT fake enforcement and do
	// NOT carry them as a constraint the bridge would silently ignore; we surface each as an explicit
	// unsupportedHardGuarantee so the model/client is told the request is not honored (H20/H21 style). This is
	// the honest degrade — never pretend deterministic sampling or multiple candidates were applied.
	if v := gjson.GetBytes(oai, "temperature"); v.Exists() {
		unsupported = append(unsupported, fmt.Sprintf("temperature=%s (not honored; Cursor composer does not accept sampling controls)", strings.TrimSpace(v.Raw)))
	}
	if v := gjson.GetBytes(oai, "top_p"); v.Exists() {
		unsupported = append(unsupported, fmt.Sprintf("top_p=%s (not honored; Cursor composer does not accept sampling controls)", strings.TrimSpace(v.Raw)))
	}
	// n / candidateCount: the response is ALWAYS a single candidate. n>1 is the most dangerous (a client may
	// expect multiple choices), so call it out explicitly while still returning one candidate honestly.
	if v := gjson.GetBytes(oai, "n"); v.Exists() && v.Int() > 1 {
		unsupported = append(unsupported, fmt.Sprintf("n=%d / candidate_count>1 (not honored; Cursor composer returns exactly one candidate)", v.Int()))
	}
	// H22: OpenAI Responses built-in tools (web_search, file_search, computer_use, ...) arrive as tools[]
	// entries WITHOUT a `function` block. composerFunctionDefs advertises only function tools, so a built-in
	// would otherwise be SILENTLY dropped on the composer path (Cursor has no equivalent to run it) — the
	// translator-side fix (H22) merely moved that drop here. Surface each built-in as an explicit unsupported
	// guarantee so the model/client is told it cannot be honored, never silently omitted. A built-in that is
	// the target of a forced/allowed tool_choice is additionally caught as forced-unavailable downstream
	// (it is never in the advertised function set, so effectiveAdvertise misses it).
	for _, t := range gjson.GetBytes(oai, "tools").Array() {
		if fn := t.Get("function"); fn.Exists() {
			// ADD-99: a strict:true function declares a HARD Structured-Outputs argument contract we cannot
			// enforce — Cursor may emit extra/missing/loose keys and the normalizer reshapes them silently. Flag
			// it so the model/client is told strict args are not hard-validated on this path. The strict hint is
			// still carried on the advertise entry (composerAdvertise) for the bridge to honor if it can.
			if s := fn.Get("strict"); s.Exists() && s.Bool() {
				name := fn.Get("name").String()
				unsupported = append(unsupported, fmt.Sprintf("function %s strict args not hard-validated (Cursor composer does not enforce strict tool-argument schemas server-side)", name))
			}
			continue
		}
		kind := t.Get("type").String()
		if kind == "" || kind == "function" {
			continue // a bare/malformed function entry, not a built-in tool we should flag
		}
		unsupported = append(unsupported, "built-in tool "+kind+" (Cursor composer has no equivalent; it is not advertised or executed)")
	}
	// ADD-99: the deprecated functions[] form also carries strict; flag those too (composerAdvertise covers the
	// advertise side via composerFunctionDefs, which already merges functions[]).
	if !gjson.GetBytes(oai, "tools").Exists() {
		for _, fn := range gjson.GetBytes(oai, "functions").Array() {
			if s := fn.Get("strict"); s.Exists() && s.Bool() {
				name := fn.Get("name").String()
				unsupported = append(unsupported, fmt.Sprintf("function %s strict args not hard-validated (Cursor composer does not enforce strict tool-argument schemas server-side)", name))
			}
		}
	}
	if len(unsupported) > 0 {
		c["unsupportedHardGuarantees"] = unsupported
	}
	return c
}

// appendUnsupportedGuarantee appends a human-readable "unsupported hard guarantee" note to the constraints'
// unsupportedHardGuarantees list (creating it if absent). The bridge renders these as a model-visible note
// so a client/model that asked for a guarantee the composer path cannot enforce server-side is told so —
// never silently pretended-enforced. Used for the allowed_tools empty-intersection case (H07/H22).
func appendUnsupportedGuarantee(c map[string]any, note string) map[string]any {
	if c == nil {
		c = map[string]any{}
	}
	existing, _ := c["unsupportedHardGuarantees"].([]string)
	c["unsupportedHardGuarantees"] = append(existing, note)
	return c
}

// validatedComposerClientEnv keeps workspace identity advisory. Missing or malformed path headers become an
// explicit neutral context; they are never replaced with a proxy-side cwd and never reject an otherwise valid
// harness turn. Tools still execute on the harness, which interprets its own relative paths.
func validatedComposerClientEnv(opts cliproxyexecutor.Options) map[string]any {
	env, err := helps.ParseComposerWorkspace(opts.Headers)
	if err != nil {
		log.Warnf("cursor composer: ignoring invalid workspace headers and using neutral client context: %v", err)
		env = &helps.ComposerClientEnv{}
	}
	out := map[string]any{}
	if env.Cwd == "" && env.Workspace == "" {
		out["workspaceUnknown"] = true
	} else {
		out["workspacePaths"] = []string{env.Workspace}
		out["processWorkingDirectory"] = env.Cwd
	}
	if env.Shell != "" {
		out["shell"] = env.Shell
	}
	if env.OsVersion != "" {
		out["osVersion"] = env.OsVersion
	}
	return out
}

// composerContinuationHint carries the out-of-band signals that let composerToolResults recognize a
// Responses/Codex SERVER-SIDE-CHAINED continuation (ADD-65 / C-ADD65-RESPID-CONT): a turn that sends only the
// current function_call_output (+ optional new user text) WITHOUT resending the prior assistant function_call,
// relying on previous_response_id for state. Such a turn's OpenAI-normalized messages are [..., role:tool,
// role:user] with NO assistant tool_calls adjacency, which neither branch (a) nor (b) matches. The hint is
// EMPTY for callers that only have the message list (back-compat via composerToolResults).
type composerContinuationHint struct {
	// hasPreviousResponseID is true when the request body carries previous_response_id (server-side chaining).
	hasPreviousResponseID bool
	// hasClientToolID recognizes this bridge's opaque signed wire-id shape without decoding or owning it.
	hasClientToolID func(id string) bool
}

// composerLiveToolResultMaxBytes bounds a single LIVE tool-result body forwarded on a tool_results
// continuation (ADD-95). It is distinct from the HISTORY bound (cursorMaxHistoryToolResultRunes, which bounds
// REPLAYED prior output): a live result is the load-bearing answer the model is waiting on, so it gets a
// larger byte cap (256 KiB) — but it must still be bounded, or a multi-hundred-MB `cat`/`grep`/test-log result
// flows straight into Cursor and trips ERROR_CONVERSATION_TOO_LONG / memory spikes with no graceful marker.
// This is a BOUNDED byte limit on already-buffered content, not a wall-clock timeout (AGENTS.md compliant).
const composerLiveToolResultMaxBytes = 256 << 10

// truncateCursorToolResultLive bounds a LIVE tool-result body to composerLiveToolResultMaxBytes (ADD-95). When
// over cap it keeps the first N bytes (on a UTF-8 boundary so the JSON stays valid) and appends the EXACT
// marker "\n[tool result truncated by proxy: kept N/M bytes]" — the pinned 'truncated by proxy' substring both
// the executor (authoritative) and the bridge backstop agree on. Returns the input unchanged when within cap.
func truncateCursorToolResultLive(text string) string {
	if len(text) <= composerLiveToolResultMaxBytes {
		return text
	}
	total := len(text)
	cut := composerLiveToolResultMaxBytes
	// Back off to a UTF-8 rune boundary so we never split a multi-byte character (which would corrupt the JSON).
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("\n[tool result truncated by proxy: kept %d/%d bytes]", cut, total)
}

// composerToolResultEntry builds one bridge wire tool-result entry from a role:tool message (ADD-95/EX3/C5).
func composerStrictBooleanField(m gjson.Result, key string) (present bool, value bool, valid bool) {
	field := m.Get(key)
	if !field.Exists() {
		return false, false, true
	}
	switch strings.TrimSpace(field.Raw) {
	case "true":
		return true, true, true
	case "false":
		return true, false, true
	default:
		return true, false, false
	}
}

func composerToolResultErrorFlag(m gjson.Result) (bool, string) {
	snakePresent, snakeValue, snakeValid := composerStrictBooleanField(m, "is_error")
	camelPresent, camelValue, camelValid := composerStrictBooleanField(m, "isError")
	if !snakeValid || !camelValid {
		return true, "tool result error flags must be JSON booleans"
	}
	if snakePresent && camelPresent && snakeValue != camelValue {
		return true, "tool result is_error and isError flags conflict"
	}
	if snakePresent {
		return snakeValue, ""
	}
	if camelPresent {
		return camelValue, ""
	}
	return false, ""
}

func composerToolResultStructuredContent(m gjson.Result) (present bool, value any, contractError string) {
	snake := m.Get("structured_content")
	camel := m.Get("structuredContent")
	snakePresent := snake.Exists()
	camelPresent := camel.Exists()
	if !snakePresent && !camelPresent {
		return false, nil, ""
	}
	decode := func(field gjson.Result) (any, string) {
		if len(field.Raw) > composerLiveToolResultMaxBytes {
			return nil, "structured tool result exceeds the proxy byte limit and was not applied"
		}
		decoded, err := decodeComposerJSON(field.Raw)
		if err != nil {
			return nil, "structured tool result is not valid JSON"
		}
		if composerJSONUnsafeNumber(decoded) {
			return nil, "structured tool result contains an integer that cannot be represented safely by the SDK runtime"
		}
		return decoded, ""
	}
	if snakePresent {
		var errText string
		value, errText = decode(snake)
		if errText != "" {
			return true, nil, errText
		}
	}
	if camelPresent {
		camelValue, errText := decode(camel)
		if errText != "" {
			return true, nil, errText
		}
		if snakePresent {
			if !composerJSONEquivalent(value, camelValue) {
				return true, nil, "tool result structured_content and structuredContent fields conflict"
			}
		} else {
			value = camelValue
		}
	}
	return true, value, ""
}

func composerToolResultEntry(m gjson.Result) map[string]any {
	// ADD-95: cap the LIVE tool-result content (authoritative executor-side bound; the bridge has a slightly
	// higher backstop cap so this one normally wins). A huge live result would otherwise flow unbounded into
	// Cursor and trip ERROR_CONVERSATION_TOO_LONG with no graceful marker.
	blocks, representationError := composerToolResultBlocks(m)
	content := composerToolResultText(blocks)
	truncated := truncateCursorToolResultLive(content)
	if truncated != content {
		// Keep the byte cap authoritative without dropping non-text payloads.
		// contentBlocks is authoritative in Node, so relying on the compatibility
		// `images` projection here silently discarded images after truncation.
		truncatedBlocks := []map[string]any{{"type": "text", "text": truncated}}
		for _, block := range blocks {
			if block["type"] == "image" {
				truncatedBlocks = append(truncatedBlocks, block)
			}
		}
		blocks = truncatedBlocks
	}
	r := map[string]any{
		"toolCallId":    m.Get("tool_call_id").String(),
		"content":       truncated,
		"contentBlocks": blocks,
	}
	isError, contractError := composerToolResultErrorFlag(m)
	if contractError == "" && composerToolResultHasMalformedImage(m) {
		contractError = "tool result contains a malformed image block"
	}
	if isError {
		r["isError"] = true
	}
	if contractError != "" {
		r["isError"] = true
		r["contractError"] = contractError
	}
	// EX3: an image embedded inside a tool_result must not be dropped.
	if imgs := extractComposerImages(m); len(imgs) > 0 {
		r["images"] = imgs
	}
	// Preserve explicitly typed structured results. Do not promote JSON-looking
	// text: only these dedicated fields participate, and alias conflicts fail.
	structuredPresent, structuredValue, structuredError := composerToolResultStructuredContent(m)
	if structuredError != "" && (strings.Contains(structuredError, "exceeds the proxy byte limit") ||
		strings.Contains(structuredError, "cannot be represented safely")) {
		representationError = structuredError
	} else if structuredError != "" {
		r["isError"] = true
		r["contractError"] = structuredError
		r["content"] = structuredError
		r["contentBlocks"] = []map[string]any{{"type": "text", "text": structuredError}}
		r["structuredContentPresent"] = false
	} else if structuredPresent {
		r["structuredContent"] = structuredValue
		r["structuredContentPresent"] = true
	}
	if representationError != "" {
		// The tool executed, but the SDK/bridge cannot preserve this result
		// faithfully. Convert it into an ordinary typed tool failure so the model
		// can recover; do not emit contractError, which would turn a successful
		// client continuation into a retry-looping HTTP 422.
		r["content"] = representationError
		r["contentBlocks"] = []map[string]any{{"type": "text", "text": representationError}}
		r["isError"] = true
		r["structuredContent"] = map[string]any{
			"applied":  false,
			"code":     "client_tool_result_unrepresentable",
			"executed": true,
		}
		r["structuredContentPresent"] = true
		delete(r, "contractError")
		delete(r, "images")
	}
	return r
}

// lastAssistantToolCallsIdx returns the index of the last assistant message bearing tool_calls, or -1.
func lastAssistantToolCallsIdx(messages []gjson.Result) int {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").Exists() {
			return i
		}
	}
	return -1
}

// trailingContinuationUserIdx returns the final role:user message after the
// final role:tool result in the unresolved continuation region. Stateless
// clients resend the whole transcript after an HTTP failure; concatenating
// every unanswered user entry makes each retry a larger, different message
// and can permanently poison idempotency. The final user entry is the current
// turn; earlier unanswered entries are superseded request snapshots.
func trailingContinuationUserIdx(messages []gjson.Result, start int) int {
	lastTool := -1
	for i := start; i < len(messages); i++ {
		if messages[i].Get("role").String() == "tool" {
			lastTool = i
		}
	}
	if lastTool < 0 {
		return -1
	}
	for i := len(messages) - 1; i > lastTool; i-- {
		if messages[i].Get("role").String() == "user" &&
			(cursorMessageText(messages[i]) != "" || messageHasImageParts(messages[i])) {
			return i
		}
	}
	return -1
}

func latestContiguousToolResults(messages []gjson.Result, start, end int) []map[string]any {
	lastTool := -1
	for i := end - 1; i >= start; i-- {
		if messages[i].Get("role").String() == "tool" {
			lastTool = i
			break
		}
	}
	if lastTool < 0 {
		return nil
	}
	batchStart := lastTool
	for batchStart > start && messages[batchStart-1].Get("role").String() == "tool" {
		batchStart--
	}
	results := make([]map[string]any, 0, lastTool-batchStart+1)
	for i := batchStart; i <= lastTool; i++ {
		results = append(results, composerToolResultEntry(messages[i]))
	}
	return results
}

// composerToolResultsHinted classifies the incoming turn: a trailing tool message means the client returned
// tool results (continuation); otherwise it is a new user turn. It extracts a tool_results continuation from
// the (OpenAI-shape) messages and is the single source of continuation detection (composerInput,
// lookupSessionByToolResults, deriveComposerSessionID all use it) so a mixed turn is never misclassified as a
// fresh user turn (Comment 4).
//
//   - (a) Preferred: role:tool results following the LAST assistant message bearing tool_calls, with no later
//     assistant reply. trailing carries any accompanying user text (a mixed tool_result+text turn).
//   - (b) Fallback: a turn that simply ENDS with a contiguous run of role:tool messages.
//   - (c) ADD-65: a Responses server-side-chained continuation [..., role:tool(+), role:user] with NO assistant
//     tool_calls adjacency and a trailing role:user — recognized ONLY when the hint confirms it (a
//     previous_response_id is present OR a trailing tool_call_id is owned for this tenant), so an ordinary
//     conversation that merely ends [tool, user] is not misread.
func composerToolResultsHinted(messages []gjson.Result, hint composerContinuationHint) (results []map[string]any, trailing string, ok bool) {
	// (a) Preferred: the role:tool results that follow the LAST assistant message bearing tool_calls, as long
	// as the model has not yet replied to them (no later assistant). This is what makes a MIXED turn (tool
	// results + trailing user text) classify as a continuation instead of a fresh user turn (Comment 4).
	lastTC := lastAssistantToolCallsIdx(messages)
	if lastTC >= 0 {
		replied := false
		for i := lastTC + 1; i < len(messages); i++ {
			if messages[i].Get("role").String() == "assistant" {
				replied = true
			}
		}
		// Only the latest contiguous result batch belongs to this continuation.
		// Older results separated by user input are replayed history from a prior
		// signed round; combining them would make one invocation identity span
		// multiple routes. Parallel results from the current round remain grouped.
		end := len(messages)
		if trailingIndex := trailingContinuationUserIdx(messages, lastTC+1); trailingIndex >= 0 {
			end = trailingIndex
		}
		res := latestContiguousToolResults(messages, lastTC+1, end)
		if len(res) > 0 && !replied {
			trailing := ""
			if index := trailingContinuationUserIdx(messages, lastTC+1); index >= 0 {
				trailing = cursorMessageText(messages[index])
			}
			return res, trailing, true
		}
	}
	// (b) Fallback: a turn that simply ENDS with a contiguous run of role:tool messages (truncated history,
	// or a lone results turn with no assistant tool_calls in view). Preserves the original detection.
	if n := len(messages); n > 0 && messages[n-1].Get("role").String() == "tool" {
		res := make([]map[string]any, 0)
		for i := n - 1; i >= 0; i-- {
			if messages[i].Get("role").String() != "tool" {
				break
			}
			res = append([]map[string]any{composerToolResultEntry(messages[i])}, res...)
		}
		if len(res) > 0 {
			return res, "", true
		}
	}
	// (c) ADD-65 (C-ADD65-RESPID-CONT): a Responses/Codex server-side-chained continuation. The client sent the
	// current function_call_output (-> role:tool) plus new user text (-> trailing role:user) WITHOUT resending
	// the prior assistant function_call, so there is no assistant tool_calls anchor (branch a misses) and the
	// LAST message is role:user, not role:tool (branch b misses). Collect the trailing role:tool run + the
	// trailing user text EXACTLY like branch (a) — but ONLY when the hint confirms this is a real continuation
	// (a previous_response_id is present, or a trailing tool_call_id has this bridge's signed prefix), so we never
	// reclassify an ordinary conversation that merely ends [..., tool, user]. This shape must NEVER fall through
	// to the fresh-user-turn branch, which would strand the tool output as inert history behind a paused run.
	if hint.hasPreviousResponseID || hint.hasClientToolID != nil {
		// Find the trailing region after the LAST assistant message of ANY kind (the prior turn boundary). In the
		// chained shape there IS no assistant (lastAssistant=-1), so the region is the whole message list; the
		// per-result loop below ignores leading (prior-history) user text by only collecting user text that comes
		// AFTER the first tool result.
		lastAssistant := -1
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Get("role").String() == "assistant" {
				lastAssistant = i
				break
			}
		}
		signedIDSignal := false
		// A server-side-chained request carries the current contiguous result
		// batch immediately before its optional trailing user message. Older
		// tool messages are replayed history and may belong to different signed
		// routes; forwarding all of them creates a false mixed-round conflict.
		end := len(messages)
		if trailingIndex := trailingContinuationUserIdx(messages, lastAssistant+1); trailingIndex >= 0 {
			end = trailingIndex
		}
		res := latestContiguousToolResults(messages, lastAssistant+1, end)
		for _, tr := range res {
			if hint.hasClientToolID != nil {
				if id, _ := tr["toolCallId"].(string); strings.TrimSpace(id) != "" && hint.hasClientToolID(id) {
					signedIDSignal = true
				}
			}
		}
		if len(res) > 0 && (hint.hasPreviousResponseID || signedIDSignal) {
			trailing := ""
			if index := trailingContinuationUserIdx(messages, lastAssistant+1); index >= 0 {
				trailing = cursorMessageText(messages[index])
			}
			return res, trailing, true
		}
	}
	return nil, "", false
}

// composerLegacyUnsignedResultReplay identifies a continuation batch emitted before signed cct1 routing was
// introduced. It is deliberately all-or-nothing: a batch containing even one signed id still belongs to the
// durable ToolRound path and must never be downgraded to replay recovery merely because another id is malformed.
func composerLegacyUnsignedResultReplay(results []map[string]any, hint composerContinuationHint) bool {
	if len(results) == 0 || hint.hasClientToolID == nil {
		return false
	}
	for _, result := range results {
		id, _ := result["toolCallId"].(string)
		id = strings.TrimSpace(id)
		if id == "" || hint.hasClientToolID(id) {
			return false
		}
	}
	return true
}

// composerLegacyReplayHistoryBound keeps completed legacy tool calls/results and subsequent injected notices in
// the bounded replay while excluding the latest user instruction, which is sent separately. With no trailing
// user instruction, the complete message list is replayed.
func composerLegacyReplayHistoryBound(messages []gjson.Result) int {
	lastTool := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Get("role").String() == "tool" {
			lastTool = i
			break
		}
	}
	for i := len(messages) - 1; i > lastTool; i-- {
		if messages[i].Get("role").String() == "user" &&
			(cursorMessageText(messages[i]) != "" || messageHasImageParts(messages[i])) {
			return i
		}
	}
	return len(messages)
}

func composerClientMessageID(results []map[string]any, userText string, images []map[string]any, turnAnchor ...string) string {
	toolCallIDSet := make(map[string]struct{}, len(results))
	for _, result := range results {
		if id, ok := result["toolCallId"].(string); ok && strings.TrimSpace(id) != "" {
			toolCallIDSet[id] = struct{}{}
		}
	}
	// A tool-result batch is a set for continuation routing. Result ordering,
	// JSON key order, whitespace, and escape spellings are serialization
	// details and must not generate a second durable user-input identity.
	toolCallIDs := make([]string, 0, len(toolCallIDSet))
	for id := range toolCallIDSet {
		toolCallIDs = append(toolCallIDs, id)
	}
	sort.Strings(toolCallIDs)
	payload := map[string]any{
		"images":      images,
		"text":        userText,
		"toolCallIds": toolCallIDs,
		"version":     2,
	}
	if len(turnAnchor) > 0 && turnAnchor[0] != "" {
		// A fresh message may legitimately repeat the same text ("continue" is
		// common). Bind its retry id to the semantic transcript before that
		// message so exact HTTP retries remain stable but a later identical turn
		// does not reuse the prior SDK idempotency key. Tool-result ids already
		// supply their own immutable turn anchor and therefore omit this field.
		payload["turnAnchor"] = turnAnchor[0]
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return "ccm2_" + base64.RawURLEncoding.EncodeToString(sum[:18])
}

// composerInput is the back-compat entry that classifies a turn with a BODY-ONLY hint (previous_response_id).
// The executor calls composerInputHinted with the signed-id-aware hint so branch (c)
// classification matches deriveComposerSessionID exactly (no route/send mismatch).
func composerInput(oai []byte) map[string]any {
	return composerInputHinted(oai, composerContinuationHint{
		hasPreviousResponseID: strings.TrimSpace(gjson.GetBytes(oai, "previous_response_id").String()) != "",
	})
}

func composerInputHinted(oai []byte, hint composerContinuationHint) map[string]any {
	messages := gjson.GetBytes(oai, "messages").Array()
	// Tool_results continuation detection (Comment 4 / ADD-65): a continuation is the LAST assistant turn
	// bearing tool_calls, followed by role:tool results the model has NOT yet replied to (no later assistant),
	// OR (ADD-65) a Responses server-side-chained [..., tool, user] shape confirmed by the hint. Keying only on
	// "the last message is role:tool" misclassifies a MIXED turn carrying tool_result blocks AND trailing user
	// text as a fresh user turn, stranding the paused run's tools. Collect the results regardless of trailing
	// user text; carry that text intentionally.
	if results, trailing, ok := composerToolResultsHinted(messages, hint); ok {
		inp := map[string]any{"type": "tool_results", "results": results}
		legacyUnsignedReplay := composerLegacyUnsignedResultReplay(results, hint)
		if legacyUnsignedReplay {
			// The bridge owns the compatibility decision. Go supplies a complete bounded replay and marker, but
			// still forwards every opaque id unchanged to /agent/continue. Signed or mixed batches never enter
			// this path and retain strict durable ToolRound validation.
			inp["legacyUnsignedReplay"] = true
		}
		// Tool results are immutable receipts. A trailing user message is a
		// separate durable input and must never be folded into a receipted result
		// under the same signed id.
		if t := strings.TrimSpace(trailing); t != "" {
			inp["userText"] = t
		}
		// EX4/EX14: carry any image attachments from the trailing user message(s) of this continuation so the
		// bridge's separate fresh send can attach them. Re-scan the messages after the last
		// assistant tool_calls turn for role:user image parts (keeps composerToolResults text-only/focused).
		trailingImages := trailingContinuationImages(messages)
		if len(trailingImages) > 0 {
			inp["images"] = trailingImages
		}
		// Every result batch gets a stable semantic request identity, including a
		// result-only continuation. The bridge uses it to replay a completed
		// response after a lost HTTP response instead of invoking agent.send twice.
		userText, hasText := inp["userText"].(string)
		inp["clientMessageId"] = composerClientMessageID(results, userText, trailingImages)
		if hasText || len(trailingImages) > 0 {
			inp["interruptRequested"] = true
		}
		// EX8 (C3): a system-prompt swap (e.g. post-ExitPlanMode) on a continuation must reach the model.
		// Carry exact block identity as well as the aggregate text so a warm bridge can append only a true
		// suffix and can distinguish a replacement/reorder from ordinary context growth.
		systemBlocks := extractComposerSystemBlocks(messages, "")
		// A server-chained previous_response_id request may intentionally omit
		// standing context because the server owns it. Do not reinterpret that
		// omission as removal. Full stateless snapshots remain authoritative,
		// including an explicit empty block list that means remove all context.
		if len(systemBlocks) > 0 || !hint.hasPreviousResponseID {
			inp["system"] = composerSystemBlocksText(systemBlocks)
			inp["systemBlocks"] = systemBlocks
		}
		// EX10: carry rendered history so the bridge can seed a fresh session before applying these results
		// (recovers an evicted/restarted/410'd session). Bounded per EX13 inside renderComposerHistory.
		historyBound := continuationHistoryBound(messages)
		if legacyUnsignedReplay {
			// A live signed continuation replays history only before its current batch because structured results
			// are applied to SDK callbacks. An unsigned legacy batch has no callback after a bridge upgrade/restart,
			// so its replay must include the assistant calls plus completed results or the model would repeat them.
			historyBound = composerLegacyReplayHistoryBound(messages)
		}
		if hist := renderComposerHistory(messages, historyBound); hist != "" {
			inp["history"] = hist
		}
		// EX7 (C2): fingerprint the non-system history so a /compact-style rewrite re-seeds the bridge.
		if fp := composerHistoryFingerprint(messages); fp != "" {
			inp["historyFingerprint"] = fp
		}
		return inp
	}
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Get("role").String() == "user" {
			lastUserIdx = i
			break
		}
	}
	inp := map[string]any{"type": "user", "text": ""}
	var currentImages []map[string]any
	priorHistory := renderComposerHistory(messages, lastUserIdx)
	systemBlocks := extractComposerSystemBlocks(messages, "")
	systemPrompt := composerSystemBlocksText(systemBlocks)
	if lastUserIdx >= 0 {
		m := messages[lastUserIdx]
		text := cursorMessageText(m)
		imgs := extractComposerImages(m)
		if len(imgs) > 0 {
			inp["images"] = imgs
			currentImages = imgs
		} else if messageHasImageParts(m) {
			// ADD-56: the turn had image part(s) but none survived extraction (malformed/unsupported). Never let
			// it become a silent empty turn. A pure image-only-invalid turn is rejected upstream by the executor
			// (errComposerImageOnlyInvalid); here (mixed text+invalid-image, or as a defensive fallback) we make
			// the invalid attachment model-visible instead of pretending it never existed.
			if strings.TrimSpace(text) == "" {
				text = composerImageInvalidPlaceholder
			} else {
				text = strings.TrimRight(text, "\n") + "\n\n" + composerImageInvalidPlaceholder
			}
		}
		inp["text"] = text
		if text != "" || len(currentImages) > 0 {
			// Fresh turns need a transport-retry identity that is also distinct
			// from a later intentional repetition of the same text. The bounded
			// semantic prior transcript provides that turn anchor without relying
			// on JSON key order or process-local counters.
			inp["clientMessageId"] = composerClientMessageID(
				nil,
				text,
				currentImages,
				systemPrompt+"\x00"+priorHistory,
			)
		}
	}
	if len(systemBlocks) > 0 || !hint.hasPreviousResponseID {
		inp["system"] = systemPrompt
		inp["systemBlocks"] = systemBlocks
	}
	if priorHistory != "" {
		inp["history"] = priorHistory
	}
	// EX7 (C2): fingerprint the non-system history on the new-user path too.
	if fp := composerHistoryFingerprint(messages); fp != "" {
		inp["historyFingerprint"] = fp
	}
	return inp
}

// continuationHistoryBound returns the index up to which renderComposerHistory should render a
// continuation's history: everything before the LAST assistant message bearing tool_calls (the calls the
// trailing tool results answer). That keeps the seeded transcript to the pre-tool-call context. Falls back
// to the full message count when no such assistant message is found.
func continuationHistoryBound(messages []gjson.Result) int {
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").Exists() {
			return i
		}
	}
	return len(messages)
}

// trailingContinuationImages collects image parts from the final role:user
// message after the final tool result. It deliberately mirrors
// composerToolResultsHinted's latest-snapshot rule so failed HTTP retries do
// not accumulate images from every unanswered user entry.
func trailingContinuationImages(messages []gjson.Result) []map[string]any {
	lastTC := -1
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").Exists() {
			lastTC = i
			break
		}
	}
	if index := trailingContinuationUserIdx(messages, lastTC+1); index >= 0 {
		return extractComposerImages(messages[index])
	}
	return nil
}

// composerFunctionDefs returns the function definitions the request exposes, as gjson objects with
// {name, description, parameters}. It reads the modern tools[] (each {type:"function", function:{...}})
// and falls back to the deprecated OpenAI functions[] (each {name, description, parameters} directly) so
// legacy function-calling clients still get their tools advertised on the default Cursor Composer Client-Tools path.
func composerFunctionDefs(oai []byte) []gjson.Result {
	var fns []gjson.Result
	for _, t := range gjson.GetBytes(oai, "tools").Array() {
		if fn := t.Get("function"); fn.Exists() {
			fns = append(fns, fn)
		}
	}
	if len(fns) == 0 {
		for _, fn := range gjson.GetBytes(oai, "functions").Array() {
			if fn.Get("name").Exists() {
				fns = append(fns, fn)
			}
		}
	}
	return fns
}

// composerAdvertise converts the request's function definitions into the bridge's advertise shape
// [{name, description, inputSchema}] so the model can call the client's tools.
func composerAdvertise(oai []byte) []map[string]any {
	fns := composerFunctionDefs(oai)
	out := make([]map[string]any, 0, len(fns))
	for _, fn := range fns {
		entry := map[string]any{"name": fn.Get("name").String(), "description": fn.Get("description").String()}
		if params := fn.Get("parameters"); params.Exists() {
			var schema any
			decoder := json.NewDecoder(strings.NewReader(params.Raw))
			decoder.UseNumber()
			if err := decoder.Decode(&schema); err == nil {
				entry["inputSchema"] = schema
			}
		}
		// ADD-99: preserve the OpenAI Structured-Outputs `strict` hint so the bridge advertise carries it (the
		// bridge can pass it to the SDK if/when it has a field for it). It is NOT hard-validated on this path —
		// composerConstraints additionally flags strict:true as an unsupported hard guarantee.
		if s := fn.Get("strict"); s.Exists() {
			entry["strict"] = s.Bool()
		}
		out = append(out, entry)
	}
	return out
}

// composerAllowedToolNames returns the tool names from a Chat-Completions `allowed_tools` object (carried
// through by the Responses request translator per C-TOOLCHOICE: {"type":"allowed_tools","mode":...,
// "tools":[{"type":"function","name":"Read"},...]}) — the client's allow-list for this turn. nil when absent
// or not an allowed_tools object. H07/H22: an allow-list must NOT be silently widened to all tools.
func composerAllowedToolNames(oai []byte) []string {
	at := gjson.GetBytes(oai, "allowed_tools")
	if !at.Exists() || !at.IsObject() {
		return nil
	}
	if t := at.Get("type").String(); t != "" && t != "allowed_tools" {
		return nil
	}
	var names []string
	for _, t := range at.Get("tools").Array() {
		if n := strings.TrimSpace(t.Get("name").String()); n != "" {
			names = append(names, n)
		} else if n := strings.TrimSpace(t.Get("function.name").String()); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// applyComposerAllowedTools restricts the advertised tool set to the client's `allowed_tools` allow-list
// (H07/H22). It resolves each allowed name through the client's tools + aliases (parity with
// resolveComposerToolChoice) before intersecting, so a generic/aliased allowed name still matches the
// advertised tool. Returns the restricted advertise set and unsupported=true when an allow-list was present
// but its intersection with the advertised tools is EMPTY — the caller then surfaces explicit-unsupported
// rather than falling back to ALL tools (never silently widen a restriction). When no allow-list is present,
// returns the input advertise set unchanged with unsupported=false. NOTE: this is best-effort structural
// gating of the ADVERTISE set only; Cursor-native built-ins are not in the advertise set and cannot be
// structurally un-advertised here (see H08 / constraintInstructions on the bridge).
func applyComposerAllowedTools(advertise []map[string]any, oai []byte, defs []cursorToolDefinition, overrides map[string]string) (restricted []map[string]any, unsupported bool) {
	allowed := composerAllowedToolNames(oai)
	if len(allowed) == 0 {
		return advertise, false
	}
	allow := map[string]bool{}
	for _, n := range allowed {
		if spec := resolveToolSpec(n, defs, overrides); spec != nil {
			allow[spec.Name] = true
		} else {
			allow[n] = true // keep the raw name so an exact advertise match still works
		}
	}
	out := make([]map[string]any, 0, len(advertise))
	for _, a := range advertise {
		name, _ := a["name"].(string)
		if allow[name] {
			out = append(out, a)
		}
	}
	if len(out) == 0 {
		// Allow-list present but nothing advertised matches: surface explicit-unsupported. Returning all tools
		// would violate the client's restriction; returning a forced-tool-unavailable signal is the honest
		// degrade (the model is told the requested tools are not available, not handed unrelated ones).
		return nil, true
	}
	return out, false
}

// composerToolDefs builds the client's tool definitions for name/arg reconciliation.
func composerToolDefs(oai []byte) []cursorToolDefinition {
	fns := composerFunctionDefs(oai)
	defs := make([]cursorToolDefinition, 0, len(fns))
	for _, fn := range fns {
		defs = append(defs, cursorToolDefinition{
			Name:        fn.Get("name").String(),
			Description: fn.Get("description").String(),
			Parameters:  fn.Get("parameters").Raw,
		})
	}
	return defs
}

// composerToolAliases returns caller-configured tool-name overrides (emitted/generic name -> client tool
// name), merged from env CURSOR_TOOL_ALIASES (base) and the per-key tool_aliases attribute (which wins).
// Accepts a JSON object ({"shell":"RunCommand"}) or a "from=to,from=to" list. Keys are normalized so they
// match regardless of case/punctuation. Empty when nothing is configured (built-in resolution only).
func composerToolAliases(auth *cliproxyauth.Auth) map[string]string {
	out := map[string]string{}
	merge := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return
		}
		if strings.HasPrefix(raw, "{") {
			// Decode to any + coerce per value: one non-string value must not discard the whole map
			// (json.Unmarshal into map[string]string fails atomically), just skip that entry.
			var m map[string]any
			if json.Unmarshal([]byte(raw), &m) == nil {
				for k, v := range m {
					s, ok := v.(string)
					if !ok {
						continue
					}
					if nk := normalizeToolName(k); nk != "" && strings.TrimSpace(s) != "" {
						out[nk] = strings.TrimSpace(s)
					}
				}
			}
			return
		}
		for _, pair := range strings.Split(raw, ",") {
			if kv := strings.SplitN(pair, "=", 2); len(kv) == 2 {
				if nk := normalizeToolName(kv[0]); nk != "" && strings.TrimSpace(kv[1]) != "" {
					out[nk] = strings.TrimSpace(kv[1])
				}
			}
		}
	}
	merge(os.Getenv("CURSOR_TOOL_ALIASES")) // base
	if auth != nil && auth.Attributes != nil {
		merge(auth.Attributes["tool_aliases"]) // per-key override wins
	}
	return out
}

// mapComposerToolCall reconciles a bridge-emitted tool name + args against the client's actual tools
// (caller override -> exact -> normalized -> built-in alias), returning the client's exact tool name
// and normalized argument JSON.
//
// ADD-104 (exec half): when the bridge emits a tool call whose `input` EXISTS but is NOT a JSON object
// (a string / number / array / boolean / null — e.g. an MCP tool whose schema is a primitive, or a raw
// JSON-string argument), preserve it as {"input": <raw value>} instead of collapsing it to {} and stranding
// the client with empty arguments (a silent wrong-tool invocation). The wrapper key 'input' is the pinned
// cross-file contract so a client that introspects sees a stable shape on both the bridge and executor halves.
func mapComposerToolCall(name string, input gjson.Result, defs []cursorToolDefinition, overrides map[string]string) (string, string) {
	// The bridge's ToolContractRegistry is authoritative for live Client-Tools
	// events: it resolves the exact client descriptor, normalizes arguments,
	// validates them, and journals that canonical payload before emitting SSE.
	// Preserve an exact bridge name byte-for-byte here so Go cannot add defaults
	// or otherwise make the harness execute arguments different from the durable
	// ToolRound receipt. The legacy resolver below remains only for older or
	// noncanonical bridge events during an upgrade.
	for i := range defs {
		if defs[i].Name != name {
			continue
		}
		if input.Exists() && input.IsObject() {
			return name, input.Raw
		}
		if !input.Exists() {
			return name, "{}"
		}
		value, _ := decodeComposerJSON(input.Raw)
		b, _ := json.Marshal(map[string]any{"input": value})
		return name, string(b)
	}
	spec := resolveToolSpec(name, defs, overrides)
	outName := name
	if spec != nil {
		outName = spec.Name
	}
	if input.Exists() && !input.IsObject() {
		// Non-object input: wrap the RAW value verbatim under 'input' so its semantics survive to the client.
		// Skip schema normalization (which is keyed on an object matching the tool schema) — there is no object
		// to reconcile, and reshaping a primitive would lose it.
		value, _ := decodeComposerJSON(input.Raw)
		b, _ := json.Marshal(map[string]any{"input": value})
		return outName, string(b)
	}
	args := map[string]any{}
	if input.Exists() && input.IsObject() {
		if value, err := decodeComposerJSON(input.Raw); err == nil {
			if object, ok := value.(map[string]any); ok {
				args = object
			}
		}
	}
	if spec == nil {
		b, _ := json.Marshal(args)
		return name, string(b)
	}
	normalized := normalizeToolArguments(args, spec)
	b, _ := json.Marshal(normalized)
	return spec.Name, string(b)
}

// attachComposerToolAliases projects explicit operator aliases onto the exact
// advertised descriptor they target. The bridge can then resolve native SDK
// names without emitting an unadvertised fallback and without relying on Go to
// rewrite a call after it has already been journaled. Alias keys are normalized
// by composerToolAliases; bridge matching is normalized too.
func attachComposerToolAliases(advertise []map[string]any, defs []cursorToolDefinition, overrides map[string]string) []map[string]any {
	if len(advertise) == 0 || len(overrides) == 0 {
		return advertise
	}
	byTarget := make(map[string][]string)
	for alias := range overrides {
		if spec := resolveToolOverride(alias, overrides, defs); spec != nil {
			byTarget[spec.Name] = append(byTarget[spec.Name], alias)
		}
	}
	for _, entry := range advertise {
		name, _ := entry["name"].(string)
		aliases := byTarget[name]
		if len(aliases) == 0 {
			continue
		}
		sort.Strings(aliases)
		entry["aliases"] = aliases
	}
	return advertise
}

// composerAdvertisePrep holds advertise/toolChoice/constraints after allow-list and tool_choice gating.
type composerAdvertisePrep struct {
	advertise   []map[string]any
	toolChoice  string
	constraints map[string]any
}

// prepareComposerAdvertise builds the advertise set and response constraints for a /agent/turn body.
func prepareComposerAdvertise(oai []byte, defs []cursorToolDefinition, toolAliases map[string]string) composerAdvertisePrep {
	advertise := composerAdvertise(oai)
	toolChoice := resolveComposerToolChoice(oai, defs, toolAliases)
	constraints := composerConstraints(oai)
	if toolChoice == "none" {
		advertise = nil
	} else {
		var unsupportedTools bool
		advertise, unsupportedTools = applyComposerAllowedTools(advertise, oai, defs, toolAliases)
		if unsupportedTools {
			constraints = appendUnsupportedGuarantee(constraints, "allowed_tools requested but none of the allowed tools are available (no tools advertised; do not call any tool)")
		}
	}
	advertise = attachComposerToolAliases(advertise, defs, toolAliases)
	return composerAdvertisePrep{advertise: advertise, toolChoice: toolChoice, constraints: constraints}
}

// composerTurnBody builds the JSON body for a POST /agent/turn. constraints carries the enforced
// response constraints (responseFormat/stop/maxTokens) as explicit top-level fields; the bridge
// converts them (and toolChoice) into model instructions and tool-advertisement gating.
func composerToolInventorySnapshot(advertise []map[string]any) (string, string) {
	// composerAdvertise constructs this tree exclusively from JSON values plus
	// proxy-owned strings/bools, so Marshal cannot encounter channels,
	// functions, cycles, NaN, or infinities. UseNumber above also preserves the
	// client's valid JSON number spelling until this one authoritative encoding.
	raw, _ := json.Marshal(advertise)
	sum := sha256.Sum256(raw)
	return string(raw), "cti1_" + hex.EncodeToString(sum[:16])
}

func composerToolInventoryEpoch(advertise []map[string]any) string {
	_, epoch := composerToolInventorySnapshot(advertise)
	return epoch
}

func composerTurnBody(sessionID, model string, input map[string]any, advertise []map[string]any, toolChoice string, clientEnv map[string]any, constraints map[string]any, leaseOwner uint64) []byte {
	body := map[string]any{
		"sessionId":          sessionID,
		"model":              model,
		"input":              input,
		"contractVersion":    2,
		"toolsAuthoritative": true,
	}
	// The bridge persists this opaque owner token with every ToolRound and
	// echoes it on continuation terminals. That lets a signed /continue finish
	// the exact fresh-turn lease which opened the paused SDK callback without
	// teaching Go how to decode or own tool-call routing ids. Decimal text is
	// lossless across the Go/JavaScript boundary (uint64 exceeds JS Number).
	if leaseOwner != 0 {
		body["clientLeaseToken"] = strconv.FormatUint(leaseOwner, 10)
	}
	// H10 (C-CONTINUATION-TOOLS): attach the current tool inventory on EVERY turn when advertised, not only
	// on a new-user turn. The bridge refreshes session.advertise from the authoritative snapshot on tool_results turns too, so
	// dropping tools on a continuation left the bridge with a STALE advertise set (removed/added tools, plan-mode
	// ExitPlanMode, MCP availability changes). The tool_choice=none gating in executeComposer/executeComposerStream
	// runs BEFORE this (advertise=nil there), so `none` still sends no tools — that ordering is intentional.
	// The inventory is an authoritative snapshot on every HTTP turn. An empty
	// array explicitly clears a reused bridge session; omitting the field left
	// stale tools executable after tool_choice:none, allowed-tools narrowing, or
	// dynamic client-tool removal.
	tools := advertise
	if tools == nil {
		tools = []map[string]any{}
	}
	// Serialize the authoritative tool inventory exactly once. The bridge
	// verifies the epoch over these same UTF-8 bytes before parsing the snapshot.
	// Re-serializing independently in JavaScript is not equivalent for every
	// valid JSON number/key shape (for example integers above 2^53 or -0), and
	// previously caused valid dynamic client/MCP inventories to fail with 422.
	toolInventoryJSON, toolInventoryEpoch := composerToolInventorySnapshot(tools)
	body["toolInventoryJSON"] = toolInventoryJSON
	body["toolInventoryEpoch"] = toolInventoryEpoch
	if toolChoice != "" {
		body["toolChoice"] = toolChoice
	}
	if len(clientEnv) > 0 {
		body["clientEnv"] = clientEnv
	}
	for k, v := range constraints {
		body[k] = v
	}
	b, _ := json.Marshal(body)
	return b
}

// composerResponseID returns a unique id per request. Clients that correlate streaming chunks to their
// non-stream counterpart (or dedupe by id) must not see the same id across unrelated responses.
const composerRoutedResponseIDPrefix = "chatcmpl-composer-cr1_"

var composerRoutedSessionIDPattern = regexp.MustCompile(`^sess_[A-Za-z0-9_-]{8,240}$`)

func composerResponseRouteMAC(apiKey, tenant, sessionID, nonce string) []byte {
	return composerResponseRouteMACBound(apiKey, tenant, sessionID, "", nonce)
}

func composerResponseRouteMACBound(apiKey, tenant, sessionID, binding, nonce string) []byte {
	key := []byte(apiKey)
	if len(key) == 0 {
		// A resolved Cursor key is present on every production composer request.
		// Retain a process-local authenticated fallback for tests or malformed
		// internal calls; it deliberately cannot claim restart durability.
		key = serverSecret[:]
	}
	mac := hmac.New(sha256.New, key)
	domain := "cursor-composer-response-route-v1\x00"
	if binding != "" {
		domain = "cursor-composer-response-route-v2\x00"
	}
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte(tenant))
	_, _ = mac.Write([]byte{'\x00'})
	_, _ = mac.Write([]byte(sessionID))
	_, _ = mac.Write([]byte{'\x00'})
	if binding != "" {
		_, _ = mac.Write([]byte(binding))
		_, _ = mac.Write([]byte{'\x00'})
	}
	_, _ = mac.Write([]byte(nonce))
	return mac.Sum(nil)[:16]
}

// composerResponseID emits an OpenAI-compatible opaque id that authenticates
// the durable bridge session route. A Responses client may return only this id
// after a Go restart; no process-local map or replayed history is required.
func composerResponseID(apiKey, tenant, sessionID string) string {
	return composerResponseIDBound(apiKey, tenant, sessionID, "")
}

func composerResponseIDBound(apiKey, tenant, sessionID, binding string) string {
	nonce := composerRandHex(8)
	sid := base64.RawURLEncoding.EncodeToString([]byte(sessionID))
	sig := base64.RawURLEncoding.EncodeToString(composerResponseRouteMACBound(apiKey, tenant, sessionID, binding, nonce))
	// Dot is outside the base64url and hexadecimal alphabets, so parsing never
	// depends on whether an opaque encoded component happens to contain '_'.
	if binding != "" {
		return composerRoutedResponseIDPrefix + sid + "." + binding + "." + nonce + "." + sig
	}
	return composerRoutedResponseIDPrefix + sid + "." + nonce + "." + sig
}

func composerSessionFromResponseID(tenant, apiKey, responseID string) string {
	return composerRouteFromResponseID(tenant, apiKey, responseID).sessionID
}

func composerRouteFromResponseID(tenant, apiKey, responseID string) composerResponseRoute {
	if !strings.HasPrefix(responseID, composerRoutedResponseIDPrefix) {
		return composerResponseRoute{}
	}
	parts := strings.Split(strings.TrimPrefix(responseID, composerRoutedResponseIDPrefix), ".")
	if len(parts) != 3 && len(parts) != 4 {
		return composerResponseRoute{}
	}
	binding := ""
	nonceIndex, signatureIndex := 1, 2
	if len(parts) == 4 {
		binding = parts[1]
		nonceIndex, signatureIndex = 2, 3
		if len(binding) != 64 {
			return composerResponseRoute{}
		}
		if _, err := hex.DecodeString(binding); err != nil {
			return composerResponseRoute{}
		}
	}
	if len(parts[nonceIndex]) != 16 || len(parts[signatureIndex]) != 22 {
		return composerResponseRoute{}
	}
	if _, err := hex.DecodeString(parts[nonceIndex]); err != nil {
		return composerResponseRoute{}
	}
	rawSession, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || !utf8.Valid(rawSession) || base64.RawURLEncoding.EncodeToString(rawSession) != parts[0] {
		return composerResponseRoute{}
	}
	sessionID := string(rawSession)
	if !composerRoutedSessionIDPattern.MatchString(sessionID) {
		return composerResponseRoute{}
	}
	supplied, err := base64.RawURLEncoding.DecodeString(parts[signatureIndex])
	if err != nil || len(supplied) != 16 || base64.RawURLEncoding.EncodeToString(supplied) != parts[signatureIndex] {
		return composerResponseRoute{}
	}
	expected := composerResponseRouteMACBound(apiKey, tenant, sessionID, binding, parts[nonceIndex])
	if !hmac.Equal(supplied, expected) {
		return composerResponseRoute{}
	}
	return composerResponseRoute{sessionID: sessionID, conversationBinding: binding}
}

// oaiChunk wraps an OpenAI chat.completion.chunk choice delta as an SSE "data:" line.
func oaiChunk(id, model string, choice map[string]any) []byte {
	c := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{choice},
	}
	b, _ := json.Marshal(c)
	return append([]byte("data: "), b...)
}

// ACCURATE ACCOUNTING (invariant 4): @cursor/sdk exposes NO per-turn token usage (Cursor meters server-side),
// so a composer turn would otherwise report 0 tokens — which a client (e.g. a Claude Code workflow) renders as
// "0 tok / did no work". composerEstimateTokens approximates a token count from a character count (~4 chars per
// token) so the proxy reports a realistic NON-ZERO usage for display and the informational ledger. It is an
// ESTIMATE, not billing-grade — the authoritative meter is Cursor's account.
func composerEstimateTokens(chars int) int {
	if chars <= 0 {
		return 0
	}
	return (chars + 3) / 4
}

// composerPromptChars estimates the size of the FULL inbound conversation (the estimate input for prompt tokens —
// what Claude Code's auto-compact reads back via message_start.usage.input_tokens). It sums each message's visible
// text via cursorMessageText (shared VERBATIM with the lineage head digest, so it must not change) PLUS assistant
// tool-call ARGUMENTS, which are not visible text but ARE a large part of a code-heavy conversation (patches, file
// writes, shell commands) — without them the estimate badly under-counts and auto-compact fires far too late. It
// counts the inbound conversation, NOT the proxy's history-bounded replay (auto-compact must track what CC holds,
// not what we forward downstream). Image base64 is deliberately not counted as prompt text.
func composerPromptChars(oai []byte) int {
	total := 0
	for _, m := range gjson.GetBytes(oai, "messages").Array() {
		total += len(cursorMessageText(m))
		for _, tc := range m.Get("tool_calls").Array() {
			total += len(tc.Get("function.name").String()) + len(tc.Get("function.arguments").String())
		}
	}
	return total
}

// composerSetMessageStartInputTokens rewrites usage.input_tokens in a translated Anthropic `message_start` SSE
// event to the composer prompt-token estimate. Claude Code's AUTO-COMPACT reads message.usage.input_tokens off
// the assistant turn (verified in the CC binary: `autocompact: tokens = input_tokens + cache_* + output_tokens`).
// The openai->claude translator hard-codes that field to 0, and a composer turn carries no upstream usage — so
// without this CC sees 0 tokens used and NEVER auto-compacts a composer session, while every native Claude model
// (which gets the real value in its own message_start) compacts normally. No-op on any chunk that is not a
// message_start, or when no estimate is available.
func composerSetMessageStartInputTokens(chunk []byte, inputTokens int) []byte {
	if inputTokens <= 0 || !bytes.Contains(chunk, []byte(`"type":"message_start"`)) {
		return chunk
	}
	idx := bytes.Index(chunk, []byte("data: "))
	if idx < 0 {
		return chunk
	}
	start := idx + len("data: ")
	rel := bytes.IndexByte(chunk[start:], '\n')
	if rel < 0 {
		return chunk
	}
	end := start + rel
	payload := chunk[start:end]
	if !gjson.GetBytes(payload, "message.usage").Exists() {
		return chunk
	}
	patched, err := sjson.SetBytes(payload, "message.usage.input_tokens", inputTokens)
	if err != nil {
		return chunk
	}
	out := make([]byte, 0, len(chunk)-len(payload)+len(patched))
	out = append(out, chunk[:start]...)
	out = append(out, patched...)
	out = append(out, chunk[end:]...)
	return out
}

// composerUsageChunk builds an OpenAI streaming usage frame (empty choices + a usage object) so the per-schema
// translator forwards token usage to the client (e.g. Anthropic message_delta.usage). It carries the composer
// usage ESTIMATE, since the bridge / @cursor/sdk provide none.
func composerUsageChunk(id, model string, promptTokens, completionTokens int) []byte {
	c := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	b, _ := json.Marshal(c)
	return append([]byte("data: "), b...)
}

// composerEstimatedUsageJSON returns the usage object (OpenAI shape) for the ledger reporter.
func composerEstimatedUsageJSON(promptTokens, completionTokens int) []byte {
	return []byte(fmt.Sprintf(`{"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, promptTokens, completionTokens, promptTokens+completionTokens))
}

// composerEstimatedUsageDetail parses the estimated usage JSON into a ledger Detail and marks it Estimated (P1-1),
// so downstream usage sinks can distinguish the @cursor/sdk's ~4-chars/token ESTIMATE (NOT billing-grade) from a
// real provider-reported count. Returns (zero, false) when there is nothing parseable to publish.
func composerEstimatedUsageDetail(promptTokens, completionTokens int) (coreusage.Detail, bool) {
	detail, ok := helps.ParseOpenAIStreamUsage(composerEstimatedUsageJSON(promptTokens, completionTokens))
	if !ok {
		return coreusage.Detail{}, false
	}
	detail.Estimated = true
	return detail, true
}

// schemaJSONContentType returns the Content-Type the OUTBOUND composer response should carry for the inbound
// schema (ADD-80 / RBT-035). The bridge always answers /agent/turn with text/event-stream, but the composer
// path TRANSLATES that into the caller's JSON (non-stream) or the caller schema's stream framing — so the
// bridge's SSE headers must NEVER be forwarded. Every inbound schema this proxy serves (openai / claude /
// openai-response / codex / gemini / gemini-cli) renders a JSON body, so the content type is application/json
// across the board; the helper keys on the inbound SCHEMA (the "canonical -> per-provider" rule) so a future
// schema with a different media type can diverge here without touching the call sites.
func schemaJSONContentType(from sdktranslator.Format) string {
	switch from.String() {
	case "gemini", "gemini-cli":
		// Gemini's REST surface answers with application/json (see sdk/api/handlers/gemini); keep parity.
		return "application/json"
	default:
		// openai / claude / openai-response / codex all use application/json for a non-streamed body.
		return "application/json"
	}
}

// composerJSONResponseHeaders builds the OUTBOUND header set for a translated composer response (ADD-80).
// It carries ONLY the schema-appropriate Content-Type — deliberately dropping the bridge's text/event-stream,
// Cache-Control, and other SSE/transport headers so a strict client SDK never mis-parses the JSON body as SSE.
func composerJSONResponseHeaders(from sdktranslator.Format) http.Header {
	h := http.Header{}
	h.Set("Content-Type", schemaJSONContentType(from))
	return h
}

// composerStreamResponseHeaders builds the OUTBOUND header set for a translated composer STREAM response
// (ADD-80). The per-schema streaming handler owns the streamed Content-Type (text/event-stream) and won't be
// overwritten, so this deliberately returns an EMPTY set: its only job is to make sure NONE of the bridge's
// transport/SSE/CDN headers (httpResp.Header) leak to the client through the StreamResult.Headers passthrough.
func composerStreamResponseHeaders() http.Header {
	return http.Header{}
}

const (
	composerEffectiveSessionHeader             = "X-CLIProxy-Composer-Session"
	composerEffectiveConversationBindingHeader = "X-CLIProxy-Composer-Conversation-Binding"
)

var composerConversationBindingPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// composerEffectiveContinuationSession returns the session selected by the
// bridge from the signed ToolRound. The locally derived continuation session
// is advisory because clients may reuse one parent conversation id across
// concurrent subagents. Only the authenticated internal bridge response may
// override it, and only with a valid routed session id.
func composerEffectiveContinuationSession(resp *http.Response, advisory string, continuation bool) string {
	if !continuation || resp == nil {
		return advisory
	}
	effective := strings.TrimSpace(resp.Header.Get(composerEffectiveSessionHeader))
	if !composerRoutedSessionIDPattern.MatchString(effective) {
		return advisory
	}
	return effective
}

// composerEffectiveContinuationBinding returns the conversation epoch owned by the signed ToolRound. The
// bridge emits the effective-session header only after it has loaded and authenticated a signed round; its
// companion binding therefore outranks the advisory request binding. When the signed round predates bindings,
// return empty rather than falsely promoting old state into a current /new epoch. A continuation split into a
// genuinely fresh turn emits neither authoritative header and retains the advisory fresh binding.
func composerEffectiveContinuationBinding(resp *http.Response, advisory string, continuation bool) (string, error) {
	if !continuation || resp == nil {
		return advisory, nil
	}
	effectiveSession := strings.TrimSpace(resp.Header.Get(composerEffectiveSessionHeader))
	if !composerRoutedSessionIDPattern.MatchString(effectiveSession) {
		return advisory, nil
	}
	binding := strings.TrimSpace(resp.Header.Get(composerEffectiveConversationBindingHeader))
	if binding == "" {
		return "", nil
	}
	if !composerConversationBindingPattern.MatchString(binding) {
		return "", fmt.Errorf("malformed authoritative conversation binding header")
	}
	return binding, nil
}

// composerContinuationLeaseStop validates the bridge's opaque fresh-lease
// echo. A continuation is allowed to mutate only the exact owner token which
// opened its signed ToolRound. Missing/malformed metadata is availability-safe:
// leave the lease for stale-TTL recovery instead of risking a newer run.
func composerContinuationLeaseStop(ev gjson.Result) (sessionID, leaseStop string, leaseOwner uint64) {
	lease := ev.Get("clientLease")
	if !lease.Exists() || !lease.IsObject() {
		return "", "", 0
	}
	sessionID = strings.TrimSpace(lease.Get("sessionId").String())
	if !composerRoutedSessionIDPattern.MatchString(sessionID) {
		return "", "", 0
	}
	token := strings.TrimSpace(lease.Get("token").String())
	owner, err := strconv.ParseUint(token, 10, 64)
	if err != nil || owner == 0 {
		return "", "", 0
	}
	terminalRaw := lease.Get("terminal").Raw
	if terminalRaw != "true" && terminalRaw != "false" {
		return "", "", 0
	}
	stop := ev.Get("stop_reason").String()
	if stop == "" {
		stop = "end_turn"
	}
	if stop != "tool_use" && terminalRaw != "true" {
		return sessionID, "", owner
	}
	return sessionID, stop, owner
}

// composerApplyLeaseStop extends a lease across a tool pause and releases it
// only after a proven terminal stop. A response that disappears without a
// terminal is ambiguous: the bridge run may still be alive, so the lease is
// deliberately left for its bounded stale-TTL self-heal.
func composerApplyLeaseStop(tenant, sessionID, leaseStop string, leaseOwner uint64) {
	switch {
	case leaseStop == "tool_use":
		composerInflight.touch(tenant, sessionID, leaseOwner)
	case leaseStop != "":
		composerInflight.release(tenant, sessionID, leaseOwner)
	}
}

// composerDebugLogAdvertisedTools logs advertised tool names when debug is enabled (stream path).
func composerDebugLogAdvertisedTools(responseID string, advertise []map[string]any) {
	if !composerDebugEnabled {
		return
	}
	names := make([]string, 0, len(advertise))
	noSchema := 0
	for _, a := range advertise {
		n, _ := a["name"].(string)
		names = append(names, n)
		if s, ok := a["inputSchema"]; !ok || s == nil {
			noSchema++
		}
	}
	composerDebugf("[composer %s] STREAM advertised %d tools (%d missing schema): %s", responseID, len(names), noSchema, strings.Join(names, ","))
}

// composerInboundTurn holds validated per-turn state shared by executeComposer and executeComposerStream.
type composerInboundTurn struct {
	model               string
	responseID          string
	tenant              string
	contHint            composerContinuationHint
	oai                 []byte
	defs                []cursorToolDefinition
	toolAliases         map[string]string
	sessionID           string
	leaseOwner          uint64
	continuation        bool
	utility             bool
	clientEnv           map[string]any
	input               map[string]any
	conversationBinding string
}

func (e *CursorExecutor) prepareComposerInbound(auth *cliproxyauth.Auth, apiKey string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (composerInboundTurn, error) {
	var turn composerInboundTurn
	var err error
	turn.clientEnv = validatedComposerClientEnv(opts)
	turn.model = resolveCursorModelName(resolveCursorModelAlias(auth, req.Model))
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	sourcePayload := bytes.Clone(req.Payload)
	if from == sdktranslator.FormatClaude {
		sourcePayload = helps.NormalizeClaudeConsecutiveTurns(sourcePayload)
	}
	turn.oai = sdktranslator.TranslateRequest(from, to, req.Model, sourcePayload, stream)
	turn.oai = composerForceStoreTrue(turn.oai)
	turn.tenant = composerTenant(auth, opts)
	// Identity conflicts outrank provenance clarification. Otherwise an ambiguous request with stale /new
	// headers can receive a signed 422 token under the wrong conversation before reaching the typed 409.
	if _, identityErr := resolveComposerConversationIdentity(opts); identityErr != nil {
		return turn, identityErr
	}
	turn.contHint = composerContinuationHintFor(turn.tenant, turn.oai)
	messages := gjson.GetBytes(turn.oai, "messages").Array()
	_, _, turn.continuation = composerToolResultsHinted(messages, turn.contHint)
	if rewritten, cerr := resolveComposerProvenance(turn.tenant, turn.oai, opts, len(composerToolDefs(turn.oai)) > 0); cerr != nil {
		return turn, cerr
	} else if rewritten != nil {
		turn.oai = rewritten
		messages = gjson.GetBytes(turn.oai, "messages").Array()
		// Provenance normalization can drop or rewrite untrusted tool-result shapes. Classification must use the
		// payload actually dispatched; retaining the pre-rewrite value can send a fresh user turn to /continue.
		turn.contHint = composerContinuationHintFor(turn.tenant, turn.oai)
		_, _, turn.continuation = composerToolResultsHinted(messages, turn.contHint)
	}
	if lastUserTurnImageOnlyInvalid(messages, turn.contHint) {
		return turn, errComposerImageOnlyInvalid
	}
	turn.defs = composerToolDefs(turn.oai)
	turn.utility = isComposerUtilityOneShot(turn.oai, opts, turn.contHint)
	turn.toolAliases = composerToolAliases(auth)
	turn.input = composerInputHinted(turn.oai, turn.contHint)
	conversationKey, identityErr := composerConversationKey(opts, messages)
	if identityErr != nil {
		return turn, identityErr
	}
	turn.conversationBinding = composerConversationBinding(turn.tenant, apiKey, conversationKey)
	if turn.conversationBinding != "" {
		turn.input["conversationBinding"] = turn.conversationBinding
	}
	invocationID := composerInvocationID(turn.oai, opts)
	if invocationID != "" {
		turn.input["invocationId"] = invocationID
	}
	leaseRequestID := invocationID
	if leaseRequestID == "" {
		// Stock clients may not provide an invocation id. The versioned ccm2 id
		// is still stable for an exact transport retry and is the bridge's own
		// compatibility identity. Use it for lease reattachment so retrying a
		// disconnected request cannot fork and execute twice. Independently
		// initiated byte-identical calls remain inherently ambiguous unless the
		// client supplies Idempotency-Key/X-Client-Turn-ID/metadata.turn_id.
		if clientMessageID, ok := turn.input["clientMessageId"].(string); ok && strings.HasPrefix(clientMessageID, "ccm2_") {
			leaseRequestID = strings.TrimSpace(clientMessageID)
		}
	}
	turn.sessionID, turn.leaseOwner, err = deriveComposerSessionIDLive(auth, apiKey, turn.oai, opts, leaseRequestID)
	if err != nil {
		return turn, err
	}
	turn.responseID = composerResponseIDBound(apiKey, turn.tenant, turn.sessionID, turn.conversationBinding)
	return turn, nil
}

// composerAgentTurnDial selects the bridge endpoint from the translated input. Tool results are forwarded
// unchanged to /agent/continue; Go never decodes their signed routing ids or owns their round state.
func (e *CursorExecutor) composerAgentTurnDial(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	apiKey string,
	body []byte,
) (*http.Response, error) {
	// Continuations route by their opaque, signed tool-call ids at the bridge. Go deliberately does not decode,
	// own, adopt, or disambiguate those ids.
	var httpResp *http.Response
	var err error
	if gjson.GetBytes(body, "input.type").String() == "tool_results" {
		httpResp, err = e.postAgentContinue(ctx, auth, apiKey, body)
	} else {
		httpResp, err = e.postAgentTurn(ctx, auth, apiKey, body)
	}
	if err != nil {
		return nil, err
	}
	return httpResp, nil
}

// reconnectComposerBridgeResponse closes one broken local bridge stream and
// reissues the exact body with the same selected credential. The bridge's
// durable invocation identity decides whether to hand off the live run or
// replay its committed receipt; AuthManager is deliberately not re-entered, so
// recovery cannot rotate to another key/session and execute the turn twice.
func (e *CursorExecutor) reconnectComposerBridgeResponse(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	apiKey string,
	body []byte,
	responseID string,
	previous *http.Response,
	stream bool,
) (*http.Response, error) {
	if previous != nil && previous.Body != nil {
		if errClose := previous.Body.Close(); errClose != nil {
			composerDebugf("[composer %s] close broken bridge response before reconnect: %v", responseID, errClose)
		}
	}
	next, err := e.composerAgentTurnDial(ctx, auth, apiKey, body)
	if err != nil {
		return nil, newComposerBridgeUnavailableError(responseID, err)
	}
	if err = composerValidateAgentTurnPreStream(next, responseID, stream, true); err != nil {
		return nil, err
	}
	return next, nil
}

// composerValidateAgentTurnPreStream rejects non-2xx and non-SSE bridge responses before SSE scanning.
// When closeBody is true the response body is closed on failure (streaming path).
func composerValidateAgentTurnPreStream(resp *http.Response, responseID string, stream, closeBody bool) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, composerBridgeMaxErrorBodyBytes))
		if closeBody {
			_ = resp.Body.Close()
		}
		retryAfter := parseComposerRetryAfterHeader(resp.Header, time.Now())
		if retryAfter == nil {
			retryAfter = parseComposerRetryAfterBody(errBody)
		}
		corr := composerCorrelationID()
		if stream {
			log.Errorf("[composer %s] STREAM bridge NON-2xx corr=%s status=%d body=%s", responseID, corr, resp.StatusCode, sanitizeBridgeBody(errBody))
		} else {
			log.Errorf("[composer %s] bridge NON-2xx corr=%s status=%d body=%s", responseID, corr, resp.StatusCode, sanitizeBridgeBody(errBody))
		}
		requestedBytes, availableBytes, priority := parseComposerCapacityMetadata(errBody)
		return &composerBridgeStatusError{
			status:         resp.StatusCode,
			correlation:    corr,
			retryAfter:     retryAfter,
			bridgeCode:     parseComposerBridgeErrorCode(errBody),
			requestedBytes: requestedBytes,
			availableBytes: availableBytes,
			priority:       priority,
		}
	}
	if !composerResponseIsSSE(resp) {
		ctHdr := resp.Header.Get("Content-Type")
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, composerBridgeMaxErrorBodyBytes))
		if closeBody {
			_ = resp.Body.Close()
		}
		return newComposerBridgeProtocolError(responseID, "non-SSE 2xx response", "content-type="+ctHdr+" body="+string(errBody))
	}
	return nil
}

func (e *CursorExecutor) executeComposerStream(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	turn, err := e.prepareComposerInbound(auth, apiKey, req, opts, true)
	if err != nil {
		if errors.Is(err, errComposerImageOnlyInvalid) {
			log.Errorf("[composer %s] STREAM image-only turn has no valid image -> 400", turn.responseID)
			return nil, err
		}
		if gjson.GetBytes(turn.oai, "store").Exists() && !gjson.GetBytes(turn.oai, "store").Bool() {
			log.Errorf("[composer %s] STREAM store:false unsupported -> 400", turn.responseID)
			return nil, err
		}
		reporter := helps.NewUsageReporter(ctx, e.Identifier(), turn.model, auth)
		reporter.PublishFailure(ctx, err)
		reporter.EnsurePublished(ctx)
		log.Errorf("[composer %s] STREAM deriveSessionID ERROR (-> 500): %v", turn.responseID, err)
		return nil, err
	}
	model := turn.model
	responseID := turn.responseID
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), model, auth)
	oai := turn.oai
	tenant := turn.tenant
	defs := turn.defs
	toolAliases := turn.toolAliases
	sessionID := turn.sessionID
	leaseOwner := turn.leaseOwner
	leaseSessionID := sessionID
	continuation := turn.continuation
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	// H16/#21 (C-RESPID): the outward-response-id -> sessionID mapping is recorded AFTER the bridge accepts a
	// valid SSE stream (below), NOT here — a dispatch that fails (transport / non-2xx / non-SSE) must not leave a
	// phantom mapping that a later previous_response_id would then resume onto a session that never ran.
	prep := prepareComposerAdvertise(oai, defs, toolAliases)
	advertise, toolChoice, constraints := prep.advertise, prep.toolChoice, prep.constraints
	if turn.utility {
		constraints["utilityOneShot"] = true
	}
	// ADD-65: build input with the SAME tenant continuation hint deriveComposerSessionID used, so a Responses
	// server-side-chained [..., tool, user] turn is sent as tool_results (with userText) — not a fresh user turn
	// behind a paused run.
	inp := turn.input
	liveStream := cliproxyexecutor.HasCapability(opts.Headers, cliproxyexecutor.CapabilityStreamResumeV1)
	if liveStream {
		if inp == nil {
			inp = map[string]any{}
		}
		inp["streamResume"] = true
		inp["capabilities"] = cliproxyexecutor.CapabilityStreamResumeV1
	}
	body := composerTurnBody(sessionID, model, inp, advertise, toolChoice, turn.clientEnv, constraints, leaseOwner)

	composerDebugf("[composer %s] STREAM sessionID=%s inputType=%v toolChoice=%q advertise=%d -> POST /agent/turn", responseID, sessionID, inp["type"], toolChoice, len(advertise))
	composerDebugLogAdvertisedTools(responseID, advertise)

	httpResp, err := e.composerAgentTurnDial(ctx, auth, apiKey, body)
	if err != nil {
		unavailErr := newComposerBridgeUnavailableError(responseID, err)
		reporter.PublishFailure(ctx, unavailErr)
		reporter.EnsurePublished(ctx)
		composerInflight.release(tenant, sessionID, leaseOwner)
		return nil, unavailErr
	}
	if err := composerValidateAgentTurnPreStream(httpResp, responseID, true, true); err != nil {
		reporter.PublishFailure(ctx, err)
		reporter.EnsurePublished(ctx)
		composerInflight.release(tenant, sessionID, leaseOwner)
		return nil, err
	}
	effectiveBinding, bindingErr := composerEffectiveContinuationBinding(
		httpResp, turn.conversationBinding, continuation,
	)
	if bindingErr != nil {
		_ = httpResp.Body.Close()
		protocolErr := newComposerBridgeProtocolError(responseID,
			"invalid effective conversation binding", bindingErr.Error())
		reporter.PublishFailure(ctx, protocolErr)
		reporter.EnsurePublished(ctx)
		composerInflight.release(tenant, sessionID, leaseOwner)
		return nil, protocolErr
	}
	if effectiveSessionID := composerEffectiveContinuationSession(httpResp, sessionID, continuation); effectiveSessionID != sessionID {
		composerDebugf("[composer %s] continuation effective session corrected advisory=%s routed=%s", responseID, sessionID, effectiveSessionID)
		sessionID = effectiveSessionID
		leaseSessionID = effectiveSessionID
	}
	if effectiveBinding != turn.conversationBinding || sessionID != turn.sessionID {
		responseID = composerResponseIDBound(apiKey, tenant, sessionID, effectiveBinding)
	}

	// #21 (C-RESPID): the bridge has now accepted a valid SSE stream, so record outward-response-id -> sessionID
	// (a later previous_response_id follow-up resumes THIS durable session). Recording only after acceptance
	// guarantees a failed dispatch left no phantom mapping. Scoped by tenant so it cannot leak across users.
	recordComposerResponseSession(tenant, responseID, sessionID)
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		// leaseStop is the stop_reason of the terminal turn_end this turn observed ("" if none — a disconnect or
		// truncated stream). The deferred handler frees the session's logical-run lease on a TERMINAL end
		// (end_turn/error) so the session can be reused, TOUCHes it on a tool_use pause so a concurrent sibling
		// still sees it held and forks (ISOLATION across the tool loop), and on no terminal leaves it for the TTL
		// to reclaim (the bridge run may still be alive — releasing could collapse a sibling onto it).
		leaseStop := ""
		// ACCURATE ACCOUNTING (invariant 4): the bridge / @cursor/sdk report NO token usage. Accumulate the
		// completion size and estimate usage at the terminal so the client sees real non-zero token counts
		// instead of 0. promptChars is the input estimate; realUsage flips true only if the bridge ever sends
		// parseable usage (a future SDK), so the estimate never double-counts.
		promptChars := composerPromptChars(oai)
		completionChars := 0
		lastLiveUsageChars := 0 // completionChars at the last live-usage estimate forwarded (throttle anchor)
		realUsage := false
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("cursor composer: close bridge response body error: %v", errClose)
			}
		}()
		defer reporter.EnsurePublished(ctx)
		defer func() {
			// No terminal, including a disconnect: leave the lease for TTL self-heal (see composerApplyLeaseStop).
			composerApplyLeaseStop(tenant, leaseSessionID, leaseStop, leaseOwner)
		}()

		// CC's auto-compact reads message.usage.input_tokens; the openai->claude translator hard-codes it to 0, so
		// inject the prompt estimate into message_start or CC never auto-compacts a composer session, however full.
		composerInputEstimate := composerEstimateTokens(promptChars)
		// Hold all model-visible frames until the bridge has emitted a durable
		// terminal UNLESS the client advertised stream-resume-v1. Capable clients
		// receive live frames that the bridge has already journal-committed;
		// legacy clients keep the atomic full-response buffer. liveStream is
		// computed before the goroutine from opts.Headers.

		commitBuffer := make([][]byte, 0, 32)
		commitBufferBytes := 0
		commitBudgetBytes := 0
		var commitBufferErr error
		releaseCommitBuffer := func() {
			composerStreamCommitGlobalBudget.release(commitBudgetBytes)
			commitBudgetBytes = 0
			commitBuffer = nil
			commitBufferBytes = 0
		}
		defer releaseCommitBuffer()
		emit := func(srcChunks [][]byte) bool {
			for i := range srcChunks {
				payload := composerSetMessageStartInputTokens(srcChunks[i], composerInputEstimate)
				if liveStream {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: bytes.Clone(payload)}:
					case <-ctx.Done():
						return false
					}
					continue
				}
				if commitBufferBytes+len(payload) > composerStreamCommitMaxBytes {
					if commitBufferErr == nil {
						commitBufferErr = newComposerBridgeProtocolError(responseID, "translated stream exceeds the atomic commit buffer", "")
						reporter.PublishFailure(ctx, commitBufferErr)
						select {
						case out <- cliproxyexecutor.StreamChunk{Err: commitBufferErr}:
						case <-ctx.Done():
						}
					}
					return false
				}
				if !composerStreamCommitGlobalBudget.reserve(len(payload)) {
					if commitBufferErr == nil {
						corr := composerCorrelationID()
						commitBufferErr = &composerCommitCapacityError{correlation: corr}
						log.Warnf("[composer %s] global atomic commit budget exhausted corr=%s requestBytes=%d globalMax=%d", responseID, corr, commitBufferBytes, composerStreamCommitGlobalBudget.max)
						reporter.PublishFailure(ctx, commitBufferErr)
						select {
						case out <- cliproxyexecutor.StreamChunk{Err: commitBufferErr}:
						case <-ctx.Done():
						}
					}
					return false
				}
				commitBuffer = append(commitBuffer, bytes.Clone(payload))
				commitBufferBytes += len(payload)
				commitBudgetBytes += len(payload)
			}
			return true
		}
		emitKeepalive := func() bool {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: []byte(": keepalive\n\n")}:
				return true
			case <-ctx.Done():
				return false
			}
		}
		flushCommitted := func() bool {
			if liveStream {
				return commitBufferErr == nil
			}
			if commitBufferErr != nil {
				return false
			}
			for _, payload := range commitBuffer {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: payload}:
				case <-ctx.Done():
					return false
				}
			}
			releaseCommitBuffer()
			return true
		}

		var param any
		toolIdx := 0
		lastLeaseTouch := time.Now()
		reconnectAllowed := composerBridgeEstablishedReconnectAllowed(auth, body)
		reconnectMax := composerBridgeEstablishedReconnectMaxAttempts()
		reconnectAttempts := 0

	bridgeStream:
		for {
			scanner := bufio.NewScanner(httpResp.Body)
			scanner.Buffer(nil, composerSSEMaxLineBytes) // shared Node/Go SSE-frame contract
			sawTerminal := false                         // a turn_end was observed before EOF
			var retryErr error
			retryReason := ""

		scanStream:
			for scanner.Scan() {
				line := scanner.Bytes()
				if !bytes.HasPrefix(line, []byte("data: ")) {
					continue
				}
				payload := bytes.TrimSpace(line[len("data: "):])
				if string(payload) == "[DONE]" {
					break
				}
				// ADD-96 (RBT-013): validate the SSE frame is real JSON before parsing. A reverse proxy that
				// corrupts/splits an event into invalid JSON (while still passing [DONE]) must fail closed, not be
				// silently dropped — a dropped text/tool_call frame would otherwise truncate the answer as success.
				if !gjson.ValidBytes(payload) {
					protoErr := newComposerBridgeProtocolError(responseID, "invalid JSON in bridge SSE frame", string(payload))
					reporter.PublishFailure(ctx, protoErr)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: protoErr}:
					case <-ctx.Done():
					}
					return
				}
				ev := gjson.ParseBytes(payload)
				now := time.Now()
				if composerLeaseHeartbeatDue(lastLeaseTouch, now) {
					composerInflight.touch(tenant, sessionID, leaseOwner)
					lastLeaseTouch = now
				}
				var choice map[string]any
				switch ev.Get("type").String() {
				case "text":
					txt := ev.Get("delta").String()
					completionChars += len(txt) // ACCURATE ACCOUNTING: estimate completion tokens
					choice = map[string]any{"index": 0, "delta": map[string]any{"content": txt}}
				case "reasoning":
					rzn := ev.Get("delta").String()
					completionChars += len(rzn)
					choice = map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": rzn}}
				case "tool_call":
					rawName := ev.Get("name").String()
					name, args := mapComposerToolCall(rawName, ev.Get("input"), defs, toolAliases)
					completionChars += len(name) + len(args) // a tool call is generated output too
					composerDebugf("[composer %s] STREAM tool_call emitted by model: raw=%q -> mapped=%q id=%s", responseID, rawName, name, ev.Get("id").String())
					choice = map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []map[string]any{{
						"index": toolIdx, "id": ev.Get("id").String(), "type": "function",
						"function": map[string]any{"name": name, "arguments": args},
					}}}}
					toolIdx++
				case "turn_end":
					if composerBridgeTerminalReplaySafe(ev) {
						retryErr = composerBridgeTurnFailure(responseID, ev)
						retryReason = "retryable_terminal"
						break scanStream
					}
					// ADD-88/96 (RBT-012): a turn_end is the bridge's terminal frame. Observing one (even an error)
					// means the stream ended deliberately, not by a truncated/empty body — record it so a clean EOF
					// WITHOUT a terminal is rejected below instead of synthesizing a [DONE] over an empty turn.
					sawTerminal = true
					// ISOLATION: record the terminal stop_reason for the deferred lease handler — a tool_use pause
					// keeps the logical run alive (touch); any other terminal frees the session (release). Never
					// leave it "" once a turn_end is observed (that sentinel means "no terminal / disconnect").
					leaseStop = ev.Get("stop_reason").String()
					if leaseStop == "" {
						leaseStop = "end_turn"
					}
					if continuation {
						leaseSessionID, leaseStop, leaseOwner = composerContinuationLeaseStop(ev)
					}
					// Non-retryable bridge failures are authoritative terminals. Retryable
					// terminals took the exact same-auth reattachment path above.
					if ev.Get("stop_reason").String() == "error" {
						turnErr := composerBridgeTurnFailure(responseID, ev)
						reporter.PublishFailure(ctx, turnErr)
						select {
						case out <- cliproxyexecutor.StreamChunk{Err: turnErr}:
						case <-ctx.Done():
						}
						return
					}
					fr := "stop"
					if ev.Get("stop_reason").String() == "tool_use" {
						fr = "tool_calls"
					}
					choice = map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": fr}
					if usage := ev.Get("usage"); usage.Exists() {
						if detail, ok := helps.ParseOpenAIStreamUsage([]byte(`{"usage":` + usage.Raw + `}`)); ok {
							reporter.Publish(ctx, detail)
							realUsage = true // bridge supplied real usage -> do not also emit an estimate
						}
					}
				case "ping":
					// The response is intentionally uncommitted until a durable terminal.
					// A raw SSE comment keeps every streaming schema/proxy connection
					// alive without opening a model-visible response envelope.
					if !emitKeepalive() {
						return
					}
					continue
				default:
					// ADD-96 (RBT-013): an unknown event type is protocol DRIFT (e.g. a renamed tool_call), not a
					// no-op. Continue ONLY for known-benign telemetry (the session announcement; ping is handled
					// above); fail closed on anything else so a dropped tool/content frame is never masked as success.
					if composerEventIsBenignTelemetry(ev.Get("type").String()) {
						continue
					}
					protoErr := newComposerBridgeProtocolError(responseID, "unknown bridge SSE event type", string(payload))
					reporter.PublishFailure(ctx, protoErr)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: protoErr}:
					case <-ctx.Done():
					}
					return
				}
				chunkLine := oaiChunk(responseID, model, choice)
				srcChunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), oai, chunkLine, &param)
				if !emit(srcChunks) {
					return
				}
				// LIVE ACCOUNTING (opt-in, CURSOR_COMPOSER_LIVE_USAGE): forward a RUNNING token estimate as completion
				// text accumulates so the client's counter grows live instead of sitting at 0 until the terminal. Same
				// ~4-chars/token estimate as the terminal; throttled by completion-char growth; never when the bridge
				// supplied real usage; wire-only (the ledger still records the single authoritative estimate at the end).
				if composerLiveUsageEnabled && !realUsage && completionChars-lastLiveUsageChars >= composerLiveUsageStepChars {
					lastLiveUsageChars = completionChars
					if !emit(sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), oai, composerUsageChunk(responseID, model, composerEstimateTokens(promptChars), composerEstimateTokens(completionChars)), &param)) {
						return
					}
				}
			}
			if retryErr == nil {
				if errScan := scanner.Err(); errScan != nil {
					if ctx.Err() != nil {
						composerDebugf("[composer %s] STREAM ended on client disconnect (benign): %v", responseID, errScan)
						return
					}
					retryErr = fmt.Errorf("cursor composer: read bridge stream: %w", errScan)
					retryReason = "scanner_error"
				} else if !sawTerminal {
					retryReason = "missing_terminal"
				} else {
					break bridgeStream
				}
			}

			if reconnectAllowed && reconnectAttempts < reconnectMax && ctx.Err() == nil {
				nextResp, reconnectErr := e.reconnectComposerBridgeResponse(ctx, auth, apiKey, body, responseID, httpResp, true)
				if reconnectErr == nil {
					httpResp = nextResp
					reconnectAttempts++
					log.Infof("[composer %s] established bridge stream transparently reattached reason=%s attempt=%d", responseID, retryReason, reconnectAttempts)
					releaseCommitBuffer()
					commitBufferErr = nil
					param = nil
					toolIdx = 0
					completionChars = 0
					lastLiveUsageChars = 0
					realUsage = false
					lastLeaseTouch = time.Now()
					leaseStop = ""
					continue bridgeStream
				}
				retryErr = reconnectErr
				retryReason = "reconnect_failed"
			}

			if retryErr == nil {
				retryErr = newComposerBridgeProtocolError(responseID, "bridge stream ended without a terminal turn_end", "")
			}
			retryErr = composerConstrainToSelectedExecution(retryErr)
			log.Errorf("[composer %s] established bridge stream recovery exhausted reason=%s attempts=%d: %v", responseID, retryReason, reconnectAttempts, retryErr)
			reporter.PublishFailure(ctx, retryErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: retryErr}:
			case <-ctx.Done():
			}
			return
		}
		// ACCURATE ACCOUNTING: the bridge reported no real token usage (the SDK exposes none), so synthesize an
		// ESTIMATE from the prompt + streamed completion and forward it BEFORE the terminal — the translator
		// renders it into the schema's usage (e.g. Anthropic message_delta.usage), and it is also recorded in the
		// ledger — so a composer turn reports realistic non-zero tokens instead of 0. (No-op if real usage came.)
		if !realUsage {
			pt := composerEstimateTokens(promptChars)
			ct := composerEstimateTokens(completionChars)
			if pt > 0 || ct > 0 {
				if detail, ok := composerEstimatedUsageDetail(pt, ct); ok {
					// #22 / P1-1: @cursor/sdk exposes NO token usage, so this is an ESTIMATE (~4 chars/token),
					// adequate for UX but NOT billing-grade. detail.Estimated=true marks it so usage sinks can
					// classify or exclude it from authoritative metering (never use it for money movement).
					composerDebugf("[composer %s] publishing ESTIMATED usage (SDK exposes none; not billing-grade): prompt~%d completion~%d", responseID, pt, ct)
					reporter.Publish(ctx, detail)
				}
				if !emit(sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), oai, composerUsageChunk(responseID, model, pt, ct), &param)) {
					return
				}
			}
		}
		// Clean end of the bridge stream. The bridge's turn_end carries finish_reason but no usage, and its
		// [DONE] is consumed above, so the inbound schema's TERMINAL is not emitted yet: the OpenAI->Anthropic
		// translator sends message_delta+message_stop only on usage or [DONE], and Responses defers
		// response.completed to [DONE]. Forward a synthetic [DONE] through the SAME translator so every schema
		// gets a well-formed terminal (Anthropic message_stop / Responses response.completed). OpenAI is a
		// no-op here (its passthrough returns nothing for [DONE]; the OpenAI handler adds its own).
		termChunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), oai, []byte("data: [DONE]"), &param)
		if !emit(termChunks) {
			return
		}
		_ = flushCommitted()
		observeComposerCompleted(tenant, oai, opts, leaseStop)
	}()

	// ADD-80 (RBT-035): do NOT forward the bridge's transport headers (text/event-stream, Cache-Control, any
	// bridge/CDN/gateway headers) on the OUTBOUND stream. The per-schema STREAM handler already forces the
	// correct streaming Content-Type (text/event-stream) and WriteUpstreamHeaders never overwrites it, so we
	// return a minimal clean header set. stream-resume-v1 is advertised only for capable live clients.

	headers := composerStreamResponseHeaders()
	if liveStream {
		headers.Set(cliproxyexecutor.HeaderCLIProxyCapabilities, cliproxyexecutor.CapabilityStreamResumeV1)
	}
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil

}

// executeComposer drives one /agent/turn and accumulates the bridge stream into a
// single non-streaming response.
func (e *CursorExecutor) executeComposer(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// One response id for the whole turn: it is both the body id (resp["id"]) the client sees as response.id
	// AND the H16 (C-RESPID) map key. Minting it separately at body-build time would let the recorded key
	// drift from the client-visible id, breaking previous_response_id resume.
	turn, err := e.prepareComposerInbound(auth, apiKey, req, opts, false)
	if err != nil {
		if errors.Is(err, errComposerImageOnlyInvalid) {
			log.Errorf("[composer %s] image-only turn has no valid image -> 400", turn.responseID)
			return cliproxyexecutor.Response{}, err
		}
		if gjson.GetBytes(turn.oai, "store").Exists() && !gjson.GetBytes(turn.oai, "store").Bool() {
			log.Errorf("[composer %s] store:false unsupported -> 400", turn.responseID)
			return cliproxyexecutor.Response{}, err
		}
		reporter := helps.NewUsageReporter(ctx, e.Identifier(), turn.model, auth)
		reporter.PublishFailure(ctx, err)
		reporter.EnsurePublished(ctx)
		return cliproxyexecutor.Response{}, err
	}
	model := turn.model
	responseID := turn.responseID
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), model, auth)
	oai := turn.oai
	tenant := turn.tenant
	defs := turn.defs
	toolAliases := turn.toolAliases
	sessionID := turn.sessionID
	leaseOwner := turn.leaseOwner
	leaseSessionID := sessionID
	continuation := turn.continuation
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	// leaseStop mirrors the streaming path: free the session's logical-run lease on a terminal end, touch it on
	// a tool_use pause, and on no terminal ("") leave it for the TTL (the bridge run may still be alive). All
	// touch/release calls carry leaseOwner so only the claiming run can mutate the lease (P0-1: no clobber).
	leaseStop := ""
	defer func() { composerApplyLeaseStop(tenant, leaseSessionID, leaseStop, leaseOwner) }()
	// H16/#21 (C-RESPID): the outward-response-id -> sessionID mapping is recorded AFTER the bridge accepts a
	// valid SSE stream (below), not here — a failed dispatch must leave no phantom mapping (mirrors the stream path).
	prep := prepareComposerAdvertise(oai, defs, toolAliases)
	advertise, toolChoice, constraints := prep.advertise, prep.toolChoice, prep.constraints
	if turn.utility {
		constraints["utilityOneShot"] = true
	}
	// ADD-65: build input with the SAME tenant continuation hint deriveComposerSessionID used, so a Responses
	// server-side-chained [..., tool, user] turn is sent as tool_results (with userText) — not a fresh user turn
	// behind a paused run.
	inp := turn.input
	body := composerTurnBody(sessionID, model, inp, advertise, toolChoice, turn.clientEnv, constraints, leaseOwner)

	httpResp, err := e.composerAgentTurnDial(ctx, auth, apiKey, body)
	if err != nil {
		unavailErr := newComposerBridgeUnavailableError(responseID, err)
		reporter.PublishFailure(ctx, unavailErr)
		reporter.EnsurePublished(ctx)
		composerInflight.release(tenant, sessionID, leaseOwner)
		return cliproxyexecutor.Response{}, unavailErr
	}
	defer func() {
		if httpResp == nil {
			return
		}
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("cursor composer: close bridge response body error: %v", errClose)
		}
	}()
	if err := composerValidateAgentTurnPreStream(httpResp, responseID, false, false); err != nil {
		reporter.PublishFailure(ctx, err)
		reporter.EnsurePublished(ctx)
		composerInflight.release(tenant, sessionID, leaseOwner)
		return cliproxyexecutor.Response{}, err
	}
	effectiveBinding, bindingErr := composerEffectiveContinuationBinding(
		httpResp, turn.conversationBinding, continuation,
	)
	if bindingErr != nil {
		protocolErr := newComposerBridgeProtocolError(responseID,
			"invalid effective conversation binding", bindingErr.Error())
		reporter.PublishFailure(ctx, protocolErr)
		reporter.EnsurePublished(ctx)
		composerInflight.release(tenant, sessionID, leaseOwner)
		return cliproxyexecutor.Response{}, protocolErr
	}
	if effectiveSessionID := composerEffectiveContinuationSession(httpResp, sessionID, continuation); effectiveSessionID != sessionID {
		composerDebugf("[composer %s] continuation effective session corrected advisory=%s routed=%s", responseID, sessionID, effectiveSessionID)
		sessionID = effectiveSessionID
		leaseSessionID = effectiveSessionID
	}
	if effectiveBinding != turn.conversationBinding || sessionID != turn.sessionID {
		responseID = composerResponseIDBound(apiKey, tenant, sessionID, effectiveBinding)
	}

	// #21 (C-RESPID): the bridge accepted a valid SSE stream -> NOW record outward-response-id -> sessionID, so a
	// failed dispatch (handled above) never leaves a phantom mapping a later previous_response_id could resume.
	recordComposerResponseSession(tenant, responseID, sessionID)
	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := make([]map[string]any, 0)
	responseBytes := 0
	finish := "stop"
	usageRaw := ""
	lastLeaseTouch := time.Now()
	reconnectAllowed := composerBridgeEstablishedReconnectAllowed(auth, body)
	reconnectMax := composerBridgeEstablishedReconnectMaxAttempts()
	reconnectAttempts := 0

bridgeResponse:
	for {
		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, composerSSEMaxLineBytes) // shared Node/Go SSE-frame contract
		sawTerminal := false
		var retryErr error
		retryReason := ""

	scanResponse:
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data: "):])
			if string(payload) == "[DONE]" {
				break
			}
			// ADD-96 (RBT-013): fail closed on a non-JSON SSE frame rather than dropping it (a dropped text/tool
			// frame would truncate the answer as a clean success).
			if !gjson.ValidBytes(payload) {
				protoErr := newComposerBridgeProtocolError(responseID, "invalid JSON in bridge SSE frame", string(payload))
				reporter.PublishFailure(ctx, protoErr)
				return cliproxyexecutor.Response{}, protoErr
			}
			ev := gjson.ParseBytes(payload)
			now := time.Now()
			if composerLeaseHeartbeatDue(lastLeaseTouch, now) {
				composerInflight.touch(tenant, sessionID, leaseOwner)
				lastLeaseTouch = now
			}
			switch ev.Get("type").String() {
			case "text":
				delta := ev.Get("delta").String()
				if responseBytes+len(delta) > composerStreamCommitMaxBytes {
					protoErr := newComposerBridgeProtocolError(responseID, "non-stream response exceeds the bounded aggregate buffer", "")
					reporter.PublishFailure(ctx, protoErr)
					return cliproxyexecutor.Response{}, protoErr
				}
				responseBytes += len(delta)
				text.WriteString(delta)
			case "reasoning":
				delta := ev.Get("delta").String()
				if responseBytes+len(delta) > composerStreamCommitMaxBytes {
					protoErr := newComposerBridgeProtocolError(responseID, "non-stream response exceeds the bounded aggregate buffer", "")
					reporter.PublishFailure(ctx, protoErr)
					return cliproxyexecutor.Response{}, protoErr
				}
				responseBytes += len(delta)
				reasoning.WriteString(delta)
			case "tool_call":
				name, args := mapComposerToolCall(ev.Get("name").String(), ev.Get("input"), defs, toolAliases)
				toolBytes := len(ev.Get("id").String()) + len(name) + len(args)
				if responseBytes+toolBytes > composerStreamCommitMaxBytes {
					protoErr := newComposerBridgeProtocolError(responseID, "non-stream response exceeds the bounded aggregate buffer", "")
					reporter.PublishFailure(ctx, protoErr)
					return cliproxyexecutor.Response{}, protoErr
				}
				responseBytes += toolBytes
				toolCalls = append(toolCalls, map[string]any{
					"id": ev.Get("id").String(), "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
			case "turn_end":
				if composerBridgeTerminalReplaySafe(ev) {
					retryErr = composerBridgeTurnFailure(responseID, ev)
					retryReason = "retryable_terminal"
					break scanResponse
				}
				sawTerminal = true // ADD-88/96 (RBT-012): terminal observed — EOF below is a clean end, not truncation
				// ISOLATION: record the terminal stop_reason for the deferred lease handler (tool_use pause -> touch;
				// any other terminal -> release). Never leave it "" once a turn_end is observed.
				leaseStop = ev.Get("stop_reason").String()
				if leaseStop == "" {
					leaseStop = "end_turn"
				}
				if continuation {
					leaseSessionID, leaseStop, leaseOwner = composerContinuationLeaseStop(ev)
				}
				switch ev.Get("stop_reason").String() {
				case "tool_use":
					finish = "tool_calls"
				case "error":
					turnErr := composerBridgeTurnFailure(responseID, ev)
					reporter.PublishFailure(ctx, turnErr)
					return cliproxyexecutor.Response{}, turnErr
				}
				if usage := ev.Get("usage"); usage.Exists() {
					usageRaw = usage.Raw
				}
			case "ping", "session":
				// ADD-96: known-benign telemetry — ignore (no content). Any OTHER unknown type fails closed below.
			default:
				protoErr := newComposerBridgeProtocolError(responseID, "unknown bridge SSE event type", string(payload))
				reporter.PublishFailure(ctx, protoErr)
				return cliproxyexecutor.Response{}, protoErr
			}
		}
		if retryErr == nil {
			if errScan := scanner.Err(); errScan != nil {
				retryErr = fmt.Errorf("cursor composer: read bridge stream: %w", errScan)
				retryReason = "scanner_error"
			} else if !sawTerminal {
				retryReason = "missing_terminal"
			} else {
				break bridgeResponse
			}
		}

		if reconnectAllowed && reconnectAttempts < reconnectMax && ctx.Err() == nil {
			nextResp, reconnectErr := e.reconnectComposerBridgeResponse(ctx, auth, apiKey, body, responseID, httpResp, false)
			if reconnectErr == nil {
				httpResp = nextResp
				reconnectAttempts++
				log.Infof("[composer %s] established bridge response transparently reattached reason=%s attempt=%d", responseID, retryReason, reconnectAttempts)
				text.Reset()
				reasoning.Reset()
				toolCalls = toolCalls[:0]
				responseBytes = 0
				finish = "stop"
				usageRaw = ""
				lastLeaseTouch = time.Now()
				leaseStop = ""
				continue bridgeResponse
			}
			retryErr = reconnectErr
			retryReason = "reconnect_failed"
		}

		if retryErr == nil {
			retryErr = newComposerBridgeProtocolError(responseID, "bridge stream ended without a terminal turn_end", "")
		}
		retryErr = composerConstrainToSelectedExecution(retryErr)
		log.Errorf("[composer %s] established bridge response recovery exhausted reason=%s attempts=%d: %v", responseID, retryReason, reconnectAttempts, retryErr)
		reporter.PublishFailure(ctx, retryErr)
		reporter.EnsurePublished(ctx)
		return cliproxyexecutor.Response{}, retryErr
	}

	message := map[string]any{"role": "assistant", "content": text.String()}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text.Len() == 0 {
			message["content"] = nil
		}
		finish = "tool_calls"
	}
	resp := map[string]any{
		"id": responseID, "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
	}
	// Carry the bridge's usage into the response AND publish it (parity with the streaming path). Only
	// embed it when it parses (same gjson.ValidBytes guard the stream path's ParseOpenAIStreamUsage uses):
	// a malformed usage fragment must be dropped, never spliced raw into the body (json.Marshal would then
	// fail and a discarded error would yield an empty 200 that loses all text + tool_calls).
	realUsage := false
	if usageRaw != "" {
		if detail, ok := helps.ParseOpenAIStreamUsage([]byte(`{"usage":` + usageRaw + `}`)); ok {
			resp["usage"] = json.RawMessage(usageRaw)
			reporter.Publish(ctx, detail)
			realUsage = true
		}
	}
	if !realUsage {
		// ACCURATE ACCOUNTING: the bridge reported no real usage (the SDK exposes none) -> estimate from the
		// prompt + completion (text + reasoning + tool-call output) so the response carries non-zero tokens.
		tcChars := 0
		for _, tc := range toolCalls {
			if fn, ok := tc["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					tcChars += len(name)
				}
				if args, ok := fn["arguments"].(string); ok {
					tcChars += len(args)
				}
			}
		}
		pt := composerEstimateTokens(composerPromptChars(oai))
		ct := composerEstimateTokens(text.Len() + reasoning.Len() + tcChars)
		if pt > 0 || ct > 0 {
			resp["usage"] = map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
			if detail, ok := composerEstimatedUsageDetail(pt, ct); ok {
				// #22 / P1-1: ESTIMATE only (the SDK exposes no usage) — adequate for UX, NOT billing-grade;
				// detail.Estimated=true so sinks can classify it as non-authoritative.
				composerDebugf("[composer %s] publishing ESTIMATED usage (SDK exposes none; not billing-grade): prompt~%d completion~%d", responseID, pt, ct)
				reporter.Publish(ctx, detail)
			}
		}
	}
	reporter.EnsurePublished(ctx)
	openaiResp, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		reporter.PublishFailure(ctx, errMarshal)
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor composer: marshal response: %w", errMarshal)
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), oai, openaiResp, &param)
	// ADD-80 (RBT-035): the body is the caller's JSON, not the bridge's SSE — never forward the bridge's
	// text/event-stream/Cache-Control headers (a strict SDK would mis-parse JSON as SSE). Emit a clean
	// schema-appropriate Content-Type instead of httpResp.Header.Clone().
	observeComposerCompleted(tenant, turn.oai, opts, leaseStop)
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: composerJSONResponseHeaders(from)}, nil
}

// composerSecretBodyPattern matches secret-ish tokens that may appear in a bridge/SDK error body (M25):
// Cursor keys (crsr_...), OpenAI-style keys (sk-...), Bearer headers, signed-URL query params, and the
// bridge auth token. It is intentionally broad — a redacted diagnostic is always preferable to leaking a
// live credential into logs or a client-visible 500.
var composerSecretBodyPattern = regexp.MustCompile(
	`(?i)(crsr_[a-z0-9_\-]+|sk-[a-z0-9_\-]{8,}|bearer\s+[a-z0-9._\-]+|x-bridge-auth["']?\s*[:=]\s*["']?[a-z0-9._\-]+|(?:signature|sig|token|x-amz-[a-z0-9\-]+|x-goog-[a-z0-9\-]+|key|auth_token)=[^&\s"']+)`,
)

// redactBridgeURL strips credential-bearing parts of a bridge URL before logging (M25): userinfo
// (user:pass@) and secret query params (key/auth_token/token/sig/signature). The host+path remain so a
// diagnostic still identifies which endpoint failed. On a parse failure it returns a coarse "[redacted-url]"
// rather than risk emitting raw credentials.
func redactBridgeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[redacted-url]"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	if q := u.Query(); len(q) > 0 {
		for _, k := range []string{"key", "auth_token", "token", "sig", "signature", "access_token"} {
			if q.Has(k) {
				q.Set(k, "redacted")
			}
		}
		u.RawQuery = q.Encode()
	}
	return u.String()
}

// sanitizeBridgeBody redacts secret-ish substrings from a bridge/SDK error body and bounds its length
// (M25), so neither a log line nor a client-visible error leaks a credential or a huge payload.
func sanitizeBridgeBody(b []byte) string {
	const maxBytes = 2048
	s := composerSecretBodyPattern.ReplaceAllString(string(b), "[redacted]")
	if len(s) > maxBytes {
		s = s[:maxBytes] + "…(truncated)"
	}
	return s
}

// composerCorrelationID returns a short opaque id to tie a client-visible generic error to a redacted
// server-side diagnostic (M25): the client sees only the id, the operator finds the full (redacted) detail
// in the logs under the same id.
func composerCorrelationID() string {
	return composerRandHex(8)
}

type composerAdmissionGate struct {
	mu     sync.Mutex
	states map[string]*composerAdmissionState
	nowFn  func() time.Time
}

type composerAdmissionState struct {
	active    int
	queued    int
	lastStart time.Time
}

type composerAdmissionLease struct {
	g        *composerAdmissionGate
	key      string
	released bool
}

type composerAdmissionError struct {
	retryAfter time.Duration
}

func (e *composerAdmissionError) Error() string {
	return fmt.Sprintf("cursor composer: local admission queue is full; retry in ~%ds", composerRetryAfterSeconds(e.retryAfter))
}

func (e *composerAdmissionError) StatusCode() int { return http.StatusTooManyRequests }

func (e *composerAdmissionError) RetryAfter() *time.Duration { return &e.retryAfter }

var composerAdmission = newComposerAdmissionGate()

func newComposerAdmissionGate() *composerAdmissionGate {
	return &composerAdmissionGate{states: make(map[string]*composerAdmissionState), nowFn: time.Now}
}

func composerEnvInt(name string, def, min int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min {
		log.Warnf("[composer] invalid %s=%q; using default %d", name, raw, def)
		return def
	}
	return n
}

func composerAdmissionConfig() (maxActive int, maxQueue int, minGap time.Duration) {
	maxActive = composerEnvInt("CURSOR_COMPOSER_ADMISSION_MAX_ACTIVE_PER_KEY", 2, 1)
	maxQueue = composerEnvInt("CURSOR_COMPOSER_ADMISSION_MAX_QUEUE_PER_KEY", 16, 0)
	minGap = time.Duration(composerEnvInt("CURSOR_COMPOSER_ADMISSION_MIN_GAP_MS", 1000, 0)) * time.Millisecond
	return maxActive, maxQueue, minGap
}

func composerAdmissionApplies(body []byte) bool {
	// The bridge owns the authoritative global, priority-aware admission queue.
	// Keeping a second default-on polling gate here can reorder recovery behind
	// fresh work and multiplies the queue bound per credential. Retain the old
	// per-key limiter only as an explicit operator compatibility switch.
	//
	// DEPRECATED (P5.6): two unreconciled admission authorities are a landmine.
	// This gate stays default-off; after one canary cycle of the bridge priority
	// queue it should be deleted outright rather than re-enabled.
	if strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_GO_ADMISSION")) != "1" {
		return false
	}
	return gjson.GetBytes(body, "input.type").String() != "tool_results"
}

func composerAdmissionKey(apiKey string) string {
	if fp := composerKeyFingerprint(apiKey); fp != "" {
		return fp
	}
	return "empty"
}

func composerAdmissionWaitLocked(st *composerAdmissionState, now time.Time, minGap time.Duration) time.Duration {
	wait := 250 * time.Millisecond
	if minGap > 0 && !st.lastStart.IsZero() {
		if gap := st.lastStart.Add(minGap).Sub(now); gap > wait {
			wait = gap
		}
	}
	return wait
}

func (g *composerAdmissionGate) acquire(ctx context.Context, apiKey string, body []byte) (*composerAdmissionLease, error) {
	if !composerAdmissionApplies(body) {
		return nil, nil
	}
	key := composerAdmissionKey(apiKey)
	queued := false
	for {
		maxActive, maxQueue, minGap := composerAdmissionConfig()
		g.mu.Lock()
		st := g.states[key]
		if st == nil {
			st = &composerAdmissionState{}
			g.states[key] = st
		}
		now := g.nowFn()
		gapOK := minGap <= 0 || st.lastStart.IsZero() || !now.Before(st.lastStart.Add(minGap))
		if st.active < maxActive && gapOK {
			if queued {
				st.queued--
				queued = false
			}
			st.active++
			st.lastStart = now
			active, queuedCount := st.active, st.queued
			g.mu.Unlock()
			composerDebugf("[composer] admission admitted key=%s active=%d queued=%d", key, active, queuedCount)
			return &composerAdmissionLease{g: g, key: key}, nil
		}
		if !queued {
			if st.queued >= maxQueue {
				retryAfter := composerAdmissionWaitLocked(st, now, minGap)
				g.mu.Unlock()
				return nil, &composerAdmissionError{retryAfter: retryAfter}
			}
			st.queued++
			queued = true
			active, queuedCount := st.active, st.queued
			composerDebugf("[composer] admission queued key=%s active=%d queued=%d", key, active, queuedCount)
		}
		wait := composerAdmissionWaitLocked(st, now, minGap)
		g.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if queued {
				g.mu.Lock()
				if st := g.states[key]; st != nil {
					st.queued--
					if st.active == 0 && st.queued == 0 {
						delete(g.states, key)
					}
				}
				g.mu.Unlock()
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *composerAdmissionLease) release() {
	if l == nil || l.g == nil || l.released {
		return
	}
	l.released = true
	l.g.mu.Lock()
	defer l.g.mu.Unlock()
	st := l.g.states[l.key]
	if st == nil {
		return
	}
	if st.active > 0 {
		st.active--
	}
	composerDebugf("[composer] admission released key=%s active=%d queued=%d", l.key, st.active, st.queued)
	if st.active == 0 && st.queued == 0 {
		delete(l.g.states, l.key)
	}
}

type composerAdmissionReadCloser struct {
	io.ReadCloser
	lease *composerAdmissionLease
}

func (rc *composerAdmissionReadCloser) Close() error {
	err := rc.ReadCloser.Close()
	rc.lease.release()
	return err
}

func composerAdmissionHTTPResponse(err *composerAdmissionError) *http.Response {
	retrySeconds := composerRetryAfterSeconds(err.retryAfter)
	body := []byte(fmt.Sprintf(`{"error":{"code":"local_admission_capacity","message":%q}}`, err.Error()))
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     http.StatusText(http.StatusTooManyRequests),
		Header: http.Header{
			"Content-Type": []string{"application/json"},
			"Retry-After":  []string{strconv.Itoa(retrySeconds)},
		},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
}

func composerRetryAfterSeconds(d time.Duration) int {
	if d <= 0 {
		return 1
	}
	seconds := int((d + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// postAgentTurn POSTs a turn body to the bridge's /agent/turn endpoint (SSE response).
func (e *CursorExecutor) postAgentTurn(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, body []byte) (*http.Response, error) {
	return e.postAgentBridge(ctx, auth, apiKey, body, composerAgentTurnPath)
}

func (e *CursorExecutor) postAgentContinue(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, body []byte) (*http.Response, error) {
	return e.postAgentBridge(ctx, auth, apiKey, body, composerAgentContinuePath)
}

func (e *CursorExecutor) postAgentBridge(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, body []byte, endpoint string) (*http.Response, error) {
	admissionLease, err := composerAdmission.acquire(ctx, apiKey, body)
	if err != nil {
		var admissionErr *composerAdmissionError
		if errors.As(err, &admissionErr) {
			return composerAdmissionHTTPResponse(admissionErr), nil
		}
		return nil, err
	}
	// ADD-41/ADD-47: validate + structurally build the /agent/turn URL (reject userinfo/query in the base,
	// require https for non-local hosts) BEFORE sending any credential. A bad/insecure config fails here
	// with a typed error instead of leaking the Cursor key over a cleartext or mis-joined URL.
	turnURL, err := buildComposerBridgeURL(auth, endpoint)
	if err != nil {
		admissionLease.release()
		corr := composerCorrelationID()
		log.Errorf("[composer] bridge request URL REJECTED corr=%s endpoint=%s base=%s: %v", corr, endpoint, redactBridgeURL(resolveComposerBridgeURL(auth)), err)
		return nil, err
	}
	// No timeout on the established data path (AGENTS.md): a tool round-trip to the
	// client can legitimately take minutes. The bridge keeps the upstream alive.
	var httpClient *http.Client
	if isLoopbackBridgeURL(turnURL) {
		// The bridge is local (default 127.0.0.1). NEVER route a loopback call through a configured OUTBOUND
		// proxy: it would break localhost routing (the proxy cannot reach our loopback) AND leak the Cursor key
		// (the Authorization bearer) to that proxy. Use a direct, proxy-free client. (Remote bridge URLs below
		// keep proxy-aware behavior, if a deployment intentionally fronts the bridge through a proxy.)
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = nil
		httpClient = &http.Client{Transport: tr}
	} else {
		httpClient = helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	}
	reconnectMax := composerBridgeReconnectMaxElapsed()
	// The supervised Railway topology runs the bridge on loopback. Remote bridge
	// URLs may have independent routing/proxy semantics, so automatic reconnect
	// there is opt-in rather than silently retaining requests against a bad URL.
	reconnect := composerBridgeReconnectEnabled(turnURL, body)
	started := time.Now()
	var httpResp *http.Response
	for attempt := 1; ; attempt++ {
		httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, turnURL, bytes.NewReader(body))
		if reqErr != nil {
			admissionLease.release()
			return nil, fmt.Errorf("cursor composer: failed to create %s request: %w", endpoint, reqErr)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		// Multi-tenant bridge: X-Bridge-Auth gates access and the bearer above is the per-user Cursor key.
		// Single-tenant (no token): omitted — the bearer doubles as the bridge gate (must equal CURSOR_API_KEY).
		if token := resolveComposerBridgeToken(auth); token != "" {
			httpReq.Header.Set("X-Bridge-Auth", token)
		}
		httpReq.Header.Set("Accept", "text/event-stream")

		httpResp, err = httpClient.Do(httpReq)
		if err == nil {
			if attempt > 1 {
				log.Infof("[composer] bridge request transparently reconnected endpoint=%s attempts=%d elapsed=%s", endpoint, attempt, time.Since(started).Round(time.Millisecond))
			}
			break
		}
		// M25: a transport error may echo its URL. Log only the redacted URL
		// and sanitized cause; never the bearer or request body.
		corr := composerCorrelationID()
		sanitized := sanitizeBridgeBody([]byte(err.Error()))
		if !reconnect || ctx.Err() != nil {
			admissionLease.release()
			log.Errorf("[composer] bridge request TRANSPORT ERROR corr=%s endpoint=%s to %s: %s", corr, endpoint, redactBridgeURL(turnURL), sanitized)
			helps.RecordAPIResponseError(ctx, e.cfg, err)
			return nil, fmt.Errorf("cursor composer: %s request failed (correlation %s)", endpoint, corr)
		}
		delay := composerBridgeReconnectDelay(attempt)
		if time.Since(started)+delay > reconnectMax {
			admissionLease.release()
			log.Errorf("[composer] bridge reconnect EXHAUSTED corr=%s endpoint=%s attempts=%d elapsed=%s to %s: %s", corr, endpoint, attempt, time.Since(started).Round(time.Millisecond), redactBridgeURL(turnURL), sanitized)
			helps.RecordAPIResponseError(ctx, e.cfg, err)
			return nil, fmt.Errorf("cursor composer: %s request failed after bridge reconnect window (correlation %s)", endpoint, corr)
		}
		log.Warnf("[composer] bridge reconnect pending corr=%s endpoint=%s attempt=%d retryIn=%s to %s: %s", corr, endpoint, attempt, delay, redactBridgeURL(turnURL), sanitized)
		if waitErr := waitComposerBridgeReconnect(ctx, delay); waitErr != nil {
			admissionLease.release()
			return nil, waitErr
		}
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if admissionLease != nil && httpResp.Body != nil {
		httpResp.Body = &composerAdmissionReadCloser{ReadCloser: httpResp.Body, lease: admissionLease}
	}
	return httpResp, nil
}
