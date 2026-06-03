package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
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
	defaultComposerBridgeURL = "http://127.0.0.1:9798"
	composerAgentTurnPath    = "/agent/turn"
)

// composerSSEMaxLineBytes bounds a single bridge SSE "data:" line for bufio.Scanner (ADD-68). The prior
// 52 MB bound tore down the whole stream/nonstream with bufio.ErrTooLong when Cursor emitted one large
// data: frame (a big tool-call argument, a large generated chunk, a JSON-encoded result/error). Raised to
// align with the bridge's 64 MB request-body cap (CURSOR_AGENT_MAX_AGENT_TURN_BYTES) so any frame the
// bridge would accept can also be read back — a BOUNDED byte limit, NOT a wall-clock timeout (AGENTS.md).
const composerSSEMaxLineBytes = 64 << 20

// composerBridgeMaxErrorBodyBytes bounds how much of a bridge non-2xx body is read before
// redaction/logging (ADD-46): a faulty or hostile bridge could return a multi-megabyte error page
// (stack trace, request echo, proxy error). The typed error returned to the client carries only a
// correlation id (never the body), so this bound is purely a memory guard on the diagnostic read.
const composerBridgeMaxErrorBodyBytes = 64 << 10

// composerBridgeStatusGone is the HTTP status the bridge returns for a lost tool-results continuation
// (the tool call this result answers was not issued by this bridge — restart/idle eviction/cap eviction).
// It is a REAL terminal error, distinct from a transient/retryable failure, and must reach the client
// with that meaning (ADD-59 / C-ADD59-TYPED-STATUS), never as an opaque 500 and never as a success.
const composerBridgeStatusGone = 410

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

// composerBridgeStatusError is a typed executor error that preserves the bridge's HTTP status so the
// conductor (sdk/cliproxy/auth/conductor.go reads cliproxyexecutor.StatusError) sets rerr.HTTPStatus and
// the API handler (sdk/api/handlers/handlers.go) maps it to the client response status — ADD-59 /
// C-ADD59-TYPED-STATUS. It carries ONLY a short generic message + a correlation id (the full, redacted
// body lives in the logs under the same corr), never the raw bridge body (M25). For a 410 specifically the
// message tells the client the tool-call continuation is gone and the turn must be re-seeded/restarted —
// distinguishable from "retry later". This mirrors the Codex precedent (classifyCodexStatusError).
type composerBridgeStatusError struct {
	status      int
	correlation string
}

func (e *composerBridgeStatusError) Error() string {
	if e.status == composerBridgeStatusGone {
		// 410: a LOST continuation. Be explicit that retrying the same tool result will not help — the
		// pending tool call is gone (bridge restart / idle or cap eviction); the turn must be restarted.
		return fmt.Sprintf("cursor composer: the tool-call this result answers is no longer active on the bridge "+
			"(session lost to a restart or eviction); re-seed/restart the turn rather than retrying (correlation %s)", e.correlation)
	}
	if e.status == http.StatusTooManyRequests {
		// 429: upstream rate-limit (Cursor HTTP/2 ENHANCE_YOUR_CALM, recycled connection + backoff) or proxy
		// capacity. Tell the client to back off — rapid retries re-trip the limit and prolong the outage.
		return fmt.Sprintf("cursor composer: upstream is rate-limiting this account or the proxy is at capacity; "+
			"back off and retry in a few seconds — rapid retries make it worse (correlation %s)", e.correlation)
	}
	return fmt.Sprintf("cursor composer: bridge /agent/turn failed with status %d (correlation %s)", e.status, e.correlation)
}

// StatusCode implements cliproxyexecutor.StatusError so the conductor/handler preserve the bridge status
// to the client (e.g. a 410 stays a 410, a 429 stays a 429) instead of collapsing every non-2xx to 500.
func (e *composerBridgeStatusError) StatusCode() int { return e.status }

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

// newComposerBridgeUnavailableError builds the typed transport error and emits one redacted diagnostic under the
// correlation id (the cause may carry the bridge URL/host, so it is sanitized via sanitizeBridgeBody).
func newComposerBridgeUnavailableError(responseID string, cause error) *composerBridgeUnavailableError {
	corr := composerBridgeLogTransport(responseID, cause)
	return &composerBridgeUnavailableError{correlation: corr, cause: cause}
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
	u.Path = path.Join("/", strings.TrimRight(u.Path, "/"), composerAgentTurnPath)
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

// Tool-call ownership store (ADD-50/ADD-70/ADD-71, extends Comment-5/M32). It records which bridge SESSION
// emitted each tool-call id so a continuation (tool_results) turn carrying no stable conversation id can be
// routed back to the right session by the id the client echoes.
//
// ADD-70: ownership is keyed by tenant + SESSION + tool-call id (not just tenant + id), because a tool-call
// id is only response/conversation-scoped — the same visible id (e.g. "call_tool_0") can recur across two of
// one tenant's sessions and the old tenant+id key let the second emitter silently OVERWRITE the first's
// routing. We additionally keep a tenant id -> set<session> index so a continuation that echoes an
// ambiguous id (owned by 2+ sessions) is REJECTED (errAmbiguousToolOwnership) rather than mis-routed to the
// latest writer. ADD-71: each TENANT has its own bounded LRU + cap, so a noisy tenant can only evict its OWN
// stale entries — never another tenant's pending continuation routing. ADD-50: re-emitting an id performs a
// true LRU touch (moves it to the tail) so an actively-reused id is not evicted by unrelated churn.
//
// TODO(ADD-39) / Comment 2 / RBT-017: this store is PROCESS-LOCAL. Horizontally-scaled CLIProxy replicas do
// not share it, so a continuation (tool_results) turn that lands on a DIFFERENT replica than the tool-call
// EMITTER will miss ownership here. That MISS DEGRADES SAFELY, NOT silently: deriveComposerSessionID falls back
// to the stable-conv hash (tenant + key-fingerprint + conversation id — deterministic across replicas, so a
// conversation that carries a stable id still routes to the SAME sess_ id on any replica) and finally to a
// fresh mint+re-seed; composerInputHinted carries history+system on the continuation so the bridge re-seeds
// the tool results rather than clean-acking. It NEVER mis-routes to a wrong session and NEVER returns a clean
// empty/end_turn success on a miss. The cross-replica REQUIREMENT for full durable-resume fidelity is therefore:
//
//	MULTI-REPLICA DEPLOYMENTS MUST PIN STICKY ROUTING BY (tenant, conversation) — i.e. all turns of one
//	conversation reach the same CLIProxy replica AND the same bridge. Without sticky routing a non-emitter
//	replica degrades to mint + history re-seed (correct but loses durable-agent continuity for that turn).
//
// A shared low-latency store (tenant/session/tool_call_id/response_id, with the SAME TTL/cap semantics as this
// local store) would remove the sticky-routing requirement; it is DEFERRED per the owner decision that deploy
// is single-instance (Railway). The tenant+session keying here is the correct local foundation for that future
// shared store. Do NOT claim cross-replica ownership is solved — it is single-instance-correct + documented.
var composerOwnership = newComposerToolCallStore()

const (
	// composerToolCallPerTenantCap bounds the number of owned tool-call ids retained PER TENANT (ADD-71).
	composerToolCallPerTenantCap = 20000
	// composerToolCallEntryTTL ages out an owned tool-call id even if the per-tenant cap is never reached
	// (ADD-71): a continuation realistically arrives within minutes of the tool call, so a far-older entry is
	// stale. The cap must not be the only cleanup. This is an in-memory housekeeping bound, not a data-path
	// wall-clock timeout (no established stream is gated by it) — AGENTS.md compliant.
	composerToolCallEntryTTL = 30 * time.Minute
)

// composerToolCallEntry is one owned tool-call id (its emitting session + last-touch time for LRU/TTL).
type composerToolCallEntry struct {
	sessionID  string
	toolCallID string
	lastUsed   time.Time
}

// composerTenantOwnership holds one tenant's owned tool-call ids: a primary map keyed by session+id, a
// id -> set<session> index for ambiguity detection, and an LRU order of the primary keys.
type composerTenantOwnership struct {
	byKey  map[string]*composerToolCallEntry // sessionID + "\x00" + toolCallID -> entry
	byTool map[string]map[string]struct{}    // toolCallID -> set<sessionID>
	order  []string                          // LRU order of byKey keys (front = oldest)
}

// composerToolCallStore is the tenant-partitioned ownership store. One mutex guards all tenants (the map is
// small and contention is low); each tenant has an independent LRU+cap so cross-tenant eviction is impossible.
type composerToolCallStore struct {
	mu       sync.Mutex
	tenants  map[string]*composerTenantOwnership
	nowFn    func() time.Time // injectable clock for tests
	perCap   int
	entryTTL time.Duration
}

func newComposerToolCallStore() *composerToolCallStore {
	return &composerToolCallStore{
		tenants:  make(map[string]*composerTenantOwnership),
		nowFn:    time.Now,
		perCap:   composerToolCallPerTenantCap,
		entryTTL: composerToolCallEntryTTL,
	}
}

func composerOwnershipKey(sessionID, toolCallID string) string {
	return sessionID + "\x00" + toolCallID
}

// record remembers that sessionID emitted toolCallID for tenant. ADD-50: an existing key is moved to the LRU
// tail (true touch) instead of left in place. ADD-70: the key includes the session, so the same id under a
// different session is a DISTINCT entry (and the id -> set index gains a second session, making lookups for
// that id ambiguous rather than silently overwritten).
func (s *composerToolCallStore) record(tenant, toolCallID, sessionID string) {
	if toolCallID == "" || sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	to := s.tenants[tenant]
	if to == nil {
		to = &composerTenantOwnership{byKey: map[string]*composerToolCallEntry{}, byTool: map[string]map[string]struct{}{}}
		s.tenants[tenant] = to
	}
	key := composerOwnershipKey(sessionID, toolCallID)
	now := s.nowFn()
	if e, ok := to.byKey[key]; ok {
		// ADD-50: true LRU touch — refresh time AND move to the tail so re-emission preserves the entry.
		e.lastUsed = now
		to.moveToTail(key)
		return
	}
	to.byKey[key] = &composerToolCallEntry{sessionID: sessionID, toolCallID: toolCallID, lastUsed: now}
	to.order = append(to.order, key)
	set := to.byTool[toolCallID]
	if set == nil {
		set = map[string]struct{}{}
		to.byTool[toolCallID] = set
	}
	set[sessionID] = struct{}{}
	// ADD-71: per-tenant cap eviction (front = oldest). Evict this tenant's own oldest entries only.
	for len(to.order) > s.perCap {
		oldest := to.order[0]
		to.order = to.order[1:]
		to.deleteKey(oldest)
	}
}

// moveToTail moves an existing LRU key to the tail (most-recently-used). Caller holds the lock.
func (to *composerTenantOwnership) moveToTail(key string) {
	for i, k := range to.order {
		if k == key {
			to.order = append(to.order[:i], to.order[i+1:]...)
			to.order = append(to.order, key)
			return
		}
	}
}

// deleteKey removes a byKey entry and its id -> set index membership. Caller holds the lock.
func (to *composerTenantOwnership) deleteKey(key string) {
	e, ok := to.byKey[key]
	if !ok {
		return
	}
	delete(to.byKey, key)
	if set := to.byTool[e.toolCallID]; set != nil {
		delete(set, e.sessionID)
		if len(set) == 0 {
			delete(to.byTool, e.toolCallID)
		}
	}
}

// expireLocked drops entries older than entryTTL for a tenant (ADD-71 age cleanup). Caller holds the lock.
func (s *composerToolCallStore) expireLocked(to *composerTenantOwnership) {
	if s.entryTTL <= 0 {
		return
	}
	cutoff := s.nowFn().Add(-s.entryTTL)
	kept := to.order[:0]
	for _, key := range to.order {
		e, ok := to.byKey[key]
		if !ok {
			continue
		}
		if e.lastUsed.Before(cutoff) {
			to.deleteKey(key)
			continue
		}
		kept = append(kept, key)
	}
	to.order = kept
}

// sessionsForTool returns the set of sessions that own toolCallID for tenant (after TTL expiry). Caller holds
// the lock.
func (s *composerToolCallStore) sessionsForTool(to *composerTenantOwnership, toolCallID string) []string {
	set := to.byTool[toolCallID]
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for sid := range set {
		out = append(out, sid)
	}
	return out
}

// forgetSession drops all of a tenant's ownership entries for one session (ADD-71: delete entries when a
// bridge session ends, so the cap is not the only cleanup). Best-effort: callers invoke it when a turn for the
// session ends with a terminal stop (no pending tool calls remain) — see executeComposer/Stream.
func (s *composerToolCallStore) forgetSession(tenant, sessionID string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	to := s.tenants[tenant]
	if to == nil {
		return
	}
	kept := to.order[:0]
	for _, key := range to.order {
		e, ok := to.byKey[key]
		if !ok {
			continue
		}
		if e.sessionID == sessionID {
			to.deleteKey(key)
			continue
		}
		kept = append(kept, key)
	}
	to.order = kept
	if len(to.byKey) == 0 {
		delete(s.tenants, tenant)
	}
}

// sessionHasTools reports whether sessionID still owns any non-expired tool-call entry for tenant. #19: a
// lineage whose session still owns undelivered tool calls is "live" and must not be dropped by the per-base cap.
func (s *composerToolCallStore) sessionHasTools(tenant, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	to := s.tenants[tenant]
	if to == nil {
		return false
	}
	s.expireLocked(to) // TTL-expire first so a stale id is not treated as still-owned (mirrors ownsTool)
	for _, e := range to.byKey {
		if e.sessionID == sessionID {
			return true
		}
	}
	return false
}

// ownsTool reports whether toolCallID is owned by ANY session for tenant (ADD-65 branch (c) signal). It does
// NOT mutate LRU order (a read-only ownership probe), only TTL-expires first so a stale id is not treated as
// owned.
func (s *composerToolCallStore) ownsTool(tenant, toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	to := s.tenants[tenant]
	if to == nil {
		return false
	}
	s.expireLocked(to)
	owned := len(to.byTool[toolCallID]) > 0
	if len(to.byKey) == 0 {
		delete(s.tenants, tenant)
	}
	return owned
}

// composerContinuationHintFor builds the ADD-65 continuation hint for a tenant + OpenAI-normalized body: the
// previous_response_id presence (server-side chaining) and a tenant-scoped tool-ownership probe. Used by every
// caller that has the body so branch (c) classification is CONSISTENT (composerInput, deriveComposerSessionID,
// lookupSessionByToolResults) — a mismatch would route a turn to a continuation session yet send it as fresh.
func composerContinuationHintFor(tenant string, oai []byte) composerContinuationHint {
	return composerContinuationHint{
		hasPreviousResponseID: strings.TrimSpace(gjson.GetBytes(oai, "previous_response_id").String()) != "",
		ownsToolCallID:        func(id string) bool { return composerOwnership.ownsTool(tenant, id) },
	}
}

// composerResponseSessions maps a tenant-scoped OUTWARD response id (the response.id the client sees on a
// turn) -> the bridge session that produced it. H16: a Responses/Codex follow-up that carries
// `previous_response_id` and no tools must resume the DURABLE agent, not get diverted to an ephemeral
// one-shot (utility) or hashed as a "stable conv id" (a previous_response_id changes every turn, so it is
// NOT stable). deriveComposerSessionID consults this map before any conv-id hash. Bounded (FIFO) so it
// cannot grow without limit. Key shape: tenant + "\x00resp:" + outwardResponseID (mirrors the tool-call map).
//
// Comment 2 / ADD-39 / RBT-017 (multi-replica): like composerOwnership this map is PROCESS-LOCAL. A
// previous_response_id follow-up that lands on a different replica than the one that emitted that response id
// MISSES here and falls through to the stable-conv hash / fresh mint (degrade-safe, never a wrong-session
// route). Full durable-resume fidelity across replicas requires the SAME sticky routing by (tenant,
// conversation) documented on composerOwnership; a shared store keyed additionally by response_id would lift
// it. Deferred per the single-instance (Railway) owner decision.
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

// lookupComposerResponseSession returns the session id recorded for a tenant-scoped outward response id, or
// "" when unknown (bridge restart / eviction / cross-instance) — in which case the caller degrades gracefully.
func lookupComposerResponseSession(tenant, outwardResponseID string) string {
	if outwardResponseID == "" {
		return ""
	}
	composerResponseSessionMu.Lock()
	defer composerResponseSessionMu.Unlock()
	return composerResponseSessions[tenant+"\x00resp:"+outwardResponseID]
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

// stableConversationID returns a stable caller/session identifier from the request headers, the inbound
// request's session metadata, or CLIProxy execution metadata — or "" when the caller provides none. It
// is NEVER derived from message text.
//
// The highest-value source is the inbound payload's metadata.user_id: Claude Code sends a per-conversation
// session id there (format user_<acct>_account__session_<uuid>, or a JSON object with session_id). That
// uuid is globally unique and stable across a conversation's turns, so keying on it both fixes
// content-collisions AND separates distinct users/conversations that happen to share one upstream Cursor
// key. The conversation/session headers are honored first for clients that do set them.
func stableConversationID(opts cliproxyexecutor.Options) string {
	if opts.Headers != nil {
		// Existing conversation/session headers first, then the additional real conv-id signals that
		// extractSessionIDs / copilot_headers honor (Codex Session_id, Amp thread id, a bare Conversation_id)
		// so a non-Anthropic agentic client keeps a stable session across its turns (EX5).
		//
		// ADD-48: X-Client-Request-Id is REMOVED from this list. By near-universal convention a "request id" is
		// a PER-REQUEST tracing id (unique every HTTP call), NOT a per-conversation id. Treating it as stable
		// minted a NEW bridge session every turn (durable agent / pending ownership / compaction all drift) — a
		// silent multi-turn regression. Clients that genuinely have a stable id set a conversation/session/thread
		// header above (Amp still routes via X-Amp-Thread-Id); a request-id-only turn now degrades gracefully to
		// mint + history re-seed, which is correct, instead of churning sessions. Re-add behind a proven-stable
		// per-client allowlist (with a test that the client's field is conversation-stable), never generically.
		for _, h := range []string{
			"X-Conversation-Id", "X-Session-Id", "X-Cc-Conversation-Id",
			"Session_id", "Conversation_id", "X-Amp-Thread-Id",
		} {
			if v := strings.TrimSpace(opts.Headers.Get(h)); v != "" {
				return v
			}
		}
	}
	if id := claudeSessionID(opts.OriginalRequest); id != "" {
		return id
	}
	// Body signals that mirror extractSessionIDs steps 7+: a conversation_id is stable across a conversation's
	// turns and never derived from message content. Never derive from message text. H16: previous_response_id
	// is DELIBERATELY EXCLUDED here — it is NOT stable across a conversation (it changes every turn), so
	// hashing it would mint a NEW session every turn and lose context. It is instead resolved via the
	// response-id->sessionID map in deriveComposerSessionID (recordComposerResponseSession).
	//
	// ADD-78: prompt_cache_key is DELIBERATELY EXCLUDED too. It is a cache-locality HINT, not a conversation
	// identity: clients reuse a single coarse cache key across SEPARATE tasks that merely share a system
	// prompt / repo context. Hashing it as the stable session preimage merged independent conversations onto
	// one durable Cursor agent (prior tool state / pending ownership / seeded system bleeds across unrelated
	// requests). A turn whose ONLY id is prompt_cache_key now degrades to mint + history re-seed instead.
	if len(opts.OriginalRequest) > 0 {
		for _, k := range []string{"conversation_id"} {
			if v := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, k).String()); v != "" {
				return v
			}
		}
	}
	if opts.Metadata != nil {
		// ADD-48: request_id/requestId are REMOVED here for the same reason as the header above — a CLIProxy
		// execution-metadata request id is per-call, not per-conversation; keying on it churned a fresh session
		// each turn. Only the explicit conversation/session metadata is conversation-stable.
		for _, k := range []string{"conversation_id", "conversationId", "session_id", "sessionId"} {
			if v, ok := opts.Metadata[k]; ok {
				if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
					return s
				}
			}
		}
	}
	return ""
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
// check on the tool_call_id lookup) is a SEPARATE sign-off decision and is NOT
// implemented here. Process-local registry only (single-instance/sticky; no
// distributed store), mirroring composerOwnership's cross-replica caveat.
const (
	// composerLineagePerTenantCap bounds the number of (baseSid,headDigest) lineages retained PER TENANT.
	// Mirrors composerToolCallPerTenantCap so a noisy tenant only evicts its own oldest lineages.
	composerLineagePerTenantCap = 20000
	// composerLineagePerBaseCap bounds the co-resident lineages under ONE baseSid, so a pathological fan-out
	// of forks under a single shared conversation id cannot exhaust the per-tenant cap on its own.
	composerLineagePerBaseCap = 64
	// composerLineageEntryTTL ages a lineage out even if neither cap is reached. Reuses the
	// composerToolCallEntryTTL 30m magnitude (a returning turn within minutes re-CONTINUEs; a far-older
	// lineage is stale). In-memory housekeeping bound, not a data-path wall-clock timeout (AGENTS.md).
	composerLineageEntryTTL = composerToolCallEntryTTL
)

// serverSecret keys the lineage head digest (HMAC) so a stored digest is a fixed-width, non-reversible value
// that never carries raw conversation text. It is NEVER logged.
//
// CONTINUITY across restart/replica (review Comment 2): when CURSOR_COMPOSER_LINEAGE_SECRET is set, the key is
// STABLE — the same (tenant, conversation, content) re-derives the same head digest and the same content-fork
// id after a process restart or on another replica, WITHOUT any shared/persisted routing state. When unset, the
// key is per-process crypto/rand: head digests + content-fork ids are deterministic only WITHIN one process
// (single-process sticky routing — the documented single-instance contract; multi-replica / cross-restart
// determinism REQUIRES setting the env secret). Either way the tenant boundary (composerTenant) is unchanged.
var serverSecret = func() []byte {
	secret := loadComposerLineageSecret(os.Getenv)
	if strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_LINEAGE_SECRET")) == "" {
		// P1-4: a per-process random lineage key means forked subagent conversations re-derive a DIFFERENT id
		// after a restart / on another replica and are reseeded (context-light) instead of re-attaching to their
		// durable agent. Warn at startup so cross-restart/replica continuity is a conscious operator choice
		// (parity with the bind-host warnings); set CURSOR_COMPOSER_LINEAGE_SECRET to a stable value to fix it.
		log.Warnf("[composer] CURSOR_COMPOSER_LINEAGE_SECRET is unset: conversation-fork routing uses a per-process random key — forked subagent conversations lose durable continuity across restarts/replicas (they reseed). Set a stable secret for cross-restart determinism.")
	}
	return secret
}()

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
	headDigest string    // growth-stable HMAC of the first 2 non-system messages
	priorHead  string    // one-behind headDigest, for retry/duplicate tolerance across a re-key
	openerFP   string    // fingerprint of the first non-system message (role + prefix) for the compaction signal
	slot       int       // recorded identical-head collision slot (0 = first claimant, omitted from the id)
	lastUsed   time.Time // LRU + TTL
}

// tenantLineage holds one tenant's lineages, keyed by baseSid + "\x00" + headDigest, with an LRU order of
// those keys. One mutex per tenant (mirrors composerTenantOwnership): contention is low and per-tenant
// isolation means a noisy tenant never evicts another's lineage.
type tenantLineage struct {
	mu         sync.Mutex
	byBaseHead map[string]*lineageEntry // baseSid + "\x00" + headDigest -> the co-resident lineage for THAT head
	order      []string                 // LRU of byBaseHead keys (front = oldest)
}

// composerLineageStore is the tenant-partitioned lineage registry. sync.Map mirrors the composerOwnership
// tenant partition shape; each tenant submap has its own mutex/LRU/caps.
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

// lineageHeadDigest is the growth-stable HMAC head used as a lineage key component (§1.2). It reuses the
// SAME head window as composerHistoryFingerprint (the first composerHistoryFingerprintHeadMessages non-system
// messages, role + a 256-rune text prefix each) so a fork's head is identical across its own turns. fp16 is
// the existing composerHistoryFingerprint (the compaction signal) folded into the keying so two conversations
// with the same opener but divergent bodies do not share a head; tenant is folded so the digest is
// tenant-scoped. crypto/hmac keyed by serverSecret keeps the stored digest a fixed-width, non-reversible value
// (the raw conversation text is never retained in the registry).
func lineageHeadDigest(tenant, fp16 string, messages []gjson.Result) string {
	mac := hmac.New(sha256.New, serverSecret)
	mac.Write([]byte(tenant))
	mac.Write([]byte{0})
	mac.Write([]byte(fp16))
	mac.Write([]byte{0})
	composerWriteFingerprintHead(mac, composerNonSystemHeadMessages(messages))
	return hex.EncodeToString(mac.Sum(nil))[:32]
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

// expireLocked drops lineages older than entryTTL for a tenant. Caller holds to.mu.
func (s *composerLineageStore) expireLocked(to *tenantLineage) {
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
		if e.lastUsed.Before(cutoff) {
			delete(to.byBaseHead, key)
			continue
		}
		kept = append(kept, key)
	}
	to.order = kept
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
// outstanding tool ownership) so a concurrent fork's continuity is never evicted just to make room. If EVERY
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

// tenantLineageFor returns the tenant submap, creating it on demand. Mirrors the composerOwnership partition.
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
// next slot with a RECORDED crypto/rand tie value folded only to disambiguate two byte-identical openings;
// the slot is recorded, not re-minted, so each clone stays head-stable across its own later turns. Returns
// the fork's stable sess_ id.
func (s *composerLineageStore) putForkSlotted(tenant, baseSid, headDigest string, collide bool) string {
	if tenant == "" {
		// Empty tenant never reaches here (deriveComposerSessionID mints first); return a deterministic id.
		return forkSessionID(baseSid, headDigest, 0)
	}
	to := s.tenantLineageFor(tenant)
	to.mu.Lock()
	defer to.mu.Unlock()
	s.expireLocked(to)
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
		// makes concurrency forks reproducible across restart/replica (with a shared lineage secret), and reuses
		// slots freed by eviction. The fork id still requires the server secret (folded into baseSid), so a
		// predictable slot does not make a fork id guessable.
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
		for len(to.order) > s.perCap {
			oldest := to.order[0]
			to.order = to.order[1:]
			delete(to.byBaseHead, oldest)
		}
		return forkSid
	}
	// First claimant of this head: slot 0, id omits the slot for back-compat.
	to.enforcePerBaseCapLocked(tenant, baseSid, s.perBase) // #19: evict only NON-live forks (else exceed softly)
	forkSid := forkSessionID(baseSid, headDigest, 0)
	e := &lineageEntry{sid: forkSid, headDigest: headDigest, lastUsed: now}
	to.byBaseHead[key] = e
	to.order = append(to.order, key)
	for len(to.order) > s.perCap {
		oldest := to.order[0]
		to.order = to.order[1:]
		delete(to.byBaseHead, oldest)
	}
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
	if tenant == "" {
		return baseSid
	}
	to := s.tenantLineageFor(tenant)
	to.mu.Lock()
	defer to.mu.Unlock()
	s.expireLocked(to)
	now := s.nowFn()
	prefix := baseSid + "\x00"

	// (a) exact head match (current or one-behind), co-resident under this baseSid.
	key := lineageKey(baseSid, headDigest)
	if e, ok := to.byBaseHead[key]; ok {
		e.lastUsed = now
		to.moveToTail(key)
		return e.sid
	}
	for _, k := range to.order {
		e := to.byBaseHead[k]
		if e != nil && e.priorHead == headDigest && strings.HasPrefix(k, prefix) {
			e.lastUsed = now
			to.moveToTail(k)
			return e.sid
		}
	}

	// (b) opener bridge: find a co-resident lineage whose opener matches but whose head moved
	// (compactionSignal) — the same conversation/fork with a rewritten (compact) or grown (turn-1→turn-3) body.
	// #11 (review): a SINGLE such match is unambiguous and is re-keyed. But when MORE THAN ONE co-resident
	// lineage shares the opener (a base AND its concurrency-fork, or several forks), the opener alone cannot say
	// which one this turn continues — the old "prefer base" tie-break would re-key the BASE onto a fork's new
	// head (corrupting the parent) or pull a fork back to base. Treat >1 as AMBIGUOUS and do NOT bridge: fall
	// through to (c) and isolate this turn, rather than collapse/corrupt a lineage on a guess.
	var matches []*lineageEntry
	var matchKeys []string
	if openerFP != "" {
		for _, k := range to.order {
			e := to.byBaseHead[k]
			if e == nil || !strings.HasPrefix(k, prefix) || !compactionSignal(e.headDigest, headDigest, e.openerFP, openerFP) {
				continue
			}
			matches = append(matches, e)
			matchKeys = append(matchKeys, k)
		}
	}
	var match *lineageEntry
	var matchKey string
	if len(matches) == 1 {
		match, matchKey = matches[0], matchKeys[0]
	}
	if match != nil {
		// Re-key the matched lineage from its old head to the new head, preserving its recorded sid.
		oldKey := matchKey
		newKey := lineageKey(baseSid, headDigest)
		for i, k := range to.order {
			if k == oldKey {
				to.order = append(to.order[:i], to.order[i+1:]...)
				break
			}
		}
		delete(to.byBaseHead, oldKey)
		match.priorHead = match.headDigest
		match.headDigest = headDigest
		match.lastUsed = now
		to.byBaseHead[newKey] = match
		to.order = append(to.order, newKey)
		return match.sid
	}

	// (c) genuinely new divergent context for this baseSid.
	baseExists := false
	for _, k := range to.order {
		e := to.byBaseHead[k]
		if e != nil && e.sid == baseSid && strings.HasPrefix(k, prefix) {
			baseExists = true
			break
		}
	}
	to.enforcePerBaseCapLocked(tenant, baseSid, s.perBase) // #19: evict only NON-live forks (else exceed softly)
	sid := baseSid
	if baseExists {
		sid = forkSessionID(baseSid, headDigest, 0) // first claimant of this head: slot 0 omitted, back-compat
	}
	e := &lineageEntry{sid: sid, headDigest: headDigest, openerFP: openerFP, lastUsed: now}
	to.byBaseHead[key] = e
	to.order = append(to.order, key)
	for len(to.order) > s.perCap {
		oldest := to.order[0]
		to.order = to.order[1:]
		delete(to.byBaseHead, oldest)
	}
	return sid
}

// resolveForkByOpener re-attaches a CONTINUATION that lost its tool-call ownership (TTL eviction / bridge
// restart / ambiguous duplicate id) to its recorded FORK lineage — the branch-1 ownership-MISS / ambiguity
// resolver (review Comment 1). It does (a) an exact head / one-behind match, then (b) an OPENER bridge: a
// co-resident lineage whose recorded opener fingerprint matches the continuation's opener but whose head moved
// (it GREW from the recorded 1-message opener to the replayed continuation head) re-keys to the new head and
// CONTINUES its sid. It returns "" on miss so the caller can reseed/fall through.
//
// CRITICAL — the opener bridge (b) matches a FORK ONLY, never the base: a content-identical concurrency-fork
// shares the base's opener, so re-attaching a continuation onto the base by opener alone could collapse such a
// fork onto the parent. A content-fork (distinct opener) has its own recorded openerFP and re-attaches here; a
// content-identical fork (no distinguishing opener) does not match and the caller reseeds (Comment 2) — never
// collapses onto baseSid.
func (s *composerLineageStore) resolveForkByOpener(tenant, baseSid, headDigest, openerFP string) string {
	if tenant == "" {
		return ""
	}
	to := s.tenantLineageFor(tenant)
	to.mu.Lock()
	defer to.mu.Unlock()
	s.expireLocked(to)
	now := s.nowFn()
	prefix := baseSid + "\x00"
	// (a) exact head / one-behind match (base OR fork) — a continuation whose head already stabilised.
	key := lineageKey(baseSid, headDigest)
	if e, ok := to.byBaseHead[key]; ok {
		e.lastUsed = now
		to.moveToTail(key)
		return e.sid
	}
	for _, k := range to.order {
		e := to.byBaseHead[k]
		if e != nil && e.priorHead == headDigest && strings.HasPrefix(k, prefix) {
			e.lastUsed = now
			to.moveToTail(k)
			return e.sid
		}
	}
	// (b) opener bridge — FORKS ONLY (see the contract above).
	if openerFP == "" {
		return ""
	}
	var match *lineageEntry
	var matchKey string
	for _, k := range to.order {
		e := to.byBaseHead[k]
		if e == nil || e.sid == baseSid || !strings.HasPrefix(k, prefix) || e.openerFP == "" || e.openerFP != openerFP {
			continue
		}
		match, matchKey = e, k
		break
	}
	if match == nil {
		return ""
	}
	newKey := lineageKey(baseSid, headDigest)
	for i, k := range to.order {
		if k == matchKey {
			to.order = append(to.order[:i], to.order[i+1:]...)
			break
		}
	}
	delete(to.byBaseHead, matchKey)
	match.priorHead = match.headDigest
	match.headDigest = headDigest
	match.lastUsed = now
	to.byBaseHead[newKey] = match
	to.order = append(to.order, newKey)
	return match.sid
}

// reseedLostFork records and returns a fork-namespaced session id for a continuation whose ownership AND
// lineage are both gone (full state loss: restart / cross-replica / full eviction) — review Comment 2. The id
// is forkSessionID(baseSid, headDigest, 0), which can NEVER equal baseSid, so a returning fork cannot collapse
// onto the parent/base; the bridge re-seeds it from the replayed history. With a STABLE serverSecret the id is
// deterministic across restarts/replicas for the same head. Recorded so the conversation's own later turns
// re-resolve to it.
func (s *composerLineageStore) reseedLostFork(tenant, baseSid, headDigest, openerFP string) string {
	forkSid := forkSessionID(baseSid, headDigest, 0)
	if tenant == "" {
		return forkSid
	}
	to := s.tenantLineageFor(tenant)
	to.mu.Lock()
	defer to.mu.Unlock()
	s.expireLocked(to)
	now := s.nowFn()
	to.enforcePerBaseCapLocked(tenant, baseSid, s.perBase) // #19: evict only NON-live forks (else exceed softly)
	key := lineageKey(baseSid, headDigest)
	to.byBaseHead[key] = &lineageEntry{sid: forkSid, headDigest: headDigest, openerFP: openerFP, lastUsed: now}
	to.order = append(to.order, key)
	for len(to.order) > s.perCap {
		oldest := to.order[0]
		to.order = to.order[1:]
		delete(to.byBaseHead, oldest)
	}
	return forkSid
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
)

// composerLeaseEntry is one held session lease (a logical run in flight on this session).
type composerLeaseEntry struct {
	owner uint64    // unique per claim — only the claiming run may touch/release (review P0-1: no clobber)
	since time.Time // last activity, for the staleness self-heal across tool pauses
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
func (s *composerInflightStore) claim(tenant, sid string) (uint64, bool) {
	if tenant == "" || sid == "" {
		return 0, true
	}
	to := s.tenantFor(tenant)
	to.mu.Lock()
	defer to.mu.Unlock()
	now := s.nowFn()
	if e, ok := to.held[sid]; ok && now.Sub(e.since) < s.ttl {
		return 0, false
	}
	owner := composerLeaseSeq.Add(1) // global monotonic mint — survives tenant GC (see composerLeaseSeq)
	to.held[sid] = composerLeaseEntry{owner: owner, since: now}
	return owner, true
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
		to.held[sid] = composerLeaseEntry{owner: owner, since: s.nowFn()}
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

// composerSessionIsLive reports whether a session has a live logical-run lease OR still owns undelivered tool
// calls — either makes its lineage entry "live" and ineligible for the per-base cap eviction (#19).
func composerSessionIsLive(tenant, sid string) bool {
	return composerInflight.held(tenant, sid) || composerOwnership.sessionHasTools(tenant, sid)
}

// composerAcquireOrFork claims the content-resolved session `sid` for a new-user turn, or — if `sid` is already
// running a logical run (a concurrent sibling) — allocates a DISTINCT fork session (the putForkSlotted collide
// path) and claims THAT, so the siblings run in PARALLEL. Returns the session id to use AND the lease owner
// token the caller MUST pass to touch/release. baseSid+headDigest key the slot.
func composerAcquireOrFork(tenant, sid, baseSid, headDigest string) (string, uint64) {
	if owner, ok := composerInflight.claim(tenant, sid); ok {
		return sid, owner
	}
	for attempt := 0; attempt < composerForkAcquireMaxAttempts; attempt++ {
		forkSid := composerLineage.putForkSlotted(tenant, baseSid, headDigest, true)
		if forkSid == "" || forkSid == sid {
			break
		}
		if owner, ok := composerInflight.claim(tenant, forkSid); ok {
			composerDebugf("[composer] concurrency-fork: %s in-flight -> sibling sessionID=%s", sid, forkSid)
			return forkSid, owner
		}
	}
	// Pathological: could not allocate a free sibling. Degrade honestly — route to the original sid (the bridge
	// serializes it) rather than dropping the turn; logged so the cap can be raised if it ever recurs.
	composerDebugf("[composer] concurrency-fork: no free sibling for %s; serializing", sid)
	return sid, 0
}

// compactionSignal reports whether a recorded BASE head change is a COMPACTION (same conversation, rewritten
// body) rather than a genuine divergence (a fork). It mirrors composerHistoryFingerprint's design: a /compact
// preserves the opener (the first non-system message) verbatim and rewrites the body, so when the recorded
// base head moved (oldHead != newHead) BUT the first non-system message's role+prefix is unchanged
// (recordedOpenerFP == currentOpenerFP), we classify it as a compaction and CONTINUE the base (re-key + let
// inp["historyFingerprint"] drive the bridge warm-reseed at the existing seam). An OPENER edit (first message
// changed) is NOT a compaction — it falls through to a fork (documented residual context loss).
func compactionSignal(oldHead, newHead, recordedOpenerFP, currentOpenerFP string) bool {
	if oldHead == newHead {
		return false // the recorded base head did not change at all
	}
	if recordedOpenerFP == "" || currentOpenerFP == "" {
		return false // cannot confirm opener preservation -> do not bridge (fork instead)
	}
	return recordedOpenerFP == currentOpenerFP
}

// lineageOpenerFingerprint returns a 16-hex fingerprint of the FIRST non-system message (role + 256-rune
// prefix) — the growth-stable opener anchor. It never changes as the conversation/fork appends tail turns, so
// the opener bridge can recognise a body rewrite (opener kept, head moved) and distinguish it from an opener
// edit (opener changed → a genuine fork). Empty when there is no non-system message.
func lineageOpenerFingerprint(messages []gjson.Result) string {
	head := composerNonSystemHeadMessages(messages)
	if len(head) == 0 {
		return ""
	}
	m := head[0]
	sum := sha256.Sum256([]byte(m.Get("role").String() + "\x1f" + composerHeadMessageText(m)))
	return hex.EncodeToString(sum[:])[:16]
}

// composerContinuationCarriesOpener reports whether a continuation REPLAYS the conversation from the top — its
// first non-system message is a USER message (a full-replay continuation that carries the opener + history).
// Such a turn can be RE-SEEDED from its own replayed history, so on full state loss it is reseeded to a
// fork-namespaced id (never collapsed onto baseSid). A THIN continuation (first non-system message is
// assistant/tool — only the tail tool exchange, no opener) carries no re-seedable context beyond the
// conversation id, so it keeps the baseSid fallback (the only signal it has). (Review Comment 2.)
func composerContinuationCarriesOpener(messages []gjson.Result) bool {
	head := composerNonSystemHeadMessages(messages)
	if len(head) == 0 {
		return false
	}
	return head[0].Get("role").String() == "user"
}

// errMixedSessionBatch is returned when a single continuation batch carries tool_call_ids that were emitted
// by DIFFERENT bridge sessions (M32). Routing the whole batch to one of them would deliver a partial/wrong
// batch (the other session never gets its result, or gets a foreign id), so the caller must surface this
// rather than silently mis-route.
var errMixedSessionBatch = fmt.Errorf("cursor composer: continuation batch spans multiple sessions")

// errAmbiguousToolOwnership is returned when an echoed tool-call id is owned by MORE THAN ONE of a tenant's
// sessions (ADD-70). The same visible id recurred across two concurrent conversations, so we cannot tell
// which pending call this result answers. The caller disambiguates with a stable conversation /
// previous_response_id if present; otherwise it surfaces this typed error (degrade to a clear client error /
// reseed) rather than guessing the latest writer and stranding the other session's result.
var errAmbiguousToolOwnership = fmt.Errorf("cursor composer: tool-call id is owned by multiple sessions; cannot disambiguate the continuation")

// composerBaseOwnsAmbiguousTool reports whether `candidate` is one of the RECORDED owners of any tool-call id in
// the continuation. #7 (review): an ambiguous tool-call id (owned by 2+ sessions) may route to baseSid ONLY when
// baseSid PROVABLY emitted the id — never as a bare guess from a shared conversation id (multiple sessions share
// that id, so the conv id alone routes the result onto the wrong session and strands the real owner).
func composerBaseOwnsAmbiguousTool(tenant string, oai []byte, candidate string) bool {
	if candidate == "" {
		return false
	}
	results, _, ok := composerToolResultsHinted(gjson.GetBytes(oai, "messages").Array(), composerContinuationHintFor(tenant, oai))
	if !ok {
		return false
	}
	s := composerOwnership
	s.mu.Lock()
	defer s.mu.Unlock()
	to := s.tenants[tenant]
	if to == nil {
		return false
	}
	for _, r := range results {
		id, _ := r["toolCallId"].(string)
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		for _, sid := range s.sessionsForTool(to, id) {
			if sid == candidate {
				return true
			}
		}
	}
	return false
}

// lookupSessionByToolResults returns the session id for a continuation turn whose trailing tool messages
// carry tool_call_ids previously emitted by a bridge session FOR THIS TENANT, or "" if none match.
//
// M32: it verifies that ALL recognized ids map to the SAME session. If two recognized ids belong to
// DIFFERENT sessions, it returns errMixedSessionBatch. ADD-70: if a SINGLE echoed id is owned by more than
// one of the tenant's sessions, it returns errAmbiguousToolOwnership. Unrecognized ids are ignored for the
// same-session check (they degrade-gracefully via the caller's re-seed path); only ids that resolve to a
// session participate, and they must all agree on exactly one session.
func lookupSessionByToolResults(tenant string, oai []byte) (string, error) {
	// ADD-65: classify with the tenant hint so a Responses server-side-chained continuation [..., tool, user]
	// (no assistant anchor) is recognized via previous_response_id / tool ownership and its ids are resolved
	// here. This call is OUTSIDE the store lock below, so the hint's ownership probe (which also locks) is safe.
	results, _, ok := composerToolResultsHinted(gjson.GetBytes(oai, "messages").Array(), composerContinuationHintFor(tenant, oai))
	if !ok {
		return "", nil
	}
	s := composerOwnership
	s.mu.Lock()
	defer s.mu.Unlock()
	to := s.tenants[tenant]
	if to == nil {
		return "", nil
	}
	s.expireLocked(to) // ADD-71: drop stale entries before resolving (and clean up below if emptied).
	defer func() {
		if len(to.byKey) == 0 {
			delete(s.tenants, tenant)
		}
	}()
	resolved := ""
	for _, r := range results {
		id, _ := r["toolCallId"].(string)
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		sids := s.sessionsForTool(to, id)
		if len(sids) == 0 {
			continue
		}
		if len(sids) > 1 {
			// ADD-70: this id alone is owned by multiple sessions — inherently ambiguous without a stable id.
			return "", errAmbiguousToolOwnership
		}
		sid := sids[0]
		if resolved == "" {
			resolved = sid
			continue
		}
		if sid != resolved {
			return "", errMixedSessionBatch // M32: the batch spans two sessions.
		}
	}
	return resolved, nil
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

// deriveComposerSessionID returns the bridge session id for this turn, scoped to the selected credential
// (tenant boundary) so different users never share a bridge session / SDK stateRoot. When a stable
// conversation identifier is present (request header, inbound metadata.user_id, body conv-id/cache-key, or
// CLIProxy session metadata) the id is derived deterministically from it. For a continuation (tool_results)
// turn it first routes by the session that emitted the pending tool calls (by tool_call_id). It NEVER routes
// by message content. When nothing stable is available it DEGRADES GRACEFULLY by minting a fresh session
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
			// DIAGNOSTIC (debug-only): a Claude Code "/workflow" compose-workflow side-channel is ALSO a 0-tool turn
			// and is isolated here, indistinguishable at routing time from a genuine title/topic probe. Dump the
			// inbound top-level keys + metadata keys + the trailing user-text prefix so we can confirm whether a
			// structured marker (e.g. workflow_keyword_request) actually reaches the proxy — the prerequisite for
			// routing such a request to a tool-equipped path instead of advertise=0. Logged ONLY under debug.
			msgs := gjson.GetBytes(oai, "messages").Array()
			ut := ""
			for i := len(msgs) - 1; i >= 0; i-- {
				if msgs[i].Get("role").String() == "user" {
					ut = cursorMessageText(msgs[i])
					break
				}
			}
			if r := []rune(ut); len(r) > 160 {
				ut = string(r[:160])
			}
			composerDebugf("[composer] one-shot DIAG sid=%s bodyKeys=[%s] metaKeys=[%s] userText=%q", sid,
				composerJSONKeys(opts.OriginalRequest), composerJSONKeys([]byte(gjson.GetBytes(opts.OriginalRequest, "metadata").Raw)), ut)
		}
		return sid, nil
	}

	// Comment 5: a tool_results continuation routes by tool_call_id OWNERSHIP first — the session that
	// actually emitted the pending tool calls is authoritative, ahead of the stable conversation id. This
	// prevents a continuation whose tools were emitted under a different session (isolation/race) from being
	// silently mis-routed to the conv-id session (which would never match the pending tool calls).
	if _, _, isCont := composerToolResultsHinted(gjson.GetBytes(oai, "messages").Array(), contHint); isCont {
		sid, errLookup := lookupSessionByToolResults(tenant, oai)
		if errLookup != nil {
			// ADD-70: an AMBIGUOUS id (owned by 2+ sessions for this tenant) can still be disambiguated when the
			// turn carries an explicit stable conversation id or previous_response_id — route by that instead of
			// guessing. Only when NO such disambiguator exists do we surface the typed ambiguity error (clear
			// client error / reseed) rather than picking the latest writer and stranding the other session.
			if errLookup == errAmbiguousToolOwnership {
				if pid := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String()); pid != "" {
					if mapped := lookupComposerResponseSession(tenant, pid); mapped != "" {
						composerDebugf("[composer] deriveSessionID BRANCH=continuation(ambiguous id disambiguated by previous_response_id) -> sessionID=%s", mapped)
						return mapped, nil
					}
				}
				if id := stableConversationID(opts); id != "" {
					baseSid := composerStableBaseSessionID(tenant, apiKey, id)
					// Comment 1: an ambiguous tool-call id (owned by 2+ sessions) can be disambiguated by the lineage
					// opener bridge when the continuation's opener matches exactly ONE recorded fork — try that before
					// collapsing to the bare stable-conv hash.
					contMsgs := gjson.GetBytes(oai, "messages").Array()
					headDigest, openerFP := lineageForkKeys(tenant, contMsgs)
					if sid := composerLineage.resolveForkByOpener(tenant, baseSid, headDigest, openerFP); sid != "" {
						composerDebugf("[composer] deriveSessionID BRANCH=continuation(ambiguous id disambiguated by opener bridge) -> sessionID=%s", sid)
						return sid, nil
					}
					// #7 (review): the stable conv id does NOT by itself disambiguate an ambiguous tool-call id —
					// multiple sessions share that conv id, which is WHY it is ambiguous. Route to baseSid ONLY when
					// baseSid is a PROVEN owner of the echoed id (it actually emitted it); otherwise collapsing onto
					// the parent/base would post the result to the wrong session and strand the real owner, so surface
					// the typed ambiguity error (a clean client error to reseed from). (#8's session-namespaced ids
					// prevent this ambiguity for new tool flows; this guards the legacy/cross-instance residual.)
					if composerBaseOwnsAmbiguousTool(tenant, oai, baseSid) {
						composerDebugf("[composer] deriveSessionID BRANCH=continuation(ambiguous id, baseSid is a PROVEN owner) -> sessionID=%s", baseSid)
						return baseSid, nil
					}
					composerDebugf("[composer] deriveSessionID BRANCH=continuation(AMBIGUOUS ownership, base not an owner, no collapse): %v", errLookup)
					return "", errLookup
				}
				composerDebugf("[composer] deriveSessionID BRANCH=continuation(AMBIGUOUS ownership, no disambiguator): %v", errLookup)
				return "", errLookup
			}
			// M32: a batch spanning multiple sessions must NOT be delivered partially to one of them — surface
			// a typed error rather than silently mis-routing (which would strand the other session's result).
			composerDebugf("[composer] deriveSessionID BRANCH=continuation(MIXED-SESSION error): %v", errLookup)
			return "", errLookup
		}
		if sid != "" {
			composerDebugf("[composer] deriveSessionID BRANCH=continuation(tool_call_id ownership) -> sessionID=%s", sid)
			return sid, nil
		}
		// No recorded emitter (bridge restart / TTL eviction / cross-instance): fall back to the stable conv
		// id so the bridge can re-seed.
		if id := stableConversationID(opts); id != "" {
			baseSid := composerStableBaseSessionID(tenant, apiKey, id)
			// Comment 1: re-attach the continuation to ITS fork via the lineage opener bridge (handles a fork
			// whose head grew past its recorded 1-message opener after ownership was lost). On miss:
			// Comment 2: a FULL-REPLAY continuation (carries a user opener) RESEEDS to a fork-namespaced id
			// (never baseSid) so a returning fork cannot collapse onto the parent — the bridge re-seeds from the
			// replayed history. A THIN continuation (no replayed opener) keeps the baseSid fallback.
			contMsgs := gjson.GetBytes(oai, "messages").Array()
			headDigest, openerFP := lineageForkKeys(tenant, contMsgs)
			if sid := composerLineage.resolveForkByOpener(tenant, baseSid, headDigest, openerFP); sid != "" {
				composerDebugf("[composer] deriveSessionID BRANCH=continuation(lineage opener-bridge) convID=%q -> sessionID=%s", id, sid)
				return sid, nil
			}
			if composerContinuationCarriesOpener(contMsgs) {
				sid := composerLineage.reseedLostFork(tenant, baseSid, headDigest, openerFP)
				composerDebugf("[composer] deriveSessionID BRANCH=continuation(reseed lost fork, no collapse) convID=%q -> sessionID=%s", id, sid)
				return sid, nil
			}
			composerDebugf("[composer] deriveSessionID BRANCH=continuation(stable fallback, thin) convID=%q -> sessionID=%s", id, baseSid)
			return baseSid, nil
		}
		// EX6: stateless/restarted client — no recorded emitter AND no stable id. DEGRADE GRACEFULLY: mint a
		// fresh session instead of 500-ing a routine turn. composerInput carries history+system on the
		// continuation (EX10/EX8), so the bridge re-seeds the fresh session before applying the tool results
		// rather than losing the turn. Never error here.
		sid = mintComposerSessionID()
		composerDebugf("[composer] deriveSessionID BRANCH=continuation(mint+re-seed, no emitter/no stable id) -> sessionID=%s", sid)
		return sid, nil
	}
	// H16 (C-RESPID): a Responses/Codex follow-up that carries previous_response_id resumes the DURABLE agent
	// via the response-id->sessionID map (recorded outward in executeComposer/executeComposerStream). This is
	// BEFORE the stable-conv hash because previous_response_id is intentionally NOT in stableConversationID's
	// list (it is not stable — it changes every turn). On a map miss (bridge restart / eviction) we fall
	// through to the stable-conv hash and finally to a fresh mint, so a routine follow-up never errors.
	if pid := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String()); pid != "" {
		if sid := lookupComposerResponseSession(tenant, pid); sid != "" {
			composerDebugf("[composer] deriveSessionID BRANCH=previous_response_id(durable resume) -> sessionID=%s", sid)
			return sid, nil
		}
		composerDebugf("[composer] deriveSessionID previous_response_id=%q present but unmapped (restart/evict) -> falling through", pid)
	}
	if id := stableConversationID(opts); id != "" {
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
		// composerHistoryFingerprint window + an opener bridge, so a fork's recorded sid is computed once and
		// re-resolved turn-to-turn (multi-turn fork stability). The UNCHANGED inp["historyFingerprint"] field
		// still drives the bridge warm-reseed on the re-key (compaction) path.
		messages := gjson.GetBytes(oai, "messages").Array()
		fp16 := composerHistoryFingerprint(messages)
		headDigest := lineageHeadDigest(tenant, fp16, messages)
		openerFP := lineageOpenerFingerprint(messages)
		sid := composerLineage.resolveStableSession(tenant, baseSid, headDigest, openerFP)
		composerDebugf("[composer] deriveSessionID BRANCH=stable convID=%q -> sessionID=%s (baseSid=%s)", id, sid, baseSid)
		return sid, nil
	}
	// New user turn with no stable id (a stateless client — curl/SDK/simple UI). Mint a FRESH RANDOM session
	// id: it can never collide with another conversation (unlike a content hash), so this is safe for the
	// default Cursor Composer Client-Tools path; a stateless multi-turn client keeps context via history re-seeding (not durable
	// resume). Continuations still resolve because we record each emitted tool_call_id -> session.
	sid := mintComposerSessionID()
	composerDebugf("[composer] deriveSessionID BRANCH=mint(stateless new user turn) -> sessionID=%s", sid)
	return sid, nil
}

// deriveComposerSessionIDLive is the live-executor session resolver. It first resolves the content-pure session
// (deriveComposerSessionID), then — ONLY for a NEW-USER turn on a stable conversation id (the lone branch where
// concurrent siblings sharing one conversation id collide) — CLAIMS that session's logical-run lease, or FORKS
// onto a distinct sibling session if it is already running (ISOLATION invariant). The caller MUST keep the
// lease alive across the run (composerInflight.touch on a tool_use pause) and free it at the run's terminal end
// (composerInflight.release). The branch-4 predicate below MIRRORS deriveComposerSessionID's guards (kept in
// lockstep) so only a turn that genuinely lands on the shared stable session participates: a continuation
// re-attaches by ownership (its run already holds the lease) and a mint is unique, so neither claims here.
func deriveComposerSessionIDLive(auth *cliproxyauth.Auth, apiKey string, oai []byte, opts cliproxyexecutor.Options) (string, uint64, error) {
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
		return sid, 0, nil // a continuation re-attaches by ownership; its logical run already holds the lease
	}
	if pid := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, "previous_response_id").String()); pid != "" {
		if lookupComposerResponseSession(tenant, pid) != "" {
			// P0-5: a previous_response_id resume starts a NEW logical run on a durable session — it must take part
			// in the lease, not bypass it. Without a claim, two concurrent resumes of the SAME response id both land
			// on the pinned session and the bridge rejects the second concurrent turn (serial session -> 500), the
			// exact collision the lease prevents. Claim the pinned session; if it is already running (a concurrent
			// resume, or an abandoned paused run still holding the lease) fork onto a distinct sibling. The common
			// case — sequential resumes — always re-claims the pinned session and preserves its durable context;
			// only the rare concurrent-resume race degrades to a fresh (context-light) fork instead of a 500.
			messages := gjson.GetBytes(oai, "messages").Array()
			headDigest := lineageHeadDigestFromMessages(tenant, messages)
			finalSid, owner := composerAcquireOrFork(tenant, sid, sid, headDigest)
			return finalSid, owner, nil
		}
	}
	id := stableConversationID(opts)
	if id == "" {
		return sid, 0, nil // stateless mint: unique by construction, cannot collide
	}
	// Branch-4 new-user stable turn: claim its session or fork onto a free sibling. baseSid+headDigest match
	// what deriveComposerSessionID just used, so the fork slots off the lineage entry it recorded.
	baseSid := composerStableBaseSessionID(tenant, apiKey, id)
	messages := gjson.GetBytes(oai, "messages").Array()
	headDigest := lineageHeadDigestFromMessages(tenant, messages)
	finalSid, owner := composerAcquireOrFork(tenant, sid, baseSid, headDigest)
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
	// ADD-65: use the hinted continuation check so a Responses server-side-chained [..., tool, user] turn (and an
	// ownership-signalled continuation) is recognized as a continuation — NOT isolated as a fresh one-shot, which
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
func cursorMessageText(m gjson.Result) string {
	c := m.Get("content")
	if c.Type == gjson.String {
		return c.String()
	}
	if c.IsArray() {
		// M29: join multiple text parts with a newline, not a bare concat. Adjacent blocks are distinct
		// segments (e.g. stdout then stderr, or user text split across client blocks); concatenating them
		// with no separator corrupts code/command-output/JSON boundaries ("foo"+"bar" -> "foobar").
		var parts []string
		for _, part := range c.Array() {
			if t := part.Get("text"); t.Exists() {
				parts = append(parts, t.String())
			}
		}
		return strings.Join(parts, "\n")
	}
	return c.String()
}

// extractComposerSystem concatenates system/developer instruction text.
func extractComposerSystem(messages []gjson.Result) string {
	var b strings.Builder
	for _, m := range messages {
		if r := m.Get("role").String(); r == "system" || r == "developer" {
			if t := cursorMessageText(m); t != "" {
				if b.Len() > 0 {
					b.WriteString("\n\n")
				}
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

// isPureSystemReminder reports whether the trailing text is ONLY auto-injected <system-reminder> block(s)
// (with no other non-whitespace content). Such a block is context that accompanies tool results, not the
// user's own instruction — so the mixed-turn fold must NOT label it as "the user's latest instruction"
// (EX1), and it must NOT drive a fresh C1 userText send.
func isPureSystemReminder(s string) bool {
	t := strings.TrimSpace(s)
	if !strings.HasPrefix(t, "<system-reminder>") {
		return false
	}
	// Strip every <system-reminder>...</system-reminder> block; if nothing non-whitespace remains, it is pure.
	const open, close = "<system-reminder>", "</system-reminder>"
	for {
		i := strings.Index(t, open)
		if i < 0 {
			break
		}
		j := strings.Index(t[i:], close)
		if j < 0 {
			// Unterminated reminder: treat the remainder as part of the block.
			t = t[:i]
			break
		}
		t = t[:i] + t[i+j+len(close):]
	}
	return strings.TrimSpace(t) == ""
}

// isAutoInjectedReminder reports whether a trailing pure-<system-reminder> block is AUTO-INJECTED context
// (M30) rather than a user literally typing a reminder as their message. It is auto-injected only when (a)
// the block is a pure system reminder AND (b) it recurs verbatim elsewhere in the transcript — clients like
// Claude Code re-inject the same reminder as context across turns, so a recurrence is a strong synthetic
// signal. A FIRST-occurrence pure reminder is treated as user text (returns false) so the turn is still
// answerable: better to occasionally answer a synthetic-looking first occurrence than to silently swallow a
// real user message. Best-effort — the wire carries no explicit synthetic-reminder flag.
func isAutoInjectedReminder(trailing string, messages []gjson.Result) bool {
	if !isPureSystemReminder(trailing) {
		return false
	}
	needle := strings.TrimSpace(trailing)
	occurrences := 0
	for _, m := range messages {
		switch m.Get("role").String() {
		case "user", "system", "developer", "tool":
			if strings.Contains(m.Get("content").Raw, needle) || strings.Contains(cursorMessageText(m), needle) {
				occurrences++
			}
		}
		if occurrences >= 2 {
			return true // appears more than once => re-injected context, not a one-off user message
		}
	}
	return false
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
func extractComposerImages(m gjson.Result) []map[string]any {
	c := m.Get("content")
	if !c.IsArray() {
		return nil
	}
	var out []map[string]any
	for _, part := range c.Array() {
		// ADD-55: accept both the object form {"image_url":{"url":"…"}} AND the scalar form
		// {"image_url":"…"} (some clients / translated shapes send a bare string). A file_id-only image part
		// has no fetchable url here, so it yields "" and is treated as a degenerate/unsupported image (ADD-56
		// then makes it model-visible or rejects an image-only turn) — never an empty image URL part.
		iu := part.Get("image_url")
		u := iu.Get("url").String()
		if u == "" && iu.Type == gjson.String {
			u = iu.String()
		}
		if u == "" {
			u = part.Get("source.data").String()
		}
		if strings.HasPrefix(u, "data:") {
			idx := strings.Index(u, ",")
			if idx <= 0 {
				continue
			}
			meta, data := u[5:idx], u[idx+1:]
			mime := meta
			if semi := strings.Index(meta, ";"); semi >= 0 {
				mime = meta[:semi]
			}
			// EX12: never append an inline image with an empty data or mime field (toSdkImages would throw).
			if strings.TrimSpace(data) == "" || strings.TrimSpace(mime) == "" {
				continue
			}
			out = append(out, map[string]any{"data": data, "mimeType": mime})
			continue
		}
		// C4: an http(s) URL is carried as a URL-form image. mimeType is best-effort from a trailing
		// .png/.jpg/... extension; omit the key when unknown.
		if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
			img := map[string]any{"url": u}
			if mime := imageMimeFromURL(u); mime != "" {
				img["mimeType"] = mime
			}
			out = append(out, img)
		}
	}
	return out
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

// composerReseedImageDirective is appended to a lost-continuation reseed that carries tool-result image(s)
// (EX3). The reseed replays the tool-call request as history with the tool still advertised, so without this
// the model re-calls the tool to fetch the file instead of reading the image already attached to the reseed.
const composerReseedImageDirective = "The image(s) referenced in the conversation above are attached directly to this message — read them here. Do NOT call a tool again to fetch them."

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

// composerRejectStoreFalse returns a typed 400 when the (translated) request explicitly set store:false. Cursor
// Composer persists/resumes durable agent state by a stable session id; there is no honest ephemeral no-store
// mode (the bridge's stateRoot/resumeAgent are inherently durable), so silently persisting despite store:false
// would violate the caller's no-store contract (ADD-94 / Comment 4) — reject BEFORE any durable state is
// created rather than fake-acknowledging it. The Responses translator surfaces store:false as a top-level body
// field (C-ADD94-STORE); store:true / absent is the durable default and needs no handling.
func composerRejectStoreFalse(oai []byte) error {
	if v := gjson.GetBytes(oai, "store"); v.Exists() && !v.Bool() {
		return &composerInvalidRequestError{msg: "cursor composer: store:false is not supported — Cursor Composer requires durable agent state; omit store or set store:true"}
	}
	return nil
}

// lastUserTurnImageOnlyInvalid reports whether the request's LAST user message had image part(s), produced no
// valid extracted image, and carries no text (ADD-56). Continuations are out of scope here (a tool_results
// turn is classified/handled elsewhere and must never be rejected for a degenerate image) — this guards only
// the new-user turn path.
//
// Comment 5: it takes the SAME composerContinuationHint that deriveComposerSessionID / composerInputHinted use
// and classifies via composerToolResultsHinted (NOT the hintless composerToolResults). Otherwise a branch-(c)
// continuation — a Responses previous_response_id-chained [..., tool, user] turn, or a tool-ownership-hinted
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

// boundComposerHistoryLines joins the rendered history lines, but if their total size exceeds maxBytes it keeps
// the HEAD (opener / early context) and the recent TAIL and replaces the dropped middle with an explicit marker
// — so a re-seeded transcript stays bounded regardless of conversation length (P0-3) while preserving the two
// ends the model needs most (who started the task + what just happened). A non-positive maxBytes disables the cap.
func boundComposerHistoryLines(lines []string, maxBytes int) string {
	if len(lines) == 0 {
		return ""
	}
	total := 0
	for _, l := range lines {
		total += len(l) + 1
	}
	if maxBytes <= 0 || total <= maxBytes {
		return strings.Join(lines, "\n")
	}
	half := maxBytes / 2
	// Head: take whole lines from the front until ~half the budget (always at least the opener).
	hi, hb := 0, 0
	for hi < len(lines) {
		c := len(lines[hi]) + 1
		if hb+c > half && hi > 0 {
			break
		}
		hb += c
		hi++
	}
	// Tail: take whole lines from the back until ~half the budget, never crossing the head.
	ti, tb := len(lines), 0
	for ti > hi {
		c := len(lines[ti-1]) + 1
		if tb+c > half && ti < len(lines) {
			break
		}
		tb += c
		ti--
	}
	if ti <= hi {
		return strings.Join(lines, "\n") // a few oversized lines filled both ends; nothing safe to drop
	}
	out := make([]string, 0, hi+1+(len(lines)-ti))
	out = append(out, lines[:hi]...)
	out = append(out, fmt.Sprintf("[... %d earlier message(s) omitted to bound the replayed history ...]", ti-hi))
	out = append(out, lines[ti:]...)
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
	if err := json.Unmarshal([]byte(rf.Raw), &out); err != nil {
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

// extractComposerClientEnv reads the client's real environment (workspace/cwd/shell/os) from request
// headers when present, so path-relative tools route against the client's machine rather than a
// hardcoded /workspace. Empty when the client does not advertise it (bridge falls back to neutral defaults).
func extractComposerClientEnv(opts cliproxyexecutor.Options) map[string]any {
	if opts.Headers == nil {
		return nil
	}
	env := map[string]any{}
	if v := strings.TrimSpace(opts.Headers.Get("X-Workspace-Path")); v != "" {
		env["workspacePaths"] = []string{v}
	}
	if v := strings.TrimSpace(opts.Headers.Get("X-Cwd")); v != "" {
		env["processWorkingDirectory"] = v
	}
	if v := strings.TrimSpace(opts.Headers.Get("X-Shell")); v != "" {
		env["shell"] = v
	}
	if v := strings.TrimSpace(opts.Headers.Get("X-Os-Version")); v != "" {
		env["osVersion"] = v
	}
	if len(env) == 0 {
		return nil
	}
	return env
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
	// ownsToolCallID reports whether a tool_call_id is OWNED by some session for this tenant (nil = unknown).
	ownsToolCallID func(id string) bool
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
func composerToolResultEntry(m gjson.Result) map[string]any {
	// ADD-95: cap the LIVE tool-result content (authoritative executor-side bound; the bridge has a slightly
	// higher backstop cap so this one normally wins). A huge live result would otherwise flow unbounded into
	// Cursor and trip ERROR_CONVERSATION_TOO_LONG with no graceful marker.
	r := map[string]any{"toolCallId": m.Get("tool_call_id").String(), "content": truncateCursorToolResultLive(cursorMessageText(m))}
	// C5: a client tool the inbound marked failed must reach the model as a failure, not a clean success.
	if m.Get("is_error").Bool() {
		r["isError"] = true
	}
	// EX3: an image embedded inside a tool_result must not be dropped.
	if imgs := extractComposerImages(m); len(imgs) > 0 {
		r["images"] = imgs
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
		var sb strings.Builder
		res := make([]map[string]any, 0)
		replied := false
		for i := lastTC + 1; i < len(messages); i++ {
			switch messages[i].Get("role").String() {
			case "tool":
				res = append(res, composerToolResultEntry(messages[i]))
			case "user":
				if t := cursorMessageText(messages[i]); t != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(t)
				}
			case "assistant":
				replied = true
			}
		}
		if len(res) > 0 && !replied {
			return res, sb.String(), true
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
	// (a previous_response_id is present, or a trailing tool_call_id is owned for this tenant), so we never
	// reclassify an ordinary conversation that merely ends [..., tool, user]. This shape must NEVER fall through
	// to the fresh-user-turn branch, which would strand the tool output as inert history behind a paused run.
	if hint.hasPreviousResponseID || hint.ownsToolCallID != nil {
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
		var sb strings.Builder
		res := make([]map[string]any, 0)
		ownedSignal := false
		seenTool := false
		for i := lastAssistant + 1; i < len(messages); i++ {
			switch messages[i].Get("role").String() {
			case "tool":
				seenTool = true
				tr := composerToolResultEntry(messages[i])
				res = append(res, tr)
				if hint.ownsToolCallID != nil {
					if id, _ := tr["toolCallId"].(string); strings.TrimSpace(id) != "" && hint.ownsToolCallID(id) {
						ownedSignal = true
					}
				}
			case "user":
				// Only the user text that TRAILS the tool results is the new instruction (C1/userText). User text
				// BEFORE the first tool result is prior-turn history, not the trailing instruction.
				if !seenTool {
					continue
				}
				if t := cursorMessageText(messages[i]); t != "" {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(t)
				}
			}
		}
		if len(res) > 0 && (hint.hasPreviousResponseID || ownedSignal) {
			return res, sb.String(), true
		}
	}
	return nil, "", false
}

// composerInput is the back-compat entry that classifies a turn with a BODY-ONLY hint (previous_response_id),
// no tenant ownership. The executor calls composerInputHinted with the full tenant hint so branch (c)
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
		// A continuation. A MIXED turn carries tool_results AND a trailing user message in the SAME turn — this
		// is normal client behavior (e.g. Claude Code when you interrupt a running tool, or background a task,
		// and then type a new message; it bundles the results for what finished WITH your new text). Anthropic
		// answers both in a SINGLE assistant turn. The @cursor/sdk has no mid-resume user-message seam, so fold
		// the user's message into the LAST tool result's content (clearly delimited) so the model reads it when
		// it resumes — AND carry it as a first-class userText (C1) so the bridge can still answer it when no
		// pending matched / the run was torn down. We must NEVER error here: erroring 500s a routine turn.
		if t := strings.TrimSpace(trailing); t != "" && len(results) > 0 {
			// EX1/M30: a trailing block that is purely an AUTO-INJECTED <system-reminder> is context, not the
			// user's instruction — frame it neutrally and do NOT set userText (a reminder must not drive a fresh
			// send). M30: but a user who LITERALLY types text starting with <system-reminder> must still be
			// answered, so the pure-reminder classification alone is not enough. No inbound schema carries a
			// "synthetic reminder" metadata flag, so the only available wire signal is: an auto-injected reminder
			// recurs verbatim in the transcript (Claude Code re-injects it as context), whereas a first-occurrence
			// user-authored <system-reminder> does not. When in doubt (a first occurrence) treat it as the user's
			// instruction so the turn is answerable. Documented best-effort edge-case limitation.
			reminder := isAutoInjectedReminder(t, messages)
			// ADD-89: choose the fold target = the last NON-error tool result. NEVER fold genuine trailing user
			// text into a result whose isError==true: the resumed run would read the instruction as part of a
			// FAILED tool's output and answer the failure instead of the instruction. If every result is an error
			// (or there is none to safely fold into), skip the inline fold entirely and rely solely on userText —
			// which we ALWAYS keep first-class (the bridge's C1 fresh-send carries it). The reminder framing is
			// likewise only applied to a non-error result; a reminder alongside an all-error batch is dropped from
			// the inline fold (it must never label a failed result, and a reminder never drives a fresh send).
			foldIdx := -1
			for i := len(results) - 1; i >= 0; i-- {
				if isErr, _ := results[i]["isError"].(bool); !isErr {
					foldIdx = i
					break
				}
			}
			if reminder {
				if foldIdx >= 0 {
					prev, _ := results[foldIdx]["content"].(string)
					results[foldIdx]["content"] = strings.TrimRight(prev, "\n") + "\n\n[System reminder accompanying these tool results:]\n" + t
				}
			} else {
				if foldIdx >= 0 {
					prev, _ := results[foldIdx]["content"].(string)
					results[foldIdx]["content"] = strings.TrimRight(prev, "\n") + "\n\n[The user also sent the following message alongside these tool results — treat it as their latest instruction:]\n" + t
				}
				// C1 (EX2)/ADD-89: a real trailing user message is ALWAYS first-class — even when every result is
				// an error and nothing was folded inline, so it is never lost.
				inp["userText"] = t
			}
		}
		// EX4/EX14: carry any image attachments from the trailing user message(s) of this continuation so the
		// bridge's fresh send (or tool-result fold) can attach them. Re-scan the messages after the last
		// assistant tool_calls turn for role:user image parts (keeps composerToolResults text-only/focused).
		if imgs := trailingContinuationImages(messages); len(imgs) > 0 {
			inp["images"] = imgs
		}
		// EX8 (C3): a system-prompt swap (e.g. post-ExitPlanMode) on a continuation must reach the model.
		if sys := extractComposerSystem(messages); sys != "" {
			inp["system"] = sys
		}
		// EX10: carry rendered history so the bridge can seed a fresh session before applying these results
		// (recovers an evicted/restarted/410'd session). Bounded per EX13 inside renderComposerHistory.
		if hist := renderComposerHistory(messages, continuationHistoryBound(messages)); hist != "" {
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
	if lastUserIdx >= 0 {
		m := messages[lastUserIdx]
		text := cursorMessageText(m)
		imgs := extractComposerImages(m)
		if len(imgs) > 0 {
			inp["images"] = imgs
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
	}
	if sys := extractComposerSystem(messages); sys != "" {
		inp["system"] = sys
	}
	if hist := renderComposerHistory(messages, lastUserIdx); hist != "" {
		inp["history"] = hist
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

// trailingContinuationImages collects image parts from the role:user message(s) that trail the last
// assistant tool_calls turn of a continuation (EX4/EX14) — the new user text that was bundled with the
// tool results. Empty when there is no trailing user image.
func trailingContinuationImages(messages []gjson.Result) []map[string]any {
	lastTC := -1
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").Exists() {
			lastTC = i
			break
		}
	}
	var out []map[string]any
	for i := lastTC + 1; i < len(messages); i++ {
		if messages[i].Get("role").String() != "user" {
			continue
		}
		out = append(out, extractComposerImages(messages[i])...)
	}
	return out
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
			if err := json.Unmarshal([]byte(params.Raw), &schema); err == nil {
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
	spec := resolveToolSpec(name, defs, overrides)
	outName := name
	if spec != nil {
		outName = spec.Name
	}
	if input.Exists() && !input.IsObject() {
		// Non-object input: wrap the RAW value verbatim under 'input' so its semantics survive to the client.
		// Skip schema normalization (which is keyed on an object matching the tool schema) — there is no object
		// to reconcile, and reshaping a primitive would lose it.
		b, _ := json.Marshal(map[string]any{"input": input.Value()})
		return outName, string(b)
	}
	args := map[string]any{}
	if input.Exists() && input.IsObject() {
		_ = json.Unmarshal([]byte(input.Raw), &args)
	}
	if spec == nil {
		b, _ := json.Marshal(args)
		return name, string(b)
	}
	normalized := normalizeToolArguments(args, spec)
	b, _ := json.Marshal(normalized)
	return spec.Name, string(b)
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
	return composerAdvertisePrep{advertise: advertise, toolChoice: toolChoice, constraints: constraints}
}

// composerTurnBody builds the JSON body for a POST /agent/turn. constraints carries the enforced
// response constraints (responseFormat/stop/maxTokens) as explicit top-level fields; the bridge
// converts them (and toolChoice) into model instructions and tool-advertisement gating.
func composerTurnBody(sessionID, model string, input map[string]any, advertise []map[string]any, toolChoice string, clientEnv map[string]any, constraints map[string]any) []byte {
	body := map[string]any{"sessionId": sessionID, "model": model, "input": input}
	// H10 (C-CONTINUATION-TOOLS): attach the current tool inventory on EVERY turn when advertised, not only
	// on a new-user turn. The bridge refreshes session.advertise from body.tools on tool_results turns too, so
	// dropping tools on a continuation left the bridge with a STALE advertise set (removed/added tools, plan-mode
	// ExitPlanMode, MCP availability changes). The tool_choice=none gating in executeComposer/executeComposerStream
	// runs BEFORE this (advertise=nil there), so `none` still sends no tools — that ordering is intentional.
	if len(advertise) > 0 {
		body["tools"] = advertise
	}
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

// composerReseedLostContinuation builds a single fresh-seed retry for a tool_results continuation that the
// bridge reported GONE (410): the session this result answers was lost (ESC-interrupt, bridge restart, or idle
// eviction). It returns (reseedSid, reseedBody, true) ONLY when the inbound turn is a tool_results continuation
// that REPLAYS the conversation from the top (composerContinuationCarriesOpener) — so it can be re-seeded from
// its own replayed history. Otherwise it returns ("", nil, false) and the caller surfaces the typed 410: a thin
// continuation (no replayed opener) or a non-continuation 410 has no re-seedable context and must NEVER be
// turned into a fabricated success. The reseed re-frames the turn as a fresh type:"user" seed (mirrors the
// deriveComposerSessionID lost-fork reseed at the ownership-miss branch): it drops the now-unanswerable
// tool_results and keeps the replayed history/system/images plus the trailing user instruction as the seed
// text, so the bridge creates a fresh session, seeds it from history+system, and answers — the lost tool work
// is re-done, never silently discarded.
func composerReseedLostContinuation(tenant, apiKey string, oai []byte, opts cliproxyexecutor.Options, contHint composerContinuationHint, model string, advertise []map[string]any, toolChoice string, constraints map[string]any) (string, []byte, bool) {
	contMsgs := gjson.GetBytes(oai, "messages").Array()
	if _, _, isCont := composerToolResultsHinted(contMsgs, contHint); !isCont {
		return "", nil, false // a new-user-turn 410 is a real error, never reseeded
	}
	if !composerContinuationCarriesOpener(contMsgs) {
		return "", nil, false // thin continuation: no replayed opener to seed from -> surface the 410
	}
	// Re-derive the reseed sid exactly as deriveComposerSessionID does on a lost continuation (the ownership-miss
	// branch): a fork-namespaced id (deterministic with a stable lineage secret, re-resolvable on later turns) when
	// a stable conversation id is present, else a fresh random mint (stateless client).
	var reseedSid string
	if id := stableConversationID(opts); id != "" {
		baseSid := composerStableBaseSessionID(tenant, apiKey, id)
		headDigest := lineageHeadDigest(tenant, composerHistoryFingerprint(contMsgs), contMsgs)
		openerFP := lineageOpenerFingerprint(contMsgs)
		reseedSid = composerLineage.reseedLostFork(tenant, baseSid, headDigest, openerFP)
	} else {
		reseedSid = mintComposerSessionID()
	}
	inp := composerInputHinted(oai, contHint)
	seed := map[string]any{"type": "user", "text": ""}
	if t, ok := inp["userText"].(string); ok && t != "" {
		seed["text"] = t
	}
	for _, k := range []string{"system", "history", "historyFingerprint"} {
		if v, ok := inp[k]; ok {
			seed[k] = v
		}
	}
	// EX3 (reseed): a Read-tool/screenshot image lives in inp["results"][].images (composerToolResultEntry),
	// NOT inp["images"] (which only carries trailing role:user images via trailingContinuationImages). The
	// reseed drives a fresh type:"user" turn, whose images come solely from seed["images"] — so gather BOTH,
	// else the image is silently dropped on a lost-continuation reseed and the model re-reads the same file.
	var seedImages []map[string]any
	if v, ok := inp["images"].([]map[string]any); ok {
		seedImages = append(seedImages, v...)
	}
	if results, ok := inp["results"].([]map[string]any); ok {
		for _, r := range results {
			if imgs, ok := r["images"].([]map[string]any); ok {
				seedImages = append(seedImages, imgs...)
			}
		}
	}
	if len(seedImages) > 0 {
		seed["images"] = seedImages
		// The reseed replays "read the file" as history WITH the tool still advertised, so the model would
		// re-call the tool instead of reading the image already attached here (the warm twin avoids this via
		// its resumed durable context). Append an explicit directive — placed last, so it is the freshest
		// instruction — telling the model the image IS attached and not to re-fetch it.
		if t, _ := seed["text"].(string); strings.TrimSpace(t) == "" {
			seed["text"] = composerReseedImageDirective
		} else {
			seed["text"] = t + "\n\n" + composerReseedImageDirective
		}
	}
	body := composerTurnBody(reseedSid, model, seed, advertise, toolChoice, extractComposerClientEnv(opts), constraints)
	return reseedSid, body, true
}

// composerResponseID returns a unique id per request. Clients that correlate streaming chunks to their
// non-stream counterpart (or dedupe by id) must not see the same id across unrelated responses.
func composerResponseID() string {
	return "chatcmpl-composer-" + composerRandHex(16)
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

// executeComposerStream drives one /agent/turn against the bridge and translates the
// bridge SSE events into the client's streaming wire format.
// composerApplyLeaseStop releases or extends the session lease based on the terminal stop_reason.
func composerApplyLeaseStop(tenant, sessionID, leaseStop string, leaseOwner uint64, ctx context.Context) {
	switch {
	case leaseStop == "tool_use":
		composerInflight.touch(tenant, sessionID, leaseOwner)
	case leaseStop != "":
		composerInflight.release(tenant, sessionID, leaseOwner)
		composerOwnership.forgetSession(tenant, sessionID)
	case ctx.Err() != nil:
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
	model       string
	responseID  string
	tenant      string
	contHint    composerContinuationHint
	oai         []byte
	defs        []cursorToolDefinition
	toolAliases map[string]string
	sessionID   string
	leaseOwner  uint64
}

// prepareComposerInbound validates and routes an inbound turn before POST /agent/turn.
func (e *CursorExecutor) prepareComposerInbound(auth *cliproxyauth.Auth, apiKey string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool) (composerInboundTurn, error) {
	var turn composerInboundTurn
	turn.model = resolveCursorModelName(resolveCursorModelAlias(auth, req.Model))
	turn.responseID = composerResponseID()
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	turn.oai = sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), stream)
	if errStore := composerRejectStoreFalse(turn.oai); errStore != nil {
		return turn, errStore
	}
	turn.tenant = composerTenant(auth, opts)
	turn.contHint = composerContinuationHintFor(turn.tenant, turn.oai)
	if lastUserTurnImageOnlyInvalid(gjson.GetBytes(turn.oai, "messages").Array(), turn.contHint) {
		return turn, errComposerImageOnlyInvalid
	}
	turn.defs = composerToolDefs(turn.oai)
	turn.toolAliases = composerToolAliases(auth)
	var err error
	turn.sessionID, turn.leaseOwner, err = deriveComposerSessionIDLive(auth, apiKey, turn.oai, opts)
	if err != nil {
		return turn, err
	}
	return turn, nil
}

// composerAgentTurnDial POSTs /agent/turn and applies P0-1 reseed-on-410. Mutates sessionID,
// leaseOwner, and body when reseed succeeds. The caller owns the returned response body.
func (e *CursorExecutor) composerAgentTurnDial(
	ctx context.Context,
	auth *cliproxyauth.Auth,
	apiKey, responseID, tenant string,
	sessionID *string,
	leaseOwner *uint64,
	body *[]byte,
	oai []byte,
	opts cliproxyexecutor.Options,
	contHint composerContinuationHint,
	model string,
	advertise []map[string]any,
	toolChoice string,
	constraints map[string]any,
	stream bool,
) (*http.Response, error) {
	httpResp, err := e.postAgentTurn(ctx, auth, apiKey, *body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode == composerBridgeStatusGone {
		if rsid, rbody, ok := composerReseedLostContinuation(tenant, apiKey, oai, opts, contHint, model, advertise, toolChoice, constraints); ok {
			if newOwner, claimed := composerInflight.claim(tenant, rsid); claimed {
				_, _ = io.Copy(io.Discard, io.LimitReader(httpResp.Body, composerBridgeMaxErrorBodyBytes))
				_ = httpResp.Body.Close()
				composerInflight.release(tenant, *sessionID, *leaseOwner)
				*sessionID, *leaseOwner, *body = rsid, newOwner, rbody
				if stream {
					log.Warnf("[composer %s] STREAM reseed-on-410: lost continuation re-seeded -> sid=%s", responseID, *sessionID)
				} else {
					log.Warnf("[composer %s] reseed-on-410: lost continuation re-seeded -> sid=%s", responseID, *sessionID)
				}
				httpResp, err = e.postAgentTurn(ctx, auth, apiKey, *body)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return httpResp, nil
}

// composerValidateAgentTurnPreStream rejects non-2xx and non-SSE bridge responses before SSE scanning.
// When closeBody is true the response body is closed on failure (streaming path).
func composerValidateAgentTurnPreStream(resp *http.Response, responseID string, stream, closeBody bool) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, composerBridgeMaxErrorBodyBytes))
		if closeBody {
			_ = resp.Body.Close()
		}
		corr := composerCorrelationID()
		if stream {
			log.Errorf("[composer %s] STREAM bridge NON-2xx corr=%s status=%d body=%s", responseID, corr, resp.StatusCode, sanitizeBridgeBody(errBody))
		} else {
			log.Errorf("[composer %s] bridge NON-2xx corr=%s status=%d body=%s", responseID, corr, resp.StatusCode, sanitizeBridgeBody(errBody))
		}
		return &composerBridgeStatusError{status: resp.StatusCode, correlation: corr}
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
	contHint := turn.contHint
	defs := turn.defs
	toolAliases := turn.toolAliases
	sessionID := turn.sessionID
	leaseOwner := turn.leaseOwner
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	// H16/#21 (C-RESPID): the outward-response-id -> sessionID mapping is recorded AFTER the bridge accepts a
	// valid SSE stream (below), NOT here — a dispatch that fails (transport / non-2xx / non-SSE) must not leave a
	// phantom mapping that a later previous_response_id would then resume onto a session that never ran.
	prep := prepareComposerAdvertise(oai, defs, toolAliases)
	advertise, toolChoice, constraints := prep.advertise, prep.toolChoice, prep.constraints
	// ADD-65: build input with the SAME tenant continuation hint deriveComposerSessionID used, so a Responses
	// server-side-chained [..., tool, user] turn is sent as tool_results (with userText) — not a fresh user turn
	// behind a paused run.
	inp := composerInputHinted(oai, contHint)
	body := composerTurnBody(sessionID, model, inp, advertise, toolChoice, extractComposerClientEnv(opts), constraints)
	composerDebugf("[composer %s] STREAM sessionID=%s inputType=%v toolChoice=%q advertise=%d -> POST /agent/turn", responseID, sessionID, inp["type"], toolChoice, len(advertise))
	composerDebugLogAdvertisedTools(responseID, advertise)

	httpResp, err := e.composerAgentTurnDial(ctx, auth, apiKey, responseID, tenant, &sessionID, &leaseOwner, &body, oai, opts, contHint, model, advertise, toolChoice, constraints, true)
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
			// No terminal AND not a disconnect: leave the lease for TTL self-heal (see composerApplyLeaseStop).
			composerApplyLeaseStop(tenant, sessionID, leaseStop, leaseOwner, ctx)
		}()

		// CC's auto-compact reads message.usage.input_tokens; the openai->claude translator hard-codes it to 0, so
		// inject the prompt estimate into message_start or CC never auto-compacts a composer session, however full.
		composerInputEstimate := composerEstimateTokens(promptChars)
		emit := func(srcChunks [][]byte) bool {
			for i := range srcChunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: composerSetMessageStartInputTokens(srcChunks[i], composerInputEstimate)}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, composerSSEMaxLineBytes) // ADD-68: align the single-line cap with the 64 MB body cap
		var param any
		toolIdx := 0
		evCount := 0         // P0-3: throttle in-stream lease touches (refresh the TTL on long single-turn streams)
		started := false     // whether any chunk has flowed (so the inbound schema's stream envelope is open)
		sawTerminal := false // ADD-88/96 (RBT-012): a turn_end (of ANY stop_reason) was observed before EOF
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
			// P0-3: a long single-turn stream (pure text/reasoning, no tool pause) can outlive the lease TTL;
			// refresh the lease periodically so a concurrent sibling still sees this run HELD and forks instead
			// of falsely re-attaching onto the live stream. Throttled so tenant-lock contention stays negligible.
			evCount++
			if evCount%512 == 0 {
				composerInflight.touch(tenant, sessionID, leaseOwner)
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
				composerOwnership.record(tenant, ev.Get("id").String(), sessionID) // route the continuation turn back here
				choice = map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []map[string]any{{
					"index": toolIdx, "id": ev.Get("id").String(), "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				}}}}
				toolIdx++
			case "turn_end":
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
				// The bridge reports upstream Cursor failures (auth/quota/network/run error) as
				// turn_end{stop_reason:"error"}. Propagate them as a real stream error instead of
				// masking them as a successful empty/partial "stop".
				if ev.Get("stop_reason").String() == "error" {
					emsg := ev.Get("error").String()
					if emsg == "" {
						emsg = "upstream Cursor run failed"
					}
					reporter.PublishFailure(ctx, fmt.Errorf("%s", emsg)) // record the reason, not just Failed=true
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("cursor composer: bridge turn failed: %s", emsg)}:
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
				// Client-facing keepalive (M24, C-KEEPALIVE). The bridge's own ": keepalive" SSE comment is
				// dropped above (we forward only "data: " lines), so the bridge emits a typed {"type":"ping"}
				// that we render into the INBOUND schema's keepalive frame here — resetting the client's idle
				// watchdog during a long or QUEUED turn, without injecting content. This keys on the inbound
				// SCHEMA (the "canonical -> per-provider" rule), never on client identity. No new timer: the
				// cadence is the bridge's existing SSE_KEEPALIVE_MS interval.
				fr := from.String()
				switch {
				case (fr == "claude" || fr == "anthropic") && started:
					// Anthropic requires the typed ping AFTER message_start. Later pings emit a real ping event;
					// the FIRST ping (started==false) falls through to the empty-delta path below, which the
					// translator turns into message_start (opening the envelope before any typed ping).
					if !emit([][]byte{[]byte("event: ping\ndata: {\"type\": \"ping\"}\n\n")}) {
						return
					}
					continue
				case (fr == "openai-response" || fr == "codex" || fr == "gemini" || fr == "gemini-cli") && started:
					// M24 (C-KEEPALIVE): for Responses/Codex and Gemini, an empty delta routed through the
					// OpenAI->schema translator renders to ZERO wire bytes (the Responses translator only emits
					// on non-empty content/tool_calls/finish_reason; Gemini emits nothing/an empty part), so the
					// keepalive was a NO-OP there. Emit a raw SSE COMMENT directly (bypassing the translator,
					// which would either drop it or, for Gemini, re-wrap it into a malformed "data: : keep-alive"
					// line). An SSE comment resets the client/proxy idle clock and is ignored by any compliant
					// SSE parser, never injecting a malformed response.*/candidates event. Only AFTER the
					// envelope is open (started) so it cannot precede response.created / the first candidate.
					if !emit([][]byte{[]byte(": keepalive\n\n")}) {
						return
					}
					continue
				}
				// Anthropic first ping AND every other schema's pre-envelope ping: a zero-content delta. Through
				// the per-schema translator it becomes message_start (Anthropic, opening the envelope before any
				// typed ping) or a benign empty chunk (OpenAI). Never a raw SSE comment routed through a
				// translator (which a per-format handler would re-wrap into a malformed line).
				choice = map[string]any{"index": 0, "delta": map[string]any{}}
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
			started = true
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
		if errScan := scanner.Err(); errScan != nil {
			// ERROR HONESTY (Comment 5): if the CLIENT disconnected, the request ctx is canceled and the scanner
			// read fails as a CONSEQUENCE — that is BENIGN, not a bridge fault. Do not log at error level and do
			// not publish a usage FAILURE for it (that inflates error metrics for ordinary disconnects). Just
			// stop; the deferred body-close runs, and leaseStop stays "" so the logical-run lease is left for the
			// TTL (the bridge run may still be alive — releasing it could collapse a sibling onto it).
			if ctx.Err() != nil {
				composerDebugf("[composer %s] STREAM ended on client disconnect (benign): %v", responseID, errScan)
				return
			}
			log.Errorf("[composer %s] STREAM scanner error: %v", responseID, errScan)
			reporter.PublishFailure(ctx, errScan)
			// Use the SAME ctx-aware send as the turn_end-error branch: if the client already disconnected
			// (ctx.Done fired), a bare `out <- ...` would block forever on the unbuffered channel and leak this
			// goroutine. The deferred body close still runs on return.
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
			return
		}
		// ADD-88 (Comment 1, RBT-012): a CLEAN bridge EOF that never delivered a terminal turn_end is a protocol
		// violation — a truncated/empty/non-SSE body (e.g. a 200 that closed the connection with no events).
		// Emitting a synthetic [DONE] here would hand the client a well-formed EMPTY completion (a false
		// success). Surface a protocol error instead. (A client disconnect exits the loop via emit()->return
		// above, so reaching here means the BRIDGE ended the stream without a terminal.)
		if !sawTerminal {
			protoErr := newComposerBridgeProtocolError(responseID, "bridge stream ended without a terminal turn_end", "")
			reporter.PublishFailure(ctx, protoErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: protoErr}:
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
		emit(termChunks)
	}()

	// ADD-80 (RBT-035): do NOT forward the bridge's transport headers (text/event-stream, Cache-Control, any
	// bridge/CDN/gateway headers) on the OUTBOUND stream. The per-schema STREAM handler already forces the
	// correct streaming Content-Type (text/event-stream) and WriteUpstreamHeaders never overwrites it, so we
	// return a minimal clean header set carrying no bridge transport headers rather than httpResp.Header.Clone().
	return &cliproxyexecutor.StreamResult{Headers: composerStreamResponseHeaders(), Chunks: out}, nil
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
	contHint := turn.contHint
	defs := turn.defs
	toolAliases := turn.toolAliases
	sessionID := turn.sessionID
	leaseOwner := turn.leaseOwner
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	// leaseStop mirrors the streaming path: free the session's logical-run lease on a terminal end, touch it on
	// a tool_use pause, and on no terminal ("") leave it for the TTL (the bridge run may still be alive). All
	// touch/release calls carry leaseOwner so only the claiming run can mutate the lease (P0-1: no clobber).
	leaseStop := ""
	defer func() { composerApplyLeaseStop(tenant, sessionID, leaseStop, leaseOwner, ctx) }()
	// H16/#21 (C-RESPID): the outward-response-id -> sessionID mapping is recorded AFTER the bridge accepts a
	// valid SSE stream (below), not here — a failed dispatch must leave no phantom mapping (mirrors the stream path).
	advertise := composerAdvertise(oai)
	toolChoice := resolveComposerToolChoice(oai, defs, toolAliases)
	constraints := composerConstraints(oai)
	if toolChoice == "none" {
		// tool_choice=none: advertise nothing so the model cannot call CLIENT tools. H08 (best-effort): the
		// `none`/`specific:` token is also forwarded to the bridge (composerTurnBody.toolChoice), which gates
		// the model's NATIVE built-ins via constraintInstructions + by rejecting native exec cases. Native
		// Cursor tools are SDK built-ins not in the advertise set, so they cannot be structurally
		// un-advertised here — the gating of native tools is best-effort and owned by the bridge.
		advertise = nil
	} else {
		// H07/H22: restrict to allowed_tools; never widen silently (empty intersection => explicit-unsupported).
		var unsupportedTools bool
		advertise, unsupportedTools = applyComposerAllowedTools(advertise, oai, defs, toolAliases)
		if unsupportedTools {
			constraints = appendUnsupportedGuarantee(constraints, "allowed_tools requested but none of the allowed tools are available (no tools advertised; do not call any tool)")
		}
	}
	// ADD-65: build input with the SAME tenant continuation hint deriveComposerSessionID used, so a Responses
	// server-side-chained [..., tool, user] turn is sent as tool_results (with userText) — not a fresh user turn
	// behind a paused run.
	inp := composerInputHinted(oai, contHint)
	body := composerTurnBody(sessionID, model, inp, advertise, toolChoice, extractComposerClientEnv(opts), constraints)

	httpResp, err := e.composerAgentTurnDial(ctx, auth, apiKey, responseID, tenant, &sessionID, &leaseOwner, &body, oai, opts, contHint, model, advertise, toolChoice, constraints, false)
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

	// #21 (C-RESPID): the bridge accepted a valid SSE stream -> NOW record outward-response-id -> sessionID, so a
	// failed dispatch (handled above) never leaves a phantom mapping a later previous_response_id could resume.
	recordComposerResponseSession(tenant, responseID, sessionID)
	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := make([]map[string]any, 0)
	finish := "stop"
	usageRaw := ""
	sawTerminal := false // ADD-88/96 (RBT-012): a turn_end (of ANY stop_reason) was observed before EOF
	evCount := 0         // P0-3: throttle in-stream lease touches (refresh the TTL on long single-turn streams)
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, composerSSEMaxLineBytes) // ADD-68: align the single-line cap with the 64 MB body cap
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data: "):])
		if string(payload) == "[DONE]" {
			break
		}
		// P0-3: refresh the lease on a long single-turn stream so a concurrent sibling still sees it HELD
		// (mirrors the streaming path). Throttled to keep tenant-lock contention negligible.
		evCount++
		if evCount%512 == 0 {
			composerInflight.touch(tenant, sessionID, leaseOwner)
		}
		// ADD-96 (RBT-013): fail closed on a non-JSON SSE frame rather than dropping it (a dropped text/tool
		// frame would truncate the answer as a clean success).
		if !gjson.ValidBytes(payload) {
			protoErr := newComposerBridgeProtocolError(responseID, "invalid JSON in bridge SSE frame", string(payload))
			reporter.PublishFailure(ctx, protoErr)
			return cliproxyexecutor.Response{}, protoErr
		}
		ev := gjson.ParseBytes(payload)
		switch ev.Get("type").String() {
		case "text":
			text.WriteString(ev.Get("delta").String())
		case "reasoning":
			reasoning.WriteString(ev.Get("delta").String())
		case "tool_call":
			name, args := mapComposerToolCall(ev.Get("name").String(), ev.Get("input"), defs, toolAliases)
			composerOwnership.record(tenant, ev.Get("id").String(), sessionID) // route the continuation turn back here
			toolCalls = append(toolCalls, map[string]any{
				"id": ev.Get("id").String(), "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		case "turn_end":
			sawTerminal = true // ADD-88/96 (RBT-012): terminal observed — EOF below is a clean end, not truncation
			// ISOLATION: record the terminal stop_reason for the deferred lease handler (tool_use pause -> touch;
			// any other terminal -> release). Never leave it "" once a turn_end is observed.
			leaseStop = ev.Get("stop_reason").String()
			if leaseStop == "" {
				leaseStop = "end_turn"
			}
			switch ev.Get("stop_reason").String() {
			case "tool_use":
				finish = "tool_calls"
			case "error":
				// Surface upstream Cursor failures instead of returning an empty "success".
				emsg := ev.Get("error").String()
				if emsg == "" {
					emsg = "upstream Cursor run failed"
				}
				reporter.PublishFailure(ctx, fmt.Errorf("%s", emsg)) // record the reason, not just Failed=true
				return cliproxyexecutor.Response{}, fmt.Errorf("cursor composer: bridge turn failed: %s", emsg)
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
	if errScan := scanner.Err(); errScan != nil {
		reporter.PublishFailure(ctx, errScan)
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor composer: read bridge stream: %w", errScan)
	}
	// ADD-88 (Comment 1, RBT-012): a clean EOF with no terminal turn_end is a truncated/empty bridge response,
	// not a successful turn. Returning the accumulated (empty) message here would be a false success — surface
	// a protocol error instead so the client is told the bridge failed rather than handed content:"" + stop.
	if !sawTerminal {
		protoErr := newComposerBridgeProtocolError(responseID, "bridge stream ended without a terminal turn_end", "")
		reporter.PublishFailure(ctx, protoErr)
		reporter.EnsurePublished(ctx)
		return cliproxyexecutor.Response{}, protoErr
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

// postAgentTurn POSTs a turn body to the bridge's /agent/turn endpoint (SSE response).
func (e *CursorExecutor) postAgentTurn(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, body []byte) (*http.Response, error) {
	// ADD-41/ADD-47: validate + structurally build the /agent/turn URL (reject userinfo/query in the base,
	// require https for non-local hosts) BEFORE sending any credential. A bad/insecure config fails here
	// with a typed error instead of leaking the Cursor key over a cleartext or mis-joined URL.
	turnURL, err := buildComposerTurnURL(auth)
	if err != nil {
		corr := composerCorrelationID()
		log.Errorf("[composer] postAgentTurn URL REJECTED corr=%s base=%s: %v", corr, redactBridgeURL(resolveComposerBridgeURL(auth)), err)
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, turnURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cursor composer: failed to create /agent/turn request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	// Multi-tenant bridge: X-Bridge-Auth gates access and the bearer above is the per-user Cursor key.
	// Single-tenant (no token): omitted — the bearer doubles as the bridge gate (must equal CURSOR_API_KEY).
	if token := resolveComposerBridgeToken(auth); token != "" {
		httpReq.Header.Set("X-Bridge-Auth", token)
	}
	httpReq.Header.Set("Accept", "text/event-stream")
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
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		// M25: redact the URL (userinfo + secret query params) before logging; a transport error string can
		// itself echo the dialed URL, so sanitize it too.
		corr := composerCorrelationID()
		log.Errorf("[composer] postAgentTurn TRANSPORT ERROR corr=%s to %s: %s", corr, redactBridgeURL(turnURL), sanitizeBridgeBody([]byte(err.Error())))
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, fmt.Errorf("cursor composer: /agent/turn request failed (correlation %s)", corr)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	return httpResp, nil
}
