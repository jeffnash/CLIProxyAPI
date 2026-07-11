package executor

import (
	"encoding/json"
	"fmt"
	"strconv"
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

func TestContinuationTerminalReleasesOnlyItsOpeningLease(t *testing.T) {
	tenant := "continuation-lease-tenant"
	sid := "sess_continuationlease"
	owner, ok := composerInflight.claim(tenant, sid)
	if !ok || owner == 0 {
		t.Fatalf("setup: fresh turn must claim a lease (owner=%d ok=%v)", owner, ok)
	}
	t.Cleanup(func() { composerInflight.release(tenant, sid, owner) })

	// The first HTTP turn pauses for a client tool and keeps the lease held.
	composerApplyLeaseStop(tenant, sid, "tool_use", owner)
	if _, ok := composerInflight.claim(tenant, sid); ok {
		t.Fatal("tool_use must retain the opening fresh-turn lease")
	}

	terminal := gjson.Parse(fmt.Sprintf(
		`{"type":"turn_end","stop_reason":"end_turn","clientLease":{"sessionId":%q,"token":%q,"terminal":true}}`,
		sid, strconv.FormatUint(owner, 10),
	))
	leaseSID, leaseStop, leaseOwner := composerContinuationLeaseStop(terminal)
	if leaseSID != sid || leaseStop != "end_turn" || leaseOwner != owner {
		t.Fatalf("continuation lease echo did not round-trip: sid=%q stop=%q owner=%d", leaseSID, leaseStop, leaseOwner)
	}
	composerApplyLeaseStop(tenant, leaseSID, leaseStop, leaseOwner)

	newOwner, ok := composerInflight.claim(tenant, sid)
	if !ok || newOwner == 0 || newOwner == owner {
		t.Fatalf("terminal continuation must free the paused session for immediate reuse (owner=%d old=%d ok=%v)", newOwner, owner, ok)
	}
	t.Cleanup(func() { composerInflight.release(tenant, sid, newOwner) })

	// A delayed replay from the old round carries the old owner token and must
	// never evict the newer logical run.
	composerApplyLeaseStop(tenant, leaseSID, leaseStop, leaseOwner)
	if _, ok := composerInflight.claim(tenant, sid); ok {
		t.Fatal("a late continuation terminal evicted a newer lease")
	}
}

func TestContinuationLeaseMetadataFailsSafe(t *testing.T) {
	cases := []string{
		`{"type":"turn_end","stop_reason":"end_turn"}`,
		`{"type":"turn_end","stop_reason":"end_turn","clientLease":{"sessionId":"bad","token":"1","terminal":true}}`,
		`{"type":"turn_end","stop_reason":"end_turn","clientLease":{"sessionId":"sess_valid123","token":"18446744073709551616","terminal":true}}`,
		`{"type":"turn_end","stop_reason":"end_turn","clientLease":{"sessionId":"sess_valid123","token":"1","terminal":"true"}}`,
	}
	for _, raw := range cases {
		sid, stop, owner := composerContinuationLeaseStop(gjson.Parse(raw))
		if sid != "" || stop != "" || owner != 0 {
			t.Fatalf("malformed continuation lease must be ignored, got sid=%q stop=%q owner=%d for %s", sid, stop, owner, raw)
		}
	}

	// A typed retry/ack may carry the exact owner but explicitly state that the
	// logical SDK run is not terminal. It can refresh a tool pause, but it must
	// not release the lease as an end_turn/error.
	sid, stop, owner := composerContinuationLeaseStop(gjson.Parse(
		`{"type":"turn_end","stop_reason":"error","clientLease":{"sessionId":"sess_valid123","token":"9","terminal":false}}`,
	))
	if sid != "sess_valid123" || stop != "" || owner != 9 {
		t.Fatalf("nonterminal continuation receipt must retain, not release, the lease: sid=%q stop=%q owner=%d", sid, stop, owner)
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
