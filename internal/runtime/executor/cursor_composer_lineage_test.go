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
	composerOwnership.record(tenant, "fork_tc_1", forkSid1)

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
	composerOwnership.record(tenant, "fork_tc_2", forkSid3)
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

// #10 (review): concurrent identical-head clones must take the SMALLEST UNUSED slot DETERMINISTICALLY (1,2,3…),
// never a random 16-bit slot (which could birthday-collide at the per-base cap and overwrite a live sibling).
// Deterministic slots also make concurrency forks reproducible across restart/replica.
func TestLineage_DeterministicMonotonicSlot(t *testing.T) {
	tenant := "t10"
	baseSid := "sess_base10"
	head := "head10"
	store := newComposerLineageStore()
	s0 := store.putForkSlotted(tenant, baseSid, head, false) // first claimant: slot 0 (id omits the slot)
	s1 := store.putForkSlotted(tenant, baseSid, head, true)  // concurrent clone -> smallest unused: slot 1
	s2 := store.putForkSlotted(tenant, baseSid, head, true)  // next concurrent clone -> slot 2
	if s1 != forkSessionID(baseSid, head, 1) {
		t.Fatalf("first concurrent clone must take slot 1 deterministically (got %s want %s)", s1, forkSessionID(baseSid, head, 1))
	}
	if s2 != forkSessionID(baseSid, head, 2) {
		t.Fatalf("second concurrent clone must take slot 2 deterministically (got %s want %s)", s2, forkSessionID(baseSid, head, 2))
	}
	if s0 == s1 || s1 == s2 || s0 == s2 {
		t.Fatalf("all three slots must be distinct (s0=%s s1=%s s2=%s)", s0, s1, s2)
	}
	// Reproducible: a FRESH store allocates the SAME slot ids in the same order (forkSessionID is a pure digest).
	store2 := newComposerLineageStore()
	if store2.putForkSlotted(tenant, baseSid, head, false) != s0 || store2.putForkSlotted(tenant, baseSid, head, true) != s1 {
		t.Fatalf("slot allocation must be deterministic/reproducible across stores")
	}
}

// #11 (review): when a base AND a concurrency-fork share the same opener and a turn's head has moved so it
// matches BOTH via the compaction signal, the opener bridge must NOT prefer the base (the old tie-break) — that
// would re-key the parent onto the fork's head (corruption) or pull a fork back to base. >1 match is ambiguous,
// so the resolver isolates the turn instead of collapsing onto the base.
func TestLineage_OpenerBridgeAmbiguousDoesNotCollapseToBase(t *testing.T) {
	store := newComposerLineageStore()
	tenant := "t11"
	baseSid := "sess_base11"
	openerFP := "openerX"
	to := store.tenantLineageFor(tenant)
	to.mu.Lock()
	baseKey := lineageKey(baseSid, "H1")
	to.byBaseHead[baseKey] = &lineageEntry{sid: baseSid, headDigest: "H1", openerFP: openerFP, lastUsed: store.nowFn()}
	to.order = append(to.order, baseKey)
	forkSid := forkSessionID(baseSid, "H1", 1)
	forkKey := baseKey + "\x00slot:1"
	to.byBaseHead[forkKey] = &lineageEntry{sid: forkSid, headDigest: "H2", slot: 1, openerFP: openerFP, lastUsed: store.nowFn()}
	to.order = append(to.order, forkKey)
	to.mu.Unlock()
	// A turn carrying the SAME opener but a NEW head H3 -> compactionSignal true for BOTH base(H1) and fork(H2).
	got := store.resolveStableSession(tenant, baseSid, "H3", openerFP)
	if got == baseSid {
		t.Fatalf("an ambiguous opener match (base + fork) must NOT collapse/re-key onto the base/parent (got base %s)", got)
	}
	if !strings.HasPrefix(got, "sess_") {
		t.Fatalf("ambiguous resolution must still yield a sess_ id, got %q", got)
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
	composerOwnership.record(tenant, "tc_owned9", "sess_owned9")
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

// #19 (review): the per-base cap must SKIP a LIVE fork (held logical-run lease) when evicting, so a concurrent
// fork's continuity is never dropped to make room. A heavy fan-out under one base leaves the live fork intact.
func TestLineage19_LiveForkNotEvictedByCap(t *testing.T) {
	store := newComposerLineageStore()
	store.perBase = 4 // tiny cap so eviction fires constantly under the fan-out below
	tenant := "t19-live"
	base := "sess_base19"
	store.resolveStableSession(tenant, base, "h0", "op0")             // establish the base
	live := store.resolveStableSession(tenant, base, "hLIVE", "opLV") // a distinct-head fork
	if live == base {
		t.Fatalf("setup: hLIVE must fork from the base (got base)")
	}
	owner, ok := composerInflight.claim(tenant, live) // hold a logical-run lease -> this fork is LIVE
	if !ok {
		t.Fatalf("setup: could not claim the live fork's lease")
	}
	defer composerInflight.release(tenant, live, owner)
	// Heavy fan-out of distinct-head forks forces the per-base cap to evict repeatedly.
	for i := 0; i < 60; i++ {
		store.resolveStableSession(tenant, base, "h"+itoa(int64(i)), "op"+itoa(int64(i)))
	}
	// The LIVE fork's lineage must survive — never evicted, even though the cap was exceeded many times over.
	v, _ := store.tenants.Load(tenant)
	to := v.(*tenantLineage)
	to.mu.Lock()
	found := false
	for _, e := range to.byBaseHead {
		if e.sid == live {
			found = true
			break
		}
	}
	to.mu.Unlock()
	if !found {
		t.Fatalf("#19: a LIVE fork's lineage must NOT be evicted by the per-base cap")
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

// ===========================================================================
// CONCURRENCY-FORK (ISOLATION invariant). The proven collapse: N subagents fan out under one parent
// metadata.user_id with a BYTE-IDENTICAL opener, so content cannot split them and they collapse onto one
// serial bridge session (sess_e6067ba9 in production). The live resolver forks each turn that arrives while the
// session is already running a logical run onto a DISTINCT slot session, so the siblings run in PARALLEL.
// ===========================================================================

func TestConcurrencyForkDistinctSessions(t *testing.T) {
	auth := authL("tcf")
	opts := claudeUUIDOpts("uuid-concurrency")
	tenant := composerTenant(auth, opts)
	// Byte-identical subagent turns sharing the parent uuid — the vector that collapses under content routing.
	turn := lineagePayload(true, lineageMsg{"system", "S"}, lineageMsg{"user", "identical subagent task"})

	a, ownerA, err := deriveComposerSessionIDLive(auth, "ckey", turn, opts)
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	// B arrives while A's logical run is still in flight (A not released) -> must FORK to a distinct session.
	b, ownerB, err := deriveComposerSessionIDLive(auth, "ckey", turn, opts)
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if a == b {
		t.Fatalf("concurrent identical subagents must get DISTINCT sessions (a=%s b=%s)", a, b)
	}
	if !strings.HasPrefix(a, "sess_") || !strings.HasPrefix(b, "sess_") {
		t.Fatalf("both must be sess_ ids (a=%s b=%s)", a, b)
	}
	if ownerA == 0 || ownerB == 0 || ownerA == ownerB {
		t.Fatalf("each forked claim must mint a distinct non-zero owner (a=%d b=%d)", ownerA, ownerB)
	}
	// A third concurrent sibling -> yet another distinct session.
	c, ownerC, _ := deriveComposerSessionIDLive(auth, "ckey", turn, opts)
	if c == a || c == b {
		t.Fatalf("a third concurrent sibling must also be distinct (a=%s b=%s c=%s)", a, b, c)
	}

	// Release A (its run reached a terminal end). A NEW same-content turn must REUSE A's freed session, not
	// fork — concurrency forking splits only turns that are ACTUALLY concurrent.
	composerInflight.release(tenant, a, ownerA)
	d, ownerD, _ := deriveComposerSessionIDLive(auth, "ckey", turn, opts)
	if d != a {
		t.Fatalf("after release, a same-content turn must REUSE the freed session, not fork (a=%s d=%s)", a, d)
	}

	// Clean up leases so the global registry does not leak into other tests. D re-claimed A's session, so its
	// lease carries ownerD (the fresh mint), not the released ownerA.
	composerInflight.release(tenant, a, ownerD)
	composerInflight.release(tenant, b, ownerB)
	composerInflight.release(tenant, c, ownerC)
}

func TestConcurrencyLeaseLifecycle(t *testing.T) {
	store := newComposerInflightStore()
	now := time.Now()
	store.nowFn = func() time.Time { return now }

	ownerA, ok := store.claim("t", "sA")
	if !ok || ownerA == 0 {
		t.Fatalf("first claim must succeed (free) and mint a non-zero owner (owner=%d ok=%v)", ownerA, ok)
	}
	if _, ok := store.claim("t", "sA"); ok {
		t.Fatalf("a second CONCURRENT claim must FAIL (held) — the serial-session latch")
	}
	// touch keeps the run alive across a tool pause; a concurrent claim must still fail.
	store.touch("t", "sA", ownerA)
	if _, ok := store.claim("t", "sA"); ok {
		t.Fatalf("claim must still fail after touch (lease still held)")
	}
	// P0-1: a NON-owner release (a stale/superseded run's late cleanup) must NOT free a lease it no longer owns.
	store.release("t", "sA", ownerA+999)
	if _, ok := store.claim("t", "sA"); ok {
		t.Fatalf("a non-owner release must be a no-op (the lease stays held)")
	}
	// release by the owner frees it.
	store.release("t", "sA", ownerA)
	ownerA2, ok := store.claim("t", "sA")
	if !ok || ownerA2 == ownerA {
		t.Fatalf("claim after release must succeed and mint a FRESH owner (owner2=%d owner1=%d ok=%v)", ownerA2, ownerA, ok)
	}
	// A stale owner from BEFORE the release can no longer touch the new lease.
	store.release("t", "sA", ownerA) // no-op: ownerA is stale
	if _, ok := store.claim("t", "sA"); ok {
		t.Fatalf("the new lease (ownerA2) must survive a stale ownerA release")
	}
	// Staleness self-heals a LEAKED hold (a turn that aborted without releasing) past the TTL.
	now = now.Add(store.ttl + time.Minute)
	ownerA3, ok := store.claim("t", "sA")
	if !ok || ownerA3 == ownerA2 {
		t.Fatalf("a stale (>TTL) hold must be reclaimable with a fresh owner (owner3=%d owner2=%d ok=%v)", ownerA3, ownerA2, ok)
	}
	// A different sid is independently claimable.
	if _, ok := store.claim("t", "sB"); !ok {
		t.Fatalf("a different sid must be independently claimable while sA is held")
	}
}

// ===========================================================================
// CONTINUITY (review Comments 1 & 2). A fork's continuation must re-attach to the fork (by ownership, or by the
// lineage opener bridge when ownership is lost), and on FULL state loss must reseed to a fork-namespaced id —
// NEVER collapse onto the parent/base session.
// ===========================================================================

// Comment 1: a content-fork emits a tool call on its FIRST (1-message-head) turn, then loses ownership; the
// full-replay continuation (grown 2-message head) must re-attach to the fork via the OPENER BRIDGE, not baseSid.
func TestContinuityForkReattachByOpenerAfterOwnershipLoss(t *testing.T) {
	auth := authL("tcr1")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	opts := claudeUUIDOpts("uuid-reattach")
	parent := lineagePayload(true, lineageMsg{"user", "parent opener task"})
	parentSid, _ := deriveComposerSessionID(auth, "ckey", parent, opts)
	// Fork turn 1: a DISTINCT opener (1-message head) -> a content-fork recorded with its openerFP.
	forkT1 := lineagePayload(true, lineageMsg{"user", "FORK distinct opener nine"})
	forkSid, _ := deriveComposerSessionID(auth, "ckey", forkT1, opts)
	if forkSid == parentSid {
		t.Fatalf("setup: fork must split from parent (fork=%s parent=%s)", forkSid, parentSid)
	}
	composerOwnership.record(tenant, "tc_reattach", forkSid)
	// LOSE ownership (TTL eviction / restart) but the lineage survives.
	composerOwnership.forgetSession(tenant, forkSid)
	// Full-replay continuation of the fork: opener + the assistant tool_call + the now-UNOWNED result. Its head
	// has GROWN past the recorded 1-message opener, so it must re-attach via the opener bridge (b), not (a).
	cont := []byte(`{"messages":[` +
		`{"role":"user","content":"FORK distinct opener nine"},` +
		`{"role":"assistant","tool_calls":[{"id":"tc_reattach"}]},` +
		`{"role":"tool","tool_call_id":"tc_reattach","content":"R"}]}`)
	routed, err := deriveComposerSessionID(auth, "ckey", cont, opts)
	if err != nil {
		t.Fatalf("continuation must route: %v", err)
	}
	if routed != forkSid {
		t.Fatalf("ownership-lost continuation must re-attach to its fork via the opener bridge (got %s want %s, base %s)", routed, forkSid, parentSid)
	}
}

// Comment 2: after FULL state loss (lineage registry + ownership both cleared), a returning fork continuation
// must RESEED to a fork-namespaced id (≠ baseSid) — never collapse onto the parent/base.
func TestContinuityReseedNotCollapseAfterStateLoss(t *testing.T) {
	auth := authL("tcr2")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	opts := claudeUUIDOpts("uuid-stateloss")
	parent := lineagePayload(true, lineageMsg{"user", "parent base opener"})
	parentSid, _ := deriveComposerSessionID(auth, "ckey", parent, opts) // == baseSid (first claimant)
	forkT1 := lineagePayload(true, lineageMsg{"user", "subagent fork opener xyz"})
	forkSid, _ := deriveComposerSessionID(auth, "ckey", forkT1, opts)
	if forkSid == parentSid {
		t.Fatalf("setup: fork must split")
	}
	composerOwnership.record(tenant, "tc_lost", forkSid)
	// FULL state loss: drop the whole lineage registry for the tenant AND the ownership.
	composerLineage.tenants.Delete(tenant)
	composerOwnership.forgetSession(tenant, forkSid)
	// The returning fork's full-replay continuation: ownership gone, lineage gone -> MUST reseed, NOT baseSid.
	cont := []byte(`{"messages":[` +
		`{"role":"user","content":"subagent fork opener xyz"},` +
		`{"role":"assistant","tool_calls":[{"id":"tc_lost"}]},` +
		`{"role":"tool","tool_call_id":"tc_lost","content":"R"}]}`)
	routed, _ := deriveComposerSessionID(auth, "ckey", cont, opts)
	if routed == parentSid {
		t.Fatalf("a returning fork after state loss must NOT collapse onto the parent/base (got base %s)", parentSid)
	}
	if !strings.HasPrefix(routed, "sess_") {
		t.Fatalf("reseed must be a sess_ id, got %q", routed)
	}
}

// ADD-116: a LINEAR lost continuation (base lineage intact, opener positively matches the base, NO live fork
// sibling) RESUMES the alive durable base instead of forking — recovering its context in place (the bridge's
// local.force clears the dead run). This is the run-death win; contrast TestContinuityReseedNotCollapseAfterStateLoss
// (full state loss -> base opener unknown -> still forks).
func TestLostContinuation_LinearResumesBase(t *testing.T) {
	auth := authL("tlcrb")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	opts := claudeUUIDOpts("uuid-linear-resume")
	base := lineagePayload(true, lineageMsg{"user", "linear base opener abc"})
	baseSid, _ := deriveComposerSessionID(auth, "ckey", base, opts) // records the base lineage (opener + head)
	// Run dies mid-tool-call: ownership of the emitted tool call is forgotten, but the base lineage SURVIVES
	// (linear conversation, no full state loss) and no fork sibling is live.
	composerOwnership.record(tenant, "tc_linear", baseSid)
	composerOwnership.forgetSession(tenant, baseSid)
	cont := []byte(`{"messages":[` +
		`{"role":"user","content":"linear base opener abc"},` +
		`{"role":"assistant","tool_calls":[{"id":"tc_linear"}]},` +
		`{"role":"tool","tool_call_id":"tc_linear","content":"R"}]}`)
	routed, _ := deriveComposerSessionID(auth, "ckey", cont, opts)
	if routed != baseSid {
		t.Fatalf("a linear lost continuation (base intact, opener matches, no live sibling) must RESUME the base, got %q want %q", routed, baseSid)
	}
}

// ADD-116: a lost continuation with a LIVE fork sibling under the same base must NEVER collapse onto the base
// (that would mis-route into a parallel sub-agent's run) — it still forks. The bridge is the backstop, but the
// predicate refuses up front.
func TestLostContinuation_LiveForkSiblingStillForks(t *testing.T) {
	auth := authL("tlcfs")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	opts := claudeUUIDOpts("uuid-livesib")
	base := lineagePayload(true, lineageMsg{"user", "concur base opener"})
	baseSid, _ := deriveComposerSessionID(auth, "ckey", base, opts)
	// A fork sibling exists under this base AND holds a live logical-run lease.
	forkSid := composerLineage.reseedLostFork(tenant, baseSid, "siblinghead0001", "siblingopenerfp")
	owner, _ := composerInflight.claim(tenant, forkSid)
	defer composerInflight.release(tenant, forkSid, owner)
	composerOwnership.forgetSession(tenant, baseSid)
	cont := []byte(`{"messages":[` +
		`{"role":"user","content":"concur base opener"},` +
		`{"role":"assistant","tool_calls":[{"id":"tc_concur"}]},` +
		`{"role":"tool","tool_call_id":"tc_concur","content":"R"}]}`)
	routed, _ := deriveComposerSessionID(auth, "ckey", cont, opts)
	if routed == baseSid {
		t.Fatalf("a lost continuation with a LIVE fork sibling must NOT collapse onto the base (got base %s)", baseSid)
	}
}

// ADD-116: a lost continuation whose opener does NOT match the recorded base opener (a divergent content-fork)
// must still fork, even with no live sibling — the positive-match requirement prevents collapsing a different
// conversation onto the base.
func TestLostContinuation_DistinctOpenerStillForks(t *testing.T) {
	auth := authL("tlcdo")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	opts := claudeUUIDOpts("uuid-distinct")
	base := lineagePayload(true, lineageMsg{"user", "the real base opener"})
	baseSid, _ := deriveComposerSessionID(auth, "ckey", base, opts)
	composerOwnership.forgetSession(tenant, baseSid)
	cont := []byte(`{"messages":[` +
		`{"role":"user","content":"a totally different fork opener"},` +
		`{"role":"assistant","tool_calls":[{"id":"tc_d"}]},` +
		`{"role":"tool","tool_call_id":"tc_d","content":"R"}]}`)
	routed, _ := deriveComposerSessionID(auth, "ckey", cont, opts)
	if routed == baseSid {
		t.Fatalf("a distinct-opener continuation must NOT collapse onto the base (got base %s)", baseSid)
	}
}

// Comment 2 (stable secret): a configured CURSOR_COMPOSER_LINEAGE_SECRET yields a deterministic key (so fork
// ids are stable across restart/replica); an unset secret is per-process random.
func TestLineageSecretStableFromEnv(t *testing.T) {
	cfg := func(string) string { return "deadbeefcafe0123deadbeefcafe0123deadbeefcafe0123deadbeefcafe0123" }
	a := loadComposerLineageSecret(cfg)
	b := loadComposerLineageSecret(cfg)
	if len(a) != 32 || string(a) != string(b) {
		t.Fatalf("a configured secret must be deterministic 32 bytes across loads")
	}
	raw := func(string) string { return "some-raw-non-hex-secret-value" }
	if string(loadComposerLineageSecret(raw)) != string(loadComposerLineageSecret(raw)) {
		t.Fatalf("a configured raw (non-hex) secret must also be deterministic")
	}
	r1 := loadComposerLineageSecret(func(string) string { return "" })
	r2 := loadComposerLineageSecret(func(string) string { return "" })
	if string(r1) == string(r2) {
		t.Fatalf("an UNSET secret must be per-process random (two loads must differ)")
	}
}

func TestConcurrencyForkContinuationDoesNotFork(t *testing.T) {
	auth := authL("tcfc")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	opts := claudeUUIDOpts("uuid-cont-nofork")
	// A fork session that emitted a tool call (ownership recorded), with its logical run in flight (lease held).
	forkSid := "sess_forkcont0001"
	composerOwnership.record(tenant, "tc_cont1", forkSid)
	owner, _ := composerInflight.claim(tenant, forkSid)

	// A continuation answering that tool call must re-attach to the emitting fork by OWNERSHIP — NOT concurrency-
	// fork — even though the session is "busy" (its own paused run is exactly what this continuation feeds).
	cont := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"tc_cont1"}]},{"role":"tool","tool_call_id":"tc_cont1","content":"R"}]}`)
	got, _, err := deriveComposerSessionIDLive(auth, "ckey", cont, opts)
	if err != nil {
		t.Fatalf("continuation must route, not error: %v", err)
	}
	if got != forkSid {
		t.Fatalf("a continuation must re-attach to its emitting fork by ownership, never concurrency-fork (got %s want %s)", got, forkSid)
	}
	composerInflight.release(tenant, forkSid, owner)
}

// P0-5: a previous_response_id resume must take part in the lease, not bypass it. Two CONCURRENT resumes of the
// SAME response id must not both land on the pinned durable session (the bridge rejects a second concurrent turn
// on a serial session -> 500). The first claims the pinned session; the second forks. A sequential resume (after
// release) re-claims the pinned session so its durable context is preserved.
func TestConcurrencyForkPreviousResponseIDResume(t *testing.T) {
	auth := authL("tpr")
	tenant := composerTenant(auth, cliproxyexecutor.Options{})
	recordComposerResponseSession(tenant, "resp-pin", "sess_durablepin0001")
	resume := cliproxyexecutor.Options{OriginalRequest: []byte(`{"previous_response_id":"resp-pin","input":"go on"}`)}
	body := []byte(`{"messages":[{"role":"user","content":"go on"}]}`)

	a, ownerA, err := deriveComposerSessionIDLive(auth, "ckey", body, resume)
	if err != nil {
		t.Fatalf("resume A must route: %v", err)
	}
	if a != "sess_durablepin0001" {
		t.Fatalf("the first resume must claim the PINNED durable session (got %s)", a)
	}
	if ownerA == 0 {
		t.Fatalf("the first resume must now CLAIM the lease (owner must be non-zero) — it no longer bypasses it")
	}
	// A second concurrent resume of the SAME pid -> the pinned session is held -> must FORK to a distinct session.
	b, ownerB, err := deriveComposerSessionIDLive(auth, "ckey", body, resume)
	if err != nil {
		t.Fatalf("resume B must route: %v", err)
	}
	if b == a {
		t.Fatalf("a concurrent same-pid resume must FORK, not collide on the pinned serial session (a=%s b=%s)", a, b)
	}
	// Release A; a sequential resume must RE-CLAIM the pinned session (durable context preserved), not fork.
	composerInflight.release(tenant, a, ownerA)
	c, ownerC, _ := deriveComposerSessionIDLive(auth, "ckey", body, resume)
	if c != a {
		t.Fatalf("a sequential resume after release must re-claim the pinned session, not fork (got %s want %s)", c, a)
	}
	composerInflight.release(tenant, a, ownerC)
	composerInflight.release(tenant, b, ownerB)
}

// ---------------------------------------------------------------------------
// CLIENT-AGNOSTIC content keying — a client that sends NO explicit conversation id (opendesign, raw
// OpenAI/SDK callers, simple UIs) must still get ONE durable session across all of a conversation's turns,
// keyed off the turn-stable conversation opener (composerContentConvKey) instead of minting fresh every turn.
// This is the built-in equivalent of the Anthropic path's metadata.user_id for non-Claude-Code clients.
// ---------------------------------------------------------------------------

// A no-conv-id conversation (stateless replaying client) must keep ONE durable session across a new-user
// follow-up: the full transcript is replayed every turn, so the opener is byte-identical and resolves to the
// same session — NOT a fresh mint (the regression that made opendesign "not continue turns").
func TestContentKey_NoConvIDDurableAcrossTurns(t *testing.T) {
	auth := authL("tck-dur")
	noID := cliproxyexecutor.Options{} // no header, no metadata.user_id, no conversation_id
	t1 := lineagePayload(true, lineageMsg{"user", "implement the OAuth device flow"})
	s1, err := deriveComposerSessionID(auth, "ckey", t1, noID)
	if err != nil {
		t.Fatalf("turn 1 derive: %v", err)
	}
	if !strings.HasPrefix(s1, "sess_") {
		t.Fatalf("content-keyed session must be a real sess_ id, got %q", s1)
	}
	// New-user follow-up: same opener replayed at the head + new tail. Must resolve to the SAME session.
	t2 := lineagePayload(true,
		lineageMsg{"user", "implement the OAuth device flow"},
		lineageMsg{"assistant", "done — added the device_code grant"},
		lineageMsg{"user", "now add token refresh"})
	s2, err := deriveComposerSessionID(auth, "ckey", t2, noID)
	if err != nil {
		t.Fatalf("turn 2 derive: %v", err)
	}
	if s1 != s2 {
		t.Fatalf("a no-conv-id conversation must keep ONE durable session across turns (opener-keyed); got s1=%q s2=%q", s1, s2)
	}
}

// Two no-conv-id conversations with DISTINCT openers must NOT collapse onto one session (distinct tasks key
// distinctly — the opener IS the task, unlike the ADD-78 shared prompt_cache_key).
func TestContentKey_DistinctOpenersDistinctSessions(t *testing.T) {
	auth := authL("tck-dist")
	noID := cliproxyexecutor.Options{}
	a := lineagePayload(true, lineageMsg{"user", "refactor the billing module"})
	b := lineagePayload(true, lineageMsg{"user", "write release notes for v2"})
	sa, _ := deriveComposerSessionID(auth, "ckey", a, noID)
	sb, _ := deriveComposerSessionID(auth, "ckey", b, noID)
	if sa == sb {
		t.Fatalf("distinct openers must NOT collapse onto one session (sa=%q sb=%q)", sa, sb)
	}
}

// A CONCURRENT same-opener turn (no conv id) must FORK onto a distinct sibling via the live executor's
// isolation invariant rather than collide on the busy content-keyed session.
func TestContentKey_LiveForkOnConcurrentSameOpener(t *testing.T) {
	auth := authL("tck-fork")
	noID := cliproxyexecutor.Options{}
	tenant := composerTenant(auth, noID)
	p := lineagePayload(true, lineageMsg{"user", "same opener concurrent run"})
	s1, owner1, err := deriveComposerSessionIDLive(auth, "ckey", p, noID)
	if err != nil {
		t.Fatalf("live derive 1: %v", err)
	}
	defer composerInflight.release(tenant, s1, owner1)
	s2, owner2, err := deriveComposerSessionIDLive(auth, "ckey", p, noID)
	if err != nil {
		t.Fatalf("live derive 2: %v", err)
	}
	defer composerInflight.release(tenant, s2, owner2)
	if s2 == s1 {
		t.Fatalf("a concurrent same-opener turn must fork onto a distinct sibling, got the same session %q", s1)
	}
}
