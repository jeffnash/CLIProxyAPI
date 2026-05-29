package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
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
	return fmt.Sprintf("cursor composer: bridge /agent/turn failed with status %d (correlation %s)", e.status, e.correlation)
}

// StatusCode implements cliproxyexecutor.StatusError so the conductor/handler preserve the bridge status
// to the client (e.g. a 410 stays a 410, a 429 stays a 429) instead of collapsing every non-2xx to 500.
func (e *composerBridgeStatusError) StatusCode() int { return e.status }

// cursorDirectEnabled reports whether the gated, ToS-exposed direct Cursor path
// is explicitly opted into. Default (unset) is the safe Cursor Composer Client-Tools sidecar path.
func cursorDirectEnabled() bool {
	v := strings.TrimSpace(os.Getenv("CURSOR_DIRECT"))
	return v == "1" || strings.EqualFold(v, "true")
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
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
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
		if v == "1" || strings.EqualFold(v, "true") {
			return true
		}
	}
	v := strings.TrimSpace(os.Getenv("CURSOR_AGENT_BRIDGE_ALLOW_INSECURE"))
	return v == "1" || strings.EqualFold(v, "true")
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
// TODO(ADD-39): this store is PROCESS-LOCAL. Horizontally-scaled CLIProxy replicas do not share it, so a
// continuation that lands on a different replica than the emitter will miss ownership and fall back to the
// stable-conv/mint path. A shared low-latency store (with TTL) or sticky routing is required for multi-replica
// correctness; deferred per owner decision (single-instance deployment). The tenant+session keying here is the
// correct local foundation for that future shared store. Do NOT claim cross-replica ownership is solved.
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
	if _, ok := composerResponseSessions[key]; ok {
		composerResponseSessions[key] = sessionID
		return
	}
	composerResponseSessions[key] = sessionID
	composerResponseSessionOrder = append(composerResponseSessionOrder, key)
	if len(composerResponseSessionOrder) > composerResponseSessionCap {
		oldest := composerResponseSessionOrder[0]
		composerResponseSessionOrder = composerResponseSessionOrder[1:]
		delete(composerResponseSessions, oldest)
	}
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
	v := os.Getenv("CURSOR_COMPOSER_DEBUG")
	return v == "1" || v == "true"
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
	v := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_REPLAY_REASONING"))
	return v == "1" || strings.EqualFold(v, "true")
}()

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
	// Body signals that mirror extractSessionIDs steps 7+: a conversation_id or an OpenAI prompt_cache_key are
	// stable across a conversation's turns and never derived from message content. Never derive from message
	// text. H16: previous_response_id is DELIBERATELY EXCLUDED here — it is NOT stable across a conversation
	// (it changes every turn), so hashing it would mint a NEW session every turn and lose context. It is
	// instead resolved via the response-id->sessionID map in deriveComposerSessionID (recordComposerResponseSession).
	if len(opts.OriginalRequest) > 0 {
		for _, k := range []string{"conversation_id", "prompt_cache_key"} {
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

// recordComposerToolCall remembers which bridge session emitted a tool call (ADD-50/70/71). It delegates to
// the per-tenant ownership store, keyed by tenant + session + tool-call id.
func recordComposerToolCall(tenant, toolCallID, sessionID string) {
	composerOwnership.record(tenant, toolCallID, sessionID)
}

// forgetComposerSessionToolCalls drops a tenant's ownership entries for a finished bridge session (ADD-71).
func forgetComposerSessionToolCalls(tenant, sessionID string) {
	composerOwnership.forgetSession(tenant, sessionID)
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

// deriveComposerSessionID returns the bridge session id for this turn, scoped to the selected credential
// (tenant boundary) so different users never share a bridge session / SDK stateRoot. When a stable
// conversation identifier is present (request header, inbound metadata.user_id, body conv-id/cache-key, or
// CLIProxy session metadata) the id is derived deterministically from it. For a continuation (tool_results)
// turn it first routes by the session that emitted the pending tool calls (by tool_call_id). It NEVER routes
// by message content. When nothing stable is available it DEGRADES GRACEFULLY by minting a fresh session
// (the continuation carries history+system so the bridge re-seeds) instead of failing a routine turn — the
// error return is retained in the signature for callers but is no longer produced on a routine turn.
//
// C05 (too-long recovery): the EXTERNAL session id this function returns is the routing key only; it stays
// STABLE across a conversation so continuations keep routing here. The decoupling between this external id
// and the DURABLE Cursor agentId lives entirely in the bridge: on ERROR_CONVERSATION_TOO_LONG the bridge
// tombstones the poisoned durable agent and rotates session.agentId (e.g. <sid>_r2), forcing a re-seed —
// WITHOUT changing the external id. The executor must NOT mint a rotated id (that would fork routing and
// orphan continuations); it only needs to keep returning the same external id, which it does. The
// continuation carries history+system (composerInput) so the rotated durable agent re-seeds bounded context.
func deriveComposerSessionID(auth *cliproxyauth.Auth, oai []byte, opts cliproxyexecutor.Options) (string, error) {
	tenant := composerTenant(auth, opts)
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
					sum := sha256.Sum256([]byte(tenant + "\x00conv:" + id))
					disambig := "sess_" + hex.EncodeToString(sum[:])[:32]
					composerDebugf("[composer] deriveSessionID BRANCH=continuation(ambiguous id disambiguated by stable conv id) -> sessionID=%s", disambig)
					return disambig, nil
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
			sum := sha256.Sum256([]byte(tenant + "\x00conv:" + id))
			sid := "sess_" + hex.EncodeToString(sum[:])[:32]
			composerDebugf("[composer] deriveSessionID BRANCH=continuation(stable fallback) convID=%q -> sessionID=%s", id, sid)
			return sid, nil
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
		// ADD-62 invariant (C-ADD62-MODEL-ROTATE): the session id is derived from tenant + conversation ONLY,
		// never the model. A model change on the same conversation MUST keep the same external session id so the
		// BRIDGE can detect the change (session.model != requested) and rotate/re-seed the durable agent into the
		// new model. Folding the model into this hash would fork routing and orphan in-flight continuations.
		sum := sha256.Sum256([]byte(tenant + "\x00conv:" + id))
		sid := "sess_" + hex.EncodeToString(sum[:])[:32]
		composerDebugf("[composer] deriveSessionID BRANCH=stable convID=%q -> sessionID=%s", id, sid)
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

// mintComposerSessionID returns a fresh random session id (never derived from request content).
func mintComposerSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "sess_" + hex.EncodeToString(b[:])
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
const composerHistoryFingerprintHeadMessages = 2

// composerHistoryFingerprintPrefixRunes bounds how much of each head message's text contributes, so a huge
// pasted opener does not dominate and a later in-place EDIT of an early message still flips the hash.
const composerHistoryFingerprintPrefixRunes = 256

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
func composerHistoryFingerprint(messages []gjson.Result) string {
	nonSystem := make([]gjson.Result, 0, len(messages))
	for _, m := range messages {
		switch m.Get("role").String() {
		case "system", "developer":
			continue
		}
		nonSystem = append(nonSystem, m)
	}
	if len(nonSystem) == 0 {
		return ""
	}
	h := sha256.New()
	limit := len(nonSystem)
	if limit > composerHistoryFingerprintHeadMessages {
		limit = composerHistoryFingerprintHeadMessages
	}
	for i := 0; i < limit; i++ {
		m := nonSystem[i]
		h.Write([]byte(m.Get("role").String()))
		h.Write([]byte{0x1f})
		text := cursorMessageText(m)
		if r := []rune(text); len(r) > composerHistoryFingerprintPrefixRunes {
			text = string(r[:composerHistoryFingerprintPrefixRunes])
		}
		h.Write([]byte(text))
		h.Write([]byte{0})
	}
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

// lastUserTurnImageOnlyInvalid reports whether the request's LAST user message had image part(s), produced no
// valid extracted image, and carries no text (ADD-56). Continuations are out of scope here (a tool_results
// turn is classified/handled elsewhere and must never be rejected for a degenerate image) — this guards only
// the new-user turn path.
func lastUserTurnImageOnlyInvalid(messages []gjson.Result) bool {
	// Never reject a continuation: its tool output is the load-bearing content, not the (possibly empty) text.
	if _, _, ok := composerToolResults(messages); ok {
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
func renderComposerHistory(messages []gjson.Result, lastUserIdx int) string {
	if lastUserIdx <= 0 {
		return ""
	}
	var b strings.Builder
	appendLine := func(s string) {
		if s == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(s)
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
	return b.String()
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
	// H20: parallel_tool_calls:false is a documented zero-or-one-tool-per-turn limit. We cannot hard-cap
	// Cursor's emission, so carry the flag (the bridge renders an instruction) and mark it unsupported as a
	// hard guarantee. Only the explicit `false` is load-bearing; absent / true needs no signal.
	if v := gjson.GetBytes(oai, "parallel_tool_calls"); v.Exists() && !v.Bool() {
		c["parallelToolCalls"] = false
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
		if t.Get("function").Exists() {
			continue
		}
		kind := t.Get("type").String()
		if kind == "" || kind == "function" {
			continue // a bare/malformed function entry, not a built-in tool we should flag
		}
		unsupported = append(unsupported, "built-in tool "+kind+" (Cursor composer has no equivalent; it is not advertised or executed)")
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

// composerToolResults is the back-compat entry (no hint): branches (a)/(b) only. See composerToolResultsHinted.
func composerToolResults(messages []gjson.Result) (results []map[string]any, trailing string, ok bool) {
	return composerToolResultsHinted(messages, composerContinuationHint{})
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
	toolRes := func(m gjson.Result) map[string]any {
		r := map[string]any{"toolCallId": m.Get("tool_call_id").String(), "content": cursorMessageText(m)}
		// C5: a client tool the inbound marked failed must reach the model as a failure, not a clean success.
		// The oai role:tool message uses snake_case is_error; the bridge wire entry uses camelCase isError.
		if m.Get("is_error").Bool() {
			r["isError"] = true
		}
		// EX3: an image embedded inside a tool_result (role:tool content array) must not be dropped. The Cursor
		// tool-result protobuf cannot carry images directly, so the bridge folds these into a follow-up send.
		if imgs := extractComposerImages(m); len(imgs) > 0 {
			r["images"] = imgs
		}
		return r
	}
	// (a) Preferred: the role:tool results that follow the LAST assistant message bearing tool_calls, as long
	// as the model has not yet replied to them (no later assistant). This is what makes a MIXED turn (tool
	// results + trailing user text) classify as a continuation instead of a fresh user turn (Comment 4).
	lastTC := -1
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Get("role").String() == "assistant" && m.Get("tool_calls").Exists() {
			lastTC = i
			break
		}
	}
	if lastTC >= 0 {
		var sb strings.Builder
		res := make([]map[string]any, 0)
		replied := false
		for i := lastTC + 1; i < len(messages); i++ {
			switch messages[i].Get("role").String() {
			case "tool":
				res = append(res, toolRes(messages[i]))
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
			res = append([]map[string]any{toolRes(messages[i])}, res...)
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
				tr := toolRes(messages[i])
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
			last := results[len(results)-1]
			prev, _ := last["content"].(string)
			if reminder {
				last["content"] = strings.TrimRight(prev, "\n") + "\n\n[System reminder accompanying these tool results:]\n" + t
			} else {
				last["content"] = strings.TrimRight(prev, "\n") + "\n\n[The user also sent the following message alongside these tool results — treat it as their latest instruction:]\n" + t
				inp["userText"] = t // C1 (EX2): a real trailing user message, first-class so it is never lost.
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
func mapComposerToolCall(name string, input gjson.Result, defs []cursorToolDefinition, overrides map[string]string) (string, string) {
	args := map[string]any{}
	if input.Exists() && input.IsObject() {
		_ = json.Unmarshal([]byte(input.Raw), &args)
	}
	spec := resolveToolSpec(name, defs, overrides)
	if spec == nil {
		b, _ := json.Marshal(args)
		return name, string(b)
	}
	normalized := normalizeToolArguments(args, spec)
	b, _ := json.Marshal(normalized)
	return spec.Name, string(b)
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

// composerResponseID returns a unique id per request. Clients that correlate streaming chunks to their
// non-stream counterpart (or dedupe by id) must not see the same id across unrelated responses.
func composerResponseID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-composer-" + hex.EncodeToString(b[:])
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

// executeComposerStream drives one /agent/turn against the bridge and translates the
// bridge SSE events into the client's streaming wire format.
func (e *CursorExecutor) executeComposerStream(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	model := resolveCursorModelName(resolveCursorModelAlias(auth, req.Model))
	responseID := composerResponseID()
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), model, auth)

	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	oai := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), true)
	// ADD-56: an image-only turn whose every image is malformed/unsupported must NOT become a silent empty
	// turn (which the model would answer with irrelevant output). Reject it with a typed 400 instead.
	if lastUserTurnImageOnlyInvalid(gjson.GetBytes(oai, "messages").Array()) {
		log.Errorf("[composer %s] STREAM image-only turn has no valid image -> 400", responseID)
		return nil, errComposerImageOnlyInvalid
	}
	defs := composerToolDefs(oai)
	toolAliases := composerToolAliases(auth)
	tenant := composerTenant(auth, opts)
	sessionID, err := deriveComposerSessionID(auth, oai, opts)
	if err != nil {
		log.Errorf("[composer %s] STREAM deriveSessionID ERROR (-> 500): %v", responseID, err)
		return nil, err
	}
	// H16 (C-RESPID): record outward-response-id -> sessionID up front (the id is fixed for the turn and is
	// exactly what the responses-response translator surfaces as response.id). A later follow-up that passes
	// it back as previous_response_id then resumes THIS durable session. Scoped by tenant so it cannot leak
	// across users sharing one upstream Cursor key.
	recordComposerResponseSession(tenant, responseID, sessionID)
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
		// H07/H22: restrict the advertise set to the client's allowed_tools allow-list (if any). Never widen
		// silently — an empty intersection is surfaced as explicit-unsupported (advertise nothing) so the
		// model is told the requested tools are unavailable rather than handed unrelated ones.
		var unsupportedTools bool
		advertise, unsupportedTools = applyComposerAllowedTools(advertise, oai, defs, toolAliases)
		if unsupportedTools {
			constraints = appendUnsupportedGuarantee(constraints, "allowed_tools requested but none of the allowed tools are available (no tools advertised; do not call any tool)")
		}
	}
	// ADD-65: build input with the SAME tenant continuation hint deriveComposerSessionID used, so a Responses
	// server-side-chained [..., tool, user] turn is sent as tool_results (with userText) — not a fresh user turn
	// behind a paused run.
	inp := composerInputHinted(oai, composerContinuationHintFor(tenant, oai))
	body := composerTurnBody(sessionID, model, inp, advertise, toolChoice, extractComposerClientEnv(opts), constraints)
	composerDebugf("[composer %s] STREAM sessionID=%s inputType=%v toolChoice=%q advertise=%d -> POST /agent/turn", responseID, sessionID, inp["type"], toolChoice, len(advertise))
	if composerDebugEnabled {
		// Log the ADVERTISED tool names (+ how many lost their schema). This is the only way to tell whether a
		// harness tool the model should call (e.g. Task/Agent for subagents) is actually offered to composer,
		// vs. dropped upstream — counts alone hide it. A tool with a mangled/absent inputSchema is one the model
		// will typically refuse to call.
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

	httpResp, err := e.postAgentTurn(ctx, auth, apiKey, body)
	if err != nil {
		log.Errorf("[composer %s] STREAM postAgentTurn ERROR (-> 500): %v", responseID, err)
		return nil, err
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// ADD-46: bound the diagnostic read so a hostile/faulty bridge cannot make us allocate an unbounded
		// error body. The returned error carries only a correlation id, not the body.
		errBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, composerBridgeMaxErrorBodyBytes))
		_ = httpResp.Body.Close()
		// M25: keep a REDACTED diagnostic in the logs (status + sanitized body) and return a SHORT GENERIC
		// client error carrying only a correlation id — never the raw body (it may carry crsr_/sk-/bearer/
		// signed-url/bridge tokens) and never the bridge URL with credentials.
		// ADD-59 (C-ADD59-TYPED-STATUS): this check runs BEFORE the goroutine opens the stream / commits
		// headers, so a typed StatusError propagates as a REAL client HTTP status (a 410 lost-continuation
		// stays a 410, not a generic 500). The bridge's 410 semantics are preserved end-to-end.
		corr := composerCorrelationID()
		log.Errorf("[composer %s] STREAM bridge NON-2xx corr=%s status=%d body=%s", responseID, corr, httpResp.StatusCode, sanitizeBridgeBody(errBody))
		return nil, &composerBridgeStatusError{status: httpResp.StatusCode, correlation: corr}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("cursor composer: close bridge response body error: %v", errClose)
			}
		}()
		defer reporter.EnsurePublished(ctx)

		emit := func(srcChunks [][]byte) bool {
			for i := range srcChunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: srcChunks[i]}:
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
		started := false // whether any chunk has flowed (so the inbound schema's stream envelope is open)
		for scanner.Scan() {
			line := scanner.Bytes()
			if !bytes.HasPrefix(line, []byte("data: ")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data: "):])
			if string(payload) == "[DONE]" {
				break
			}
			ev := gjson.ParseBytes(payload)
			var choice map[string]any
			switch ev.Get("type").String() {
			case "text":
				choice = map[string]any{"index": 0, "delta": map[string]any{"content": ev.Get("delta").String()}}
			case "reasoning":
				choice = map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": ev.Get("delta").String()}}
			case "tool_call":
				rawName := ev.Get("name").String()
				name, args := mapComposerToolCall(rawName, ev.Get("input"), defs, toolAliases)
				composerDebugf("[composer %s] STREAM tool_call emitted by model: raw=%q -> mapped=%q id=%s", responseID, rawName, name, ev.Get("id").String())
				recordComposerToolCall(tenant, ev.Get("id").String(), sessionID) // route the continuation turn back here
				choice = map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []map[string]any{{
					"index": toolIdx, "id": ev.Get("id").String(), "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				}}}}
				toolIdx++
			case "turn_end":
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
				continue
			}
			chunkLine := oaiChunk(responseID, model, choice)
			srcChunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), oai, chunkLine, &param)
			if !emit(srcChunks) {
				return
			}
			started = true
		}
		if errScan := scanner.Err(); errScan != nil {
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
		// Clean end of the bridge stream. The bridge's turn_end carries finish_reason but no usage, and its
		// [DONE] is consumed above, so the inbound schema's TERMINAL is not emitted yet: the OpenAI->Anthropic
		// translator sends message_delta+message_stop only on usage or [DONE], and Responses defers
		// response.completed to [DONE]. Forward a synthetic [DONE] through the SAME translator so every schema
		// gets a well-formed terminal (Anthropic message_stop / Responses response.completed). OpenAI is a
		// no-op here (its passthrough returns nothing for [DONE]; the OpenAI handler adds its own).
		termChunks := sdktranslator.TranslateStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), oai, []byte("data: [DONE]"), &param)
		emit(termChunks)
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// executeComposer drives one /agent/turn and accumulates the bridge stream into a
// single non-streaming response.
func (e *CursorExecutor) executeComposer(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	model := resolveCursorModelName(resolveCursorModelAlias(auth, req.Model))
	// One response id for the whole turn: it is both the body id (resp["id"]) the client sees as response.id
	// AND the H16 (C-RESPID) map key. Minting it separately at body-build time would let the recorded key
	// drift from the client-visible id, breaking previous_response_id resume.
	responseID := composerResponseID()
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), model, auth)
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	oai := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), false)
	// ADD-56: reject an image-only turn whose every image is invalid (see the streaming path for rationale).
	if lastUserTurnImageOnlyInvalid(gjson.GetBytes(oai, "messages").Array()) {
		log.Errorf("[composer %s] image-only turn has no valid image -> 400", responseID)
		return cliproxyexecutor.Response{}, errComposerImageOnlyInvalid
	}
	defs := composerToolDefs(oai)
	toolAliases := composerToolAliases(auth)
	tenant := composerTenant(auth, opts)
	sessionID, err := deriveComposerSessionID(auth, oai, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	// H16 (C-RESPID): record outward-response-id -> sessionID under the SAME id used in resp["id"] below.
	recordComposerResponseSession(tenant, responseID, sessionID)
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
	inp := composerInputHinted(oai, composerContinuationHintFor(tenant, oai))
	body := composerTurnBody(sessionID, model, inp, advertise, toolChoice, extractComposerClientEnv(opts), constraints)

	httpResp, err := e.postAgentTurn(ctx, auth, apiKey, body)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("cursor composer: close bridge response body error: %v", errClose)
		}
	}()
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		// ADD-46: bound the diagnostic read (see the streaming path for rationale).
		errBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, composerBridgeMaxErrorBodyBytes))
		// M25: redacted diagnostic in logs; short generic client error with a correlation id (never the raw body).
		// ADD-59 (C-ADD59-TYPED-STATUS): typed StatusError preserves the bridge status to the client (a 410
		// lost-continuation stays a 410). The non-stream path has not committed any response yet here.
		corr := composerCorrelationID()
		log.Errorf("[composer %s] bridge NON-2xx corr=%s status=%d body=%s", responseID, corr, httpResp.StatusCode, sanitizeBridgeBody(errBody))
		return cliproxyexecutor.Response{}, &composerBridgeStatusError{status: httpResp.StatusCode, correlation: corr}
	}

	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := make([]map[string]any, 0)
	finish := "stop"
	usageRaw := ""
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
		ev := gjson.ParseBytes(payload)
		switch ev.Get("type").String() {
		case "text":
			text.WriteString(ev.Get("delta").String())
		case "reasoning":
			reasoning.WriteString(ev.Get("delta").String())
		case "tool_call":
			name, args := mapComposerToolCall(ev.Get("name").String(), ev.Get("input"), defs, toolAliases)
			recordComposerToolCall(tenant, ev.Get("id").String(), sessionID) // route the continuation turn back here
			toolCalls = append(toolCalls, map[string]any{
				"id": ev.Get("id").String(), "type": "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
		case "turn_end":
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
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		reporter.PublishFailure(ctx, errScan)
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor composer: read bridge stream: %w", errScan)
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
	if usageRaw != "" {
		if detail, ok := helps.ParseOpenAIStreamUsage([]byte(`{"usage":` + usageRaw + `}`)); ok {
			resp["usage"] = json.RawMessage(usageRaw)
			reporter.Publish(ctx, detail)
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
	return cliproxyexecutor.Response{Payload: []byte(out), Headers: httpResp.Header.Clone()}, nil
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
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
