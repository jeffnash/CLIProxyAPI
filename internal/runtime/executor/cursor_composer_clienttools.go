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

// composerToolCallSessions maps an emitted tool-call id -> the bridge session that produced it, so a
// continuation (tool_results) turn that carries no stable conversation id can still be routed back to the
// right session by the tool_call_id the client echoes. Bounded (FIFO) so it cannot grow without limit.
var (
	composerToolCallMu       sync.Mutex
	composerToolCallSessions = make(map[string]string)
	composerToolCallOrder    []string
)

const composerToolCallMapCap = 20000

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
		// extractSessionIDs / copilot_headers honor (Codex Session_id, Amp thread id, PI client-request id, a
		// bare Conversation_id) so a non-Anthropic agentic client keeps a stable session across its turns (EX5).
		for _, h := range []string{
			"X-Conversation-Id", "X-Session-Id", "X-Cc-Conversation-Id",
			"Session_id", "Conversation_id", "X-Amp-Thread-Id", "X-Client-Request-Id",
		} {
			if v := strings.TrimSpace(opts.Headers.Get(h)); v != "" {
				return v
			}
		}
	}
	if id := claudeSessionID(opts.OriginalRequest); id != "" {
		return id
	}
	// Body signals that mirror extractSessionIDs steps 7+ and the Responses continuation key: a
	// conversation_id, an OpenAI prompt_cache_key, or a Responses previous_response_id are all stable across a
	// conversation's turns and never derived from message content. Never derive from message text.
	if len(opts.OriginalRequest) > 0 {
		for _, k := range []string{"conversation_id", "prompt_cache_key", "previous_response_id"} {
			if v := strings.TrimSpace(gjson.GetBytes(opts.OriginalRequest, k).String()); v != "" {
				return v
			}
		}
	}
	if opts.Metadata != nil {
		for _, k := range []string{"conversation_id", "conversationId", "session_id", "sessionId", "request_id", "requestId"} {
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

// recordComposerToolCall remembers which bridge session emitted a tool call, so a later continuation turn
// can be routed by the tool_call_id the client echoes back even if it carries no stable conversation id.
func recordComposerToolCall(tenant, toolCallID, sessionID string) {
	if toolCallID == "" || sessionID == "" {
		return
	}
	// Scope the key by tenant: an SDK-supplied tool_call_id has no cross-tenant uniqueness guarantee, so a
	// bare key could let one tenant's continuation resolve to another's session. The tenant prefix matches
	// the session-derivation pre-image so within one tenant routing is unchanged.
	key := tenant + "\x00" + toolCallID
	composerToolCallMu.Lock()
	defer composerToolCallMu.Unlock()
	if _, ok := composerToolCallSessions[key]; ok {
		// Comment 5: refresh to the LATEST emitting session (a re-emitted tool_call_id moves to whichever
		// session emitted it most recently), avoiding a stale first-writer mapping. The key already exists
		// in the LRU order, so don't re-append.
		composerToolCallSessions[key] = sessionID
		return
	}
	composerToolCallSessions[key] = sessionID
	composerToolCallOrder = append(composerToolCallOrder, key)
	if len(composerToolCallOrder) > composerToolCallMapCap {
		oldest := composerToolCallOrder[0]
		composerToolCallOrder = composerToolCallOrder[1:]
		delete(composerToolCallSessions, oldest)
	}
}

// lookupSessionByToolResults returns the session id for a continuation turn whose trailing tool messages
// carry tool_call_ids previously emitted by a bridge session FOR THIS TENANT, or "" if none match.
func lookupSessionByToolResults(tenant string, oai []byte) string {
	results, _, ok := composerToolResults(gjson.GetBytes(oai, "messages").Array())
	if !ok {
		return ""
	}
	composerToolCallMu.Lock()
	defer composerToolCallMu.Unlock()
	for _, r := range results {
		id, _ := r["toolCallId"].(string)
		if id = strings.TrimSpace(id); id != "" {
			if sid, ok := composerToolCallSessions[tenant+"\x00"+id]; ok {
				return sid
			}
		}
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

// deriveComposerSessionID returns the bridge session id for this turn, scoped to the selected credential
// (tenant boundary) so different users never share a bridge session / SDK stateRoot. When a stable
// conversation identifier is present (request header, inbound metadata.user_id, body conv-id/cache-key, or
// CLIProxy session metadata) the id is derived deterministically from it. For a continuation (tool_results)
// turn it first routes by the session that emitted the pending tool calls (by tool_call_id). It NEVER routes
// by message content. When nothing stable is available it DEGRADES GRACEFULLY by minting a fresh session
// (the continuation carries history+system so the bridge re-seeds) instead of failing a routine turn — the
// error return is retained in the signature for callers but is no longer produced on a routine turn.
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
	if isComposerUtilityOneShot(oai) {
		sid := mintComposerSessionID()
		composerDebugf("[composer] deriveSessionID BRANCH=ephemeral(utility one-shot) -> sessionID=%s", sid)
		return sid, nil
	}
	// Comment 5: a tool_results continuation routes by tool_call_id OWNERSHIP first — the session that
	// actually emitted the pending tool calls is authoritative, ahead of the stable conversation id. This
	// prevents a continuation whose tools were emitted under a different session (isolation/race) from being
	// silently mis-routed to the conv-id session (which would never match the pending tool calls).
	if _, _, isCont := composerToolResults(gjson.GetBytes(oai, "messages").Array()); isCont {
		if sid := lookupSessionByToolResults(tenant, oai); sid != "" {
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
		sid := mintComposerSessionID()
		composerDebugf("[composer] deriveSessionID BRANCH=continuation(mint+re-seed, no emitter/no stable id) -> sessionID=%s", sid)
		return sid, nil
	}
	if id := stableConversationID(opts); id != "" {
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
// must be isolated from the conversation's stable Cursor session. It is a single STRUCTURAL, schema-neutral
// rule — not keyed on client identity: a request that advertises NO tools cannot participate in the
// Client-Tools flow, so it is a standalone text completion, never part of an agentic conversation thread.
// This generalizes across clients: title generation, topic/"isNewTopic" detection, quota probes, and
// conversation summaries (Claude Code, OpenCode, Crush, Gemini CLI, …) are all tool-less calls that some
// clients fire CONCURRENTLY with the real turn under the same conversation id; routing them to the
// conversation's stable session would collide with (and pollute) the in-flight agentic turn. The only
// exclusion is a tool_results continuation, which carries no advertised tools yet MUST route to the session
// that emitted its pending tool calls. (Earlier this also required "no assistant history", but that missed
// history-carrying summary calls and risked misclassifying nothing important — a tool-less turn has no
// durable agent state to preserve and continuations still resolve via tool_call_id.)
func isComposerUtilityOneShot(oai []byte) bool {
	if len(composerToolDefs(oai)) > 0 {
		return false // an agentic turn advertises tools — never isolate it
	}
	if _, _, ok := composerToolResults(gjson.GetBytes(oai, "messages").Array()); ok {
		return false // a tool_results continuation must route to its emitting session, not a fresh one
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
		var b strings.Builder
		for _, part := range c.Array() {
			if t := part.Get("text"); t.Exists() {
				b.WriteString(t.String())
			}
		}
		return b.String()
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

// composerHistoryFingerprint returns a 32-hex fingerprint anchored on the conversation's FIRST non-system
// message (role + text). It is GROWTH-STABLE: a normal multi-turn conversation only APPENDS turns, so the
// opener never changes and the fingerprint stays constant turn-to-turn. It changes only when the client
// REPLACES the head of history — e.g. a /compact summary supplanting the original opening turn — which is
// exactly when the bridge must re-seed (C2). Hashing the WHOLE history instead would change every turn and
// force a full history re-seed each turn, racing the very ERROR_CONVERSATION_TOO_LONG it aims to avoid.
// Empty when there is no non-system content (nothing to fingerprint).
func composerHistoryFingerprint(messages []gjson.Result) string {
	for _, m := range messages {
		switch m.Get("role").String() {
		case "system", "developer":
			continue
		}
		h := sha256.New()
		h.Write([]byte(m.Get("role").String()))
		h.Write([]byte{0})
		h.Write([]byte(cursorMessageText(m)))
		return hex.EncodeToString(h.Sum(nil))[:32]
	}
	return ""
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
		u := part.Get("image_url.url").String()
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
					call += "(" + args + ")"
				}
				if id := tc.Get("id").String(); id != "" {
					call = id + ":" + call
				}
				calls = append(calls, call)
			}
			// EX9: preserve the model's prior reasoning on a stateless re-seed (bounded so a long chain of
			// thought cannot blow the per-message limit).
			reasoning := truncateCursorToolResultForHistory(m.Get("reasoning_content").String())
			if txt == "" && reasoning == "" && len(calls) == 0 {
				continue
			}
			line := "ASSISTANT:"
			if txt != "" {
				line += " " + txt
			}
			if reasoning != "" {
				line += " [thinking: " + reasoning + "]"
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

// composerConstraints collects the enforced response constraints (response_format / stop / token limit)
// as explicit bridge fields. tool_choice is carried separately (it also gates tool advertisement).
func composerConstraints(oai []byte) map[string]any {
	c := map[string]any{}
	if rf := extractComposerResponseFormat(oai); rf != nil {
		c["responseFormat"] = rf
	}
	if stop := extractComposerStop(oai); len(stop) > 0 {
		c["stop"] = stop
	}
	if mt := extractComposerMaxTokens(oai); mt > 0 {
		c["maxTokens"] = mt
	}
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

// composerInput classifies the incoming turn: a trailing tool message means the client returned tool
// results (continuation); otherwise it is a new user turn carrying text + images + system + history.
// composerToolResults extracts a tool_results continuation from the (OpenAI-shape) messages: the role:tool
// results that follow the LAST assistant message bearing tool_calls, provided the model has not yet replied
// to them (no assistant message after them). ok=false when the turn is not such a continuation. trailing is
// any user text that accompanies the results in the same turn (a mixed tool_result+text turn). This is the
// single source of continuation detection (composerInput, lookupSessionByToolResults, deriveComposerSessionID
// all use it) so a mixed turn is never misclassified as a fresh user turn (Comment 4).
func composerToolResults(messages []gjson.Result) (results []map[string]any, trailing string, ok bool) {
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
	return nil, "", false
}

func composerInput(oai []byte) map[string]any {
	messages := gjson.GetBytes(oai, "messages").Array()
	// Tool_results continuation detection (Comment 4): a continuation is the LAST assistant turn bearing
	// tool_calls, followed by role:tool results the model has NOT yet replied to (no later assistant). Keying
	// only on "the last message is role:tool" misclassifies a MIXED turn carrying tool_result blocks AND
	// trailing user text (which translates to a trailing role:user) as a fresh user turn, stranding the
	// paused run's tools. Collect the results regardless of trailing user text; carry that text intentionally.
	if results, trailing, ok := composerToolResults(messages); ok {
		inp := map[string]any{"type": "tool_results", "results": results}
		// A continuation. A MIXED turn carries tool_results AND a trailing user message in the SAME turn — this
		// is normal client behavior (e.g. Claude Code when you interrupt a running tool, or background a task,
		// and then type a new message; it bundles the results for what finished WITH your new text). Anthropic
		// answers both in a SINGLE assistant turn. The @cursor/sdk has no mid-resume user-message seam, so fold
		// the user's message into the LAST tool result's content (clearly delimited) so the model reads it when
		// it resumes — AND carry it as a first-class userText (C1) so the bridge can still answer it when no
		// pending matched / the run was torn down. We must NEVER error here: erroring 500s a routine turn.
		if t := strings.TrimSpace(trailing); t != "" && len(results) > 0 {
			// EX1: a trailing block that is purely an auto-injected <system-reminder> is context, not the user's
			// instruction. Frame it neutrally and do NOT set userText (a reminder must not drive a fresh send).
			reminder := isPureSystemReminder(t)
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
		inp["text"] = cursorMessageText(messages[lastUserIdx])
		if imgs := extractComposerImages(messages[lastUserIdx]); len(imgs) > 0 {
			inp["images"] = imgs
		}
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
	if input["type"] == "user" && len(advertise) > 0 {
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
	defs := composerToolDefs(oai)
	toolAliases := composerToolAliases(auth)
	tenant := composerTenant(auth, opts)
	sessionID, err := deriveComposerSessionID(auth, oai, opts)
	if err != nil {
		log.Errorf("[composer %s] STREAM deriveSessionID ERROR (-> 500): %v", responseID, err)
		return nil, err
	}
	advertise := composerAdvertise(oai)
	toolChoice := resolveComposerToolChoice(oai, defs, toolAliases)
	if toolChoice == "none" {
		advertise = nil // tool_choice=none: advertise nothing so the model cannot call tools
	}
	inp := composerInput(oai)
	body := composerTurnBody(sessionID, model, inp, advertise, toolChoice, extractComposerClientEnv(opts), composerConstraints(oai))
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
		errBody, _ := io.ReadAll(httpResp.Body)
		_ = httpResp.Body.Close()
		log.Errorf("[composer %s] STREAM bridge NON-2xx (-> 500) status=%d sessionID=%s body=%s", responseID, httpResp.StatusCode, sessionID, string(errBody))
		return nil, fmt.Errorf("cursor composer: bridge /agent/turn failed with status %d: %s", httpResp.StatusCode, string(errBody))
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
		scanner.Buffer(nil, 52_428_800)
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
				// Client-facing keepalive. The bridge's own ": keepalive" SSE comment is dropped above (we
				// forward only "data: " lines), so the bridge emits a typed {"type":"ping"} that we render
				// into the INBOUND schema's keepalive frame here — resetting the client's idle watchdog during
				// a long or QUEUED turn, without injecting content. This keys on the inbound SCHEMA (the
				// "canonical -> per-provider" rule), never on client identity. Anthropic requires the typed
				// ping AFTER message_start, so the first ping lazily opens the envelope (an empty delta ->
				// message_start) and later pings emit a real ping event; for OpenAI an empty delta is itself a
				// valid no-op chunk. NOTE: for Gemini-family and OpenAI-Responses inbound, an empty delta
				// currently translates to zero wire bytes (after the first), so the keepalive is a no-op there
				// — acceptable for the Claude-Code-centric composer path; revisit if those become hot routes.
				if fr := from.String(); (fr == "claude" || fr == "anthropic") && started {
					if !emit([][]byte{[]byte("event: ping\ndata: {\"type\": \"ping\"}\n\n")}) {
						return
					}
					continue
				}
				// Anthropic first ping AND every non-Anthropic schema: a zero-content delta. Through the
				// per-schema translator it becomes message_start (Anthropic, opening the envelope before any
				// typed ping), a benign empty chunk (OpenAI), or a schema no-op — never a raw SSE comment,
				// which a per-format handler (e.g. Gemini's writeGeminiSSEData) would re-wrap into a malformed
				// "data: : keep-alive" line.
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
			out <- cliproxyexecutor.StreamChunk{Err: errScan}
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
	reporter := helps.NewUsageReporter(ctx, e.Identifier(), model, auth)
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	oai := sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(req.Payload), false)
	defs := composerToolDefs(oai)
	toolAliases := composerToolAliases(auth)
	tenant := composerTenant(auth, opts)
	sessionID, err := deriveComposerSessionID(auth, oai, opts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}
	advertise := composerAdvertise(oai)
	toolChoice := resolveComposerToolChoice(oai, defs, toolAliases)
	if toolChoice == "none" {
		advertise = nil // tool_choice=none: advertise nothing so the model cannot call tools
	}
	inp := composerInput(oai)
	body := composerTurnBody(sessionID, model, inp, advertise, toolChoice, extractComposerClientEnv(opts), composerConstraints(oai))

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
		errBody, _ := io.ReadAll(httpResp.Body)
		return cliproxyexecutor.Response{}, fmt.Errorf("cursor composer: bridge /agent/turn failed with status %d: %s", httpResp.StatusCode, string(errBody))
	}

	var text strings.Builder
	var reasoning strings.Builder
	toolCalls := make([]map[string]any, 0)
	finish := "stop"
	usageRaw := ""
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(nil, 52_428_800)
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
		"id": composerResponseID(), "object": "chat.completion", "created": time.Now().Unix(), "model": model,
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

// postAgentTurn POSTs a turn body to the bridge's /agent/turn endpoint (SSE response).
func (e *CursorExecutor) postAgentTurn(ctx context.Context, auth *cliproxyauth.Auth, apiKey string, body []byte) (*http.Response, error) {
	url := strings.TrimRight(resolveComposerBridgeURL(auth), "/") + composerAgentTurnPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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
	if isLoopbackBridgeURL(url) {
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
		log.Errorf("[composer] postAgentTurn TRANSPORT ERROR to %s: %v", url, err)
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, fmt.Errorf("cursor composer: /agent/turn request failed: %w", err)
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	return httpResp, nil
}
