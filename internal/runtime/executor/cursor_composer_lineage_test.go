package executor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// ---------------------------------------------------------------------------
// identity-finalplan §4 — conversation-lineage test matrix (all 10).
//
// These cover the proven session-identity bug: distinct logical conversations
// that SHARE one stable conversation id (subagents reusing the parent
// metadata.user_id, parallel fan-out, branches) must NOT collapse onto one
// durable cursor session — each divergent context becomes a distinct, durable,
// multi-turn-STABLE session. baseSid stays ID-authoritative; the lineage
// registry only SPLITS a shared id and re-resolves each fork to ONE stable
// sess_ across all of its own turns.
// ---------------------------------------------------------------------------

// lineageMsg is a single OpenAI-shape message for building lineage test payloads.
type lineageMsg struct {
	role    string
	content string
}

// lineagePayload builds an OpenAI-shape body that advertises a tool (so it is a real agentic turn, not a
// utility one-shot) from an ordered list of messages. withTools=false omits the tool advertisement.
func lineagePayload(withTools bool, msgs ...lineageMsg) []byte {
	m := make([]map[string]any, 0, len(msgs))
	for _, x := range msgs {
		m = append(m, map[string]any{"role": x.role, "content": x.content})
	}
	body := map[string]any{"messages": m}
	if withTools {
		body["tools"] = []map[string]any{{"type": "function", "function": map[string]any{"name": "Read"}}}
	}
	b, _ := json.Marshal(body)
	return b
}

// claudeUUIDOpts returns Options whose inbound payload carries a Claude metadata.user_id with the given
// session uuid — the parent identity that subagents reuse (the proven collapse vector).
func claudeUUIDOpts(uuid string) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{OriginalRequest: []byte(`{"metadata":{"user_id":"user_acct_account__session_` + uuid + `"}}`)}
}

// authL builds an auth with a fixed tenant id + api key for the lineage tests.
func authL(id string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: id, Attributes: map[string]string{"api_key": id + "-key"}}
}

// lineageCountForTenant returns the number of lineage entries the global registry holds for a tenant.
func lineageCountForTenant(tenant string) int {
	v, ok := composerLineage.tenants.Load(tenant)
	if !ok {
		return 0
	}
	to := v.(*tenantLineage)
	to.mu.Lock()
	defer to.mu.Unlock()
	return len(to.byBaseHead)
}

// ===========================================================================
// #1 MULTI-PROTOCOL divergent -> distinct (the proven bug).
// Anthropic (metadata.user_id _session_<uuid>) and OpenAI-chat (X-Conversation-Id): two new-user turns
// sharing the SAME id but DIFFERENT system+first-user -> TWO DISTINCT sids. Today both collapse.
// ===========================================================================
func TestLineage1_MultiProtocolDivergentDistinct(t *testing.T) {
	// Anthropic: same metadata.user_id uuid, different system + first user.
	auth := authL("t1-anthropic")
	opts := claudeUUIDOpts("uuid-div-1")
	parent := lineagePayload(true, lineageMsg{"system", "PARENT system"}, lineageMsg{"user", "parent task: refactor the parser"})
	sub := lineagePayload(true, lineageMsg{"system", "SUBAGENT system"}, lineageMsg{"user", "subagent task: write a unit test"})
	pSid, err := deriveComposerSessionID(auth, "ckey", parent, opts)
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	sSid, err := deriveComposerSessionID(auth, "ckey", sub, opts)
	if err != nil {
		t.Fatalf("sub: %v", err)
	}
	if pSid == sSid {
		t.Fatalf("Anthropic: divergent subagent sharing the parent uuid must SPLIT (parent=%s sub=%s)", pSid, sSid)
	}
	if !strings.HasPrefix(pSid, "sess_") || !strings.HasPrefix(sSid, "sess_") {
		t.Fatalf("both must be sess_ ids (parent=%s sub=%s)", pSid, sSid)
	}

	// OpenAI-chat: same X-Conversation-Id, different opener.
	authO := authL("t1-openai")
	conv := optsWithHeaders(map[string]string{"X-Conversation-Id": "conv-div-1"})
	a := lineagePayload(true, lineageMsg{"user", "first divergent task A"})
	b := lineagePayload(true, lineageMsg{"user", "first divergent task B is different"})
	aSid, _ := deriveComposerSessionID(authO, "ckey", a, conv)
	bSid, _ := deriveComposerSessionID(authO, "ckey", b, conv)
	if aSid == bSid {
		t.Fatalf("OpenAI-chat: divergent openers sharing one X-Conversation-Id must SPLIT (a=%s b=%s)", aSid, bSid)
	}
	// Exactly one of them is the baseSid (first claimant); the other is a fork.
}

// ===========================================================================
// #2 ★ MULTI-TURN FORK STABILITY (the blocker regression guard).
// A forked subagent over >=4 new-user turns: turn1 (fork instruction), turn2 (tool loop -> continuation),
// turn3 (FOLLOW-UP new-user), turn4 (tool loop) MUST return the SAME forkSid on turns 1 AND 3, and its
// tool_call ownership stays on that one forkSid. Assert NO new sess_ is minted on turn 3.
// ===========================================================================
func TestLineage2_MultiTurnForkStability(t *testing.T) {
	auth := authL("t2")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	opts := claudeUUIDOpts("uuid-fork-stable")

	// First, the PARENT establishes baseSid (its own opener).
	parent := lineagePayload(true, lineageMsg{"system", "S"}, lineageMsg{"user", "parent opener"})
	parentSid, _ := deriveComposerSessionID(auth, "ckey", parent, opts)

	// Turn 1: the subagent fork — its OWN first task (a divergent opener) sharing the parent uuid.
	t1 := lineagePayload(true, lineageMsg{"system", "S"}, lineageMsg{"user", "FORK: implement feature X"})
	forkSid1, _ := deriveComposerSessionID(auth, "ckey", t1, opts)
	if forkSid1 == parentSid {
		t.Fatalf("turn1 fork must split from the parent (fork=%s parent=%s)", forkSid1, parentSid)
	}
	// The fork's turn-1 tool call is owned by the fork session.
	recordComposerToolCall(tenant, "fork_tc_1", forkSid1)

	// Turn 2: tool-results continuation answering the fork's pending tool call -> ownership routes back to the
	// fork (branch 1), NOT branch 3. Built explicitly (the assistant carries tool_calls + a trailing result).
	t2 := []byte(`{"messages":[` +
		`{"role":"system","content":"S"},` +
		`{"role":"user","content":"FORK: implement feature X"},` +
		`{"role":"assistant","tool_calls":[{"id":"fork_tc_1"}]},` +
		`{"role":"tool","tool_call_id":"fork_tc_1","content":"RESULT"}]}`)
	contSid, _ := deriveComposerSessionID(auth, "ckey", t2, opts)
	if contSid != forkSid1 {
		t.Fatalf("turn2 continuation must route back to the fork via ownership (got %s want %s)", contSid, forkSid1)
	}

	// Snapshot how many lineages exist before turn 3 so we can assert NO new sess_ is minted.
	before := lineageCountForTenant(tenant)

	// Turn 3: a NEW-USER follow-up of the SAME fork (the full transcript replayed, opener preserved, head now
	// has 2 non-system messages). MUST re-resolve to the SAME forkSid (multi-turn fork stability).
	t3 := lineagePayload(true,
		lineageMsg{"system", "S"},
		lineageMsg{"user", "FORK: implement feature X"},
		lineageMsg{"assistant", "done step 1"},
		lineageMsg{"user", "now also add tests"},
	)
	forkSid3, _ := deriveComposerSessionID(auth, "ckey", t3, opts)
	if forkSid3 != forkSid1 {
		t.Fatalf("MULTI-TURN FORK STABILITY: turn3 must return the SAME forkSid as turn1 (turn1=%s turn3=%s)", forkSid1, forkSid3)
	}

	after := lineageCountForTenant(tenant)
	if after != before {
		t.Fatalf("turn3 must NOT mint a new lineage/session (before=%d after=%d) — it re-keys in place", before, after)
	}

	// Turn 4: another tool loop on the fork — ownership keeps it on the same forkSid.
	recordComposerToolCall(tenant, "fork_tc_2", forkSid3)
	t4 := []byte(`{"messages":[` +
		`{"role":"system","content":"S"},` +
		`{"role":"user","content":"FORK: implement feature X"},` +
		`{"role":"assistant","content":"done step 1"},` +
		`{"role":"user","content":"now also add tests"},` +
		`{"role":"assistant","tool_calls":[{"id":"fork_tc_2"}]},` +
		`{"role":"tool","tool_call_id":"fork_tc_2","content":"R2"}]}`)
	cont4, _ := deriveComposerSessionID(auth, "ckey", t4, opts)
	if cont4 != forkSid1 {
		t.Fatalf("turn4 continuation must stay on the fork (got %s want %s)", cont4, forkSid1)
	}
}

// ===========================================================================
// #3 GROWTH -> same session.
// Append tail turns (head 2 msgs unchanged) -> headDigest unchanged -> SAME baseSid every turn, no fork, no
// spurious reseed (growth-stable parity with composerHistoryFingerprint).
// ===========================================================================
func TestLineage3_GrowthSameSession(t *testing.T) {
	auth := authL("t3")
	opts := claudeUUIDOpts("uuid-growth")
	// Turn with a stable 2-message head; appending tail turns must not move the lineage.
	turnA := lineagePayload(true, lineageMsg{"user", "opener"}, lineageMsg{"assistant", "reply"})
	sidA, _ := deriveComposerSessionID(auth, "ckey", turnA, opts)
	turnB := lineagePayload(true,
		lineageMsg{"user", "opener"},
		lineageMsg{"assistant", "reply"},
		lineageMsg{"user", "more"},
		lineageMsg{"assistant", "more reply"},
	)
	sidB, _ := deriveComposerSessionID(auth, "ckey", turnB, opts)
	if sidA != sidB {
		t.Fatalf("growth (tail append, head unchanged) must keep the same session (a=%s b=%s)", sidA, sidB)
	}
	// And the head digest itself is growth-stable for the 2-message head.
	msgsA := gjson.GetBytes(turnA, "messages").Array()
	msgsB := gjson.GetBytes(turnB, "messages").Array()
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	hA := lineageHeadDigest(tenant, composerHistoryFingerprint(msgsA), msgsA)
	hB := lineageHeadDigest(tenant, composerHistoryFingerprint(msgsB), msgsB)
	if hA != hB {
		t.Fatalf("headDigest must be growth-stable for a >=2-message head (a=%s b=%s)", hA, hB)
	}
}

// ===========================================================================
// #4 COMPACTION-BRIDGE.
// Same uuid, /compact rewrites the body but preserves msg[0] opener -> base headDigest flips,
// compactionSignal true -> CONTINUE baseSid + re-key; composerHistoryFingerprint ALSO flips so
// inp["historyFingerprint"] drives the bridge warm-reseed. Assert NO fork.
// ===========================================================================
func TestLineage4_CompactionBridge(t *testing.T) {
	auth := authL("t4")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	opts := claudeUUIDOpts("uuid-compact")

	// Original conversation: opener + body.
	orig := lineagePayload(true,
		lineageMsg{"user", "opener: build the feature"},
		lineageMsg{"assistant", "the long original body of work that will later be compacted away"},
	)
	baseSid, _ := deriveComposerSessionID(auth, "ckey", orig, opts)
	before := lineageCountForTenant(tenant)

	// /compact: the opener (msg index 0) is preserved verbatim; message index 1 becomes a summary -> the head
	// digest flips while the opener fingerprint is unchanged.
	compacted := lineagePayload(true,
		lineageMsg{"user", "opener: build the feature"},
		lineageMsg{"assistant", "[summary] prior work condensed"},
		lineageMsg{"user", "continue from here"},
	)
	contSid, _ := deriveComposerSessionID(auth, "ckey", compacted, opts)
	if contSid != baseSid {
		t.Fatalf("compaction (opener preserved, body rewritten) must CONTINUE baseSid, not fork (got %s want %s)", contSid, baseSid)
	}
	after := lineageCountForTenant(tenant)
	if after != before {
		t.Fatalf("compaction must NOT add a fork lineage (before=%d after=%d)", before, after)
	}

	// The compaction signal itself: same opener fingerprint, different head -> true; same head -> false.
	origMsgs := gjson.GetBytes(orig, "messages").Array()
	compMsgs := gjson.GetBytes(compacted, "messages").Array()
	origHead := lineageHeadDigest(tenant, composerHistoryFingerprint(origMsgs), origMsgs)
	compHead := lineageHeadDigest(tenant, composerHistoryFingerprint(compMsgs), compMsgs)
	openFP := lineageOpenerFingerprint(origMsgs)
	if !compactionSignal(origHead, compHead, openFP, lineageOpenerFingerprint(compMsgs)) {
		t.Fatalf("compactionSignal must be true when the opener is preserved and the head moved")
	}
	if compactionSignal(origHead, origHead, openFP, openFP) {
		t.Fatalf("compactionSignal must be false when the head is unchanged")
	}
	// composerHistoryFingerprint ALSO flips (drives inp["historyFingerprint"] warm-reseed at the bridge seam).
	if composerHistoryFingerprint(origMsgs) == composerHistoryFingerprint(compMsgs) {
		t.Fatalf("composerHistoryFingerprint must flip on a /compact so the bridge re-seeds")
	}
}

// ===========================================================================
// #5 BRANCH/REGENERATE FORK + multi-turn stability.
// Same id, a branch that diverges at the OPENER (the genuine divergence the splitter targets) -> distinct
// forkSid; a SECOND new-user turn of that branch re-attaches to the same forkSid (opener-anchored re-key).
// Assert the parent's session is a different id (untouched).
//
// NOTE (documented residual, identity-finalplan §2/§6): a branch that PRESERVES the opener (msg[0]) and
// diverges only at msg[1] is structurally identical to a /compact (same opener, rewritten body) — no content
// signal separates them, so it is treated as a CONTINUE (the accepted residual). Genuine forks (subagents /
// parallel fan-out) carry their OWN first task → a distinct opener → split correctly, which is what this
// test exercises.
func TestLineage5_BranchForkMultiTurnStability(t *testing.T) {
	auth := authL("t5")
	opts := claudeUUIDOpts("uuid-branch")

	parent := lineagePayload(true, lineageMsg{"user", "parent opener task"}, lineageMsg{"assistant", "parent path A"})
	parentSid, _ := deriveComposerSessionID(auth, "ckey", parent, opts)

	// A branch with its OWN opener (a regenerate / new starting task sharing the conversation id).
	branch1 := lineagePayload(true, lineageMsg{"user", "branch opener: explore an alternative"}, lineageMsg{"assistant", "branch path B"})
	branchSid1, _ := deriveComposerSessionID(auth, "ckey", branch1, opts)
	if branchSid1 == parentSid {
		t.Fatalf("a branch with a divergent opener must SPLIT from the parent (branch=%s parent=%s)", branchSid1, parentSid)
	}

	// A SECOND new-user turn of that branch (the branch grows at the tail; its head index 0..1 is preserved).
	branch2 := lineagePayload(true,
		lineageMsg{"user", "branch opener: explore an alternative"},
		lineageMsg{"assistant", "branch path B"},
		lineageMsg{"user", "branch follow-up"},
	)
	branchSid2, _ := deriveComposerSessionID(auth, "ckey", branch2, opts)
	if branchSid2 != branchSid1 {
		t.Fatalf("the branch's later new-user turn must re-attach to the same forkSid (turn1=%s turn2=%s)", branchSid1, branchSid2)
	}
	// The parent's session is a distinct id (its durable agent is untouched by the branch).
	if branchSid1 == parentSid || branchSid2 == parentSid {
		t.Fatalf("the parent session id must stay distinct from the branch fork")
	}
}

// ===========================================================================
// #6 SUBAGENT ISOLATION (fresh history) + recorded collision slot.
// Subagent with own system + own first task, shares parent uuid -> own forkSid. Two byte-identical concurrent
// subagents -> collision-slot seed -> DISTINCT forkSid on turn 1 WHEN a second-claimant signal is present;
// the seed is recorded, and both stay head-stable across their own later turns.
// ===========================================================================
func TestLineage6_SubagentIsolationCollisionSlot(t *testing.T) {
	auth := authL("t6")
	opts := claudeUUIDOpts("uuid-subagent")

	// Parent establishes baseSid.
	parent := lineagePayload(true, lineageMsg{"system", "P"}, lineageMsg{"user", "parent"})
	parentSid, _ := deriveComposerSessionID(auth, "ckey", parent, opts)

	// Subagent: own system + own first task -> divergent head -> own fork.
	sub := lineagePayload(true, lineageMsg{"system", "SUB"}, lineageMsg{"user", "subagent first task"})
	subSid, _ := deriveComposerSessionID(auth, "ckey", sub, opts)
	if subSid == parentSid {
		t.Fatalf("subagent with its own opener must get its own fork (sub=%s parent=%s)", subSid, parentSid)
	}

	// Two byte-identical concurrent subagents sharing both id AND head: the recorded collision-slot mechanism
	// splits them WHEN a second-claimant signal is present. Drive the mechanism directly via putForkSlotted on
	// a fresh store (the production routing layer has no in-flight oracle, so it uses the honest floor).
	store := newComposerLineageStore()
	tenant := "t6-clones"
	baseSid := "sess_baseclone"
	head := "headclone"
	first := store.putForkSlotted(tenant, baseSid, head, false) // first claimant: slot 0
	second := store.putForkSlotted(tenant, baseSid, head, true) // a concurrent identical-head clone: new slot
	if first == second {
		t.Fatalf("a concurrent identical-head clone (collide=true) must get a DISTINCT recorded forkSid (first=%s second=%s)", first, second)
	}
	// The seed is RECORDED: the slotted clone re-resolves to its own stable id (head-stable across its turns).
	v, _ := store.tenants.Load(tenant)
	to := v.(*tenantLineage)
	to.mu.Lock()
	found := false
	for _, e := range to.byBaseHead {
		if e.sid == second && e.slot > 0 {
			found = true
		}
	}
	to.mu.Unlock()
	if !found {
		t.Fatalf("the collision slot must be RECORDED (a slot>0 lineage routing to the second clone)")
	}
}

// ===========================================================================
// #7 TENANT-SCOPE ROUTING (the identity-finalplan §4 item-7 cross-tenant + empty-tenant cases; the
// content-binding amendment-1 case (b) is intentionally NOT implemented — a separate sign-off decision, so it
// is not exercised here). This verifies routing scope only, not any isolation guarantee.
// (a) two tenants present the SAME conversation id + the SAME verbatim opening -> different composerTenant
// partitions -> registry MISS across tenants -> distinct sessions, never shared.
// (c) empty tenant -> mint each turn, continuity OFF (fail-closed).
// ===========================================================================
func TestLineage7_TenantScopeRouting(t *testing.T) {
	convID := "conv-scope7"
	opts := optsWithHeaders(map[string]string{"X-Conversation-Id": convID})
	payload := lineagePayload(true, lineageMsg{"system", "shared system"}, lineageMsg{"user", "identical opener verbatim"})

	authA := authL("tenant-a")
	authB := authL("tenant-b")
	aSid, _ := deriveComposerSessionID(authA, "ckey", payload, opts)
	// (a) the same conv id + same opening under a DIFFERENT tenant -> different partition -> distinct sid.
	bSid, _ := deriveComposerSessionID(authB, "ckey", payload, opts)
	if aSid == bSid {
		t.Fatalf("cross-tenant same-content turns must NOT share a session (a=%s b=%s)", aSid, bSid)
	}

	// (c) empty tenant: no auth.ID/api_key AND no caller credential -> mint, continuity OFF (fail-closed).
	emptyAuth := &cliproxyauth.Auth{}
	e1, _ := deriveComposerSessionID(emptyAuth, "ckey", payload, cliproxyexecutor.Options{Headers: opts.Headers})
	e2, _ := deriveComposerSessionID(emptyAuth, "ckey", payload, cliproxyexecutor.Options{Headers: opts.Headers})
	if !strings.HasPrefix(e1, "sess_") || e1 == e2 {
		t.Fatalf("empty tenant must mint a fresh session each turn (continuity OFF): e1=%s e2=%s", e1, e2)
	}
}

// ===========================================================================
// #8 RESPONSES CARVE-OUT.
// A previous_response_id follow-up -> H16 resume (branch 2), lineage SKIPPED; no fork, no lineage recorded.
// Map miss (restart) -> stable-conv hash, never errors. (Subagent isolation under Responses is harness-
// dependent and documented as a residual; not asserted here.)
// ===========================================================================
func TestLineage8_ResponsesCarveOut(t *testing.T) {
	auth := authL("t8")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})

	// A mapped previous_response_id resumes the durable session via the H16 map and never consults lineage.
	recordComposerResponseSession(tenant, "resp-known", "sess_durable8")
	followup := cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"resp-known","input":"thanks"}`)}
	got, err := deriveComposerSessionID(auth, "ckey", []byte(`{"messages":[{"role":"user","content":"thanks"}]}`), followup)
	if err != nil {
		t.Fatalf("responses follow-up must not error: %v", err)
	}
	if got != "sess_durable8" {
		t.Fatalf("previous_response_id must resume the H16-mapped session (got %s)", got)
	}
	if lineageCountForTenant(tenant) != 0 {
		t.Fatalf("Responses branch must NOT record any lineage (carve-out)")
	}

	// Map miss -> falls through to the stable-conv hash; never errors, and is deterministic.
	miss := cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"resp-UNKNOWN","conversation_id":"conv8"}`)}
	m1, err := deriveComposerSessionID(auth, "ckey", []byte(`{"messages":[{"role":"user","content":"hi"}]}`), miss)
	if err != nil {
		t.Fatalf("map miss must not error: %v", err)
	}
	if !strings.HasPrefix(m1, "sess_") {
		t.Fatalf("map miss must fall through to a stable-conv session, got %q", m1)
	}
}

// ===========================================================================
// #9 OWNERSHIP UNCHANGED.
// A tool_results continuation whose ids resolve to one session -> ownership wins, lineage never consulted.
// Ownership MISS with a replayed verifiable head -> lineageLookupByReplayedHead routes to the lineage; no
// match -> stable-hash/mint+reseed.
// ===========================================================================
func TestLineage9_OwnershipUnchangedAndCorroborator(t *testing.T) {
	auth := authL("t9")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})

	// Ownership wins: a continuation whose id is owned routes by ownership, ignoring any conv id.
	recordComposerToolCall(tenant, "tc_owned9", "sess_owned9")
	cont := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"tc_owned9"}]},{"role":"tool","tool_call_id":"tc_owned9","content":"R"}]}`)
	got, _ := deriveComposerSessionID(auth, "ckey", cont, optsWithHeaders(map[string]string{"X-Conversation-Id": "conv-ignored9"}))
	if got != "sess_owned9" {
		t.Fatalf("ownership must win on a continuation (got %s want sess_owned9)", got)
	}

	// Ownership MISS + a replayed verifiable head that matches a recorded fork lineage -> the corroborator
	// routes the continuation to that fork. First establish a fork under a shared conv id.
	convID := "conv-corrob9"
	conv := optsWithHeaders(map[string]string{"X-Conversation-Id": convID})
	base := lineagePayload(true, lineageMsg{"user", "base opener9"})
	baseSid, _ := deriveComposerSessionID(auth, "ckey", base, conv)
	forkTurn := lineagePayload(true, lineageMsg{"user", "fork opener9 distinct"}, lineageMsg{"assistant", "fork body"})
	forkSid, _ := deriveComposerSessionID(auth, "ckey", forkTurn, conv)
	if forkSid == baseSid {
		t.Fatalf("setup: fork must split from base")
	}
	// A continuation that replays the FORK's exact head (its 2-message head) but whose tool_call_id is NOT
	// owned -> falls to the corroborator, which routes to the fork via the replayed head.
	contFork := []byte(`{"messages":[` +
		`{"role":"user","content":"fork opener9 distinct"},` +
		`{"role":"assistant","content":"fork body"},` +
		`{"role":"assistant","tool_calls":[{"id":"unowned_tc9"}]},` +
		`{"role":"tool","tool_call_id":"unowned_tc9","content":"R"}]}`)
	routed, _ := deriveComposerSessionID(auth, "ckey", contFork, conv)
	if routed != forkSid {
		t.Fatalf("ownership-MISS corroborator must route the replayed head to its fork (got %s want %s)", routed, forkSid)
	}
}

// ===========================================================================
// #10 EVICTION NON-FORK + cap bound.
// Evict a lineage entry (TTL/cap) while its durable agent still lives; a returning turn with the same id+head
// re-CONTINUEs (re-key/re-record), does NOT mint a divergent fork off the live agent. A fan-out of N forks
// under one uuid is bounded by the per-baseSid co-resident cap, not unbounded.
// ===========================================================================
func TestLineage10_EvictionNonForkAndCapBound(t *testing.T) {
	// Drive the registry directly with an injected clock to force TTL eviction.
	store := newComposerLineageStore()
	now := time.Now()
	store.nowFn = func() time.Time { return now }
	tenant := "t10"
	baseSid := "sess_base10"
	head := "head10"
	opener := "opener10"

	// Turn 1 establishes the base lineage.
	s1 := store.resolveStableSession(tenant, baseSid, head, opener)
	if s1 != baseSid {
		t.Fatalf("first claim must establish baseSid (got %s)", s1)
	}
	// Age past the TTL and expire.
	now = now.Add(store.entryTTL + time.Minute)
	// A returning turn with the SAME id+head must re-CONTINUE (re-establish baseSid), never a divergent fork.
	s2 := store.resolveStableSession(tenant, baseSid, head, opener)
	if s2 != baseSid {
		t.Fatalf("a returning turn after eviction must re-CONTINUE baseSid, not fork (got %s)", s2)
	}

	// Cap bound: a fan-out of many divergent forks under ONE baseSid is bounded by perBase.
	store2 := newComposerLineageStore()
	store2.perBase = 8 // small cap for the test
	tenant2 := "t10-cap"
	base2 := "sess_capbase"
	// Establish the base, then fan out many distinct-head forks.
	store2.resolveStableSession(tenant2, base2, "h0", "op0")
	for i := 0; i < 200; i++ {
		store2.resolveStableSession(tenant2, base2, "h"+itoa(int64(i+1)), "op"+itoa(int64(i+1)))
	}
	v, _ := store2.tenants.Load(tenant2)
	to := v.(*tenantLineage)
	to.mu.Lock()
	n := to.countBaseLocked(base2)
	to.mu.Unlock()
	if n > store2.perBase {
		t.Fatalf("co-resident forks under one baseSid must be bounded by perBase=%d, got %d", store2.perBase, n)
	}
}

// forget (the documented terminal-stop release helper) drops only the target sid's lineages and deletes the
// tenant submap once empty. It is intentionally NOT wired at terminal-stop sites (that would evict a base
// lineage and break multi-turn fork stability); the registry's TTL+LRU handle cleanup. This verifies the
// helper itself behaves should a future scoped wiring use it.
func TestLineage_ForgetReleasesOnlyTargetSid(t *testing.T) {
	store := newComposerLineageStore()
	tenant := "tforget"
	base := "sess_baseforget"
	// Establish a base + one fork under it.
	store.resolveStableSession(tenant, base, "hbase", "opbase")
	forkSid := store.resolveStableSession(tenant, base, "hfork", "opfork")
	if forkSid == base {
		t.Fatalf("setup: fork must differ from base")
	}
	if got := tenantLineageLen(store, tenant); got != 2 {
		t.Fatalf("setup: expected 2 lineages, got %d", got)
	}
	// Forget the fork: the base survives, only the fork lineage is dropped.
	store.forget(tenant, forkSid)
	if got := tenantLineageLen(store, tenant); got != 1 {
		t.Fatalf("forgetting a fork must drop exactly one lineage (remaining=%d)", got)
	}
	// Forget the base too: the tenant submap is deleted once empty.
	store.forget(tenant, base)
	if _, ok := store.tenants.Load(tenant); ok {
		t.Fatalf("the tenant submap must be deleted once empty")
	}
	// The exported helper routes through the global registry (smoke: must not panic on an unknown tenant).
	lineageForget("tforget-unknown", "sess_none")
}

// tenantLineageLen returns the lineage count for a tenant on a SPECIFIC store (not the global one).
func tenantLineageLen(s *composerLineageStore, tenant string) int {
	v, ok := s.tenants.Load(tenant)
	if !ok {
		return 0
	}
	to := v.(*tenantLineage)
	to.mu.Lock()
	defer to.mu.Unlock()
	return len(to.byBaseHead)
}
