package turnprovenance

import (
	"bytes"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestTokenSecretFallsBackToPurposeSeparatedLineageSecret(t *testing.T) {
	t.Setenv("CLIPROXY_TURN_PROVENANCE_SECRET", "")
	t.Setenv("CURSOR_COMPOSER_LINEAGE_SECRET", "stable-lineage-secret")
	got, err := TokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || bytes.Equal(got, []byte("stable-lineage-secret")) {
		t.Fatalf("TokenSecret() must return a non-empty purpose-separated key")
	}
	again, err := TokenSecret()
	if err != nil || !bytes.Equal(got, again) {
		t.Fatalf("derived key must be stable: equal=%t err=%v", bytes.Equal(got, again), err)
	}
}

func TestResolveExplicitStandingAndCurrent(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"system","content":"rules"},
		{"role":"user","content":"standing block that is large enough","metadata":{"cliproxy":{"provenance":"standing"}}},
		{"role":"user","content":"do the new thing","metadata":{"cliproxy":{"provenance":"current"}}}
	]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	d := (&Resolver{}).Resolve(Input{Segments: segs, TenantID: "t1", ConversationID: "c1"})
	if d.Kind != DecisionResolvedExplicit {
		t.Fatalf("kind=%s reason=%s", d.Kind, d.ReasonCode)
	}
	if len(d.CurrentSegments) != 1 || len(d.StandingSegments) < 2 {
		t.Fatalf("current=%v standing=%v", d.CurrentSegments, d.StandingSegments)
	}
}

func TestExtractSegmentsRecognizesOpenAITextPartVariants(t *testing.T) {
	raw := []byte(`{"messages":[{"role":"user","content":[{"type":"input_text","text":"first"},{"type":"output_text","text":"second"}]}]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	if len(segs) != 1 || segs[0].ByteLength != len("first\nsecond") {
		t.Fatalf("OpenAI text parts must contribute to provenance identity, got %+v", segs)
	}
}

func TestResolveCompleteFragments(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":"part-a","metadata":{"cliproxy":{"provenance":"fragment","fragment_group_id":"g1","fragment_index":0,"fragment_count":2}}},
		{"role":"user","content":"part-b","metadata":{"cliproxy":{"provenance":"fragment","fragment_group_id":"g1","fragment_index":1,"fragment_count":2}}}
	]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	d := (&Resolver{}).Resolve(Input{Segments: segs})
	if d.Kind != DecisionResolvedFragments {
		t.Fatalf("kind=%s reason=%s", d.Kind, d.ReasonCode)
	}
	if len(d.MergeGroups) != 1 || len(d.MergeGroups[0]) != 2 {
		t.Fatalf("merge groups=%v", d.MergeGroups)
	}
}

func TestResolveExplicitCurrentWithUnspecifiedNeighborClarifies(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":"declared current only","metadata":{"cliproxy":{"provenance":"current"}}},
		{"role":"user","content":"unspecified adjacent competitor"}
	]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	d := (&Resolver{}).Resolve(Input{Segments: segs, TenantID: "t1", ConversationID: "c1"})
	if d.Kind == DecisionResolvedExplicit {
		t.Fatalf("partial explicit declaration must not resolve: %+v", d)
	}
}

func TestResolveIncompleteFragmentsClarify(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":"part-a","metadata":{"cliproxy":{"fragment_group_id":"g1","fragment_index":0,"fragment_count":2}}}
	]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	d := (&Resolver{}).Resolve(Input{Segments: segs})
	if d.Kind != DecisionClarificationRequired || d.ReasonCode != ReasonIncompleteFragments {
		t.Fatalf("kind=%s reason=%s", d.Kind, d.ReasonCode)
	}
}

func TestAmbiguousAdjacentUsersColdStartClarify(t *testing.T) {
	raw := []byte(`{"messages":[
		{"role":"user","content":"injected standing context block that is intentionally long"},
		{"role":"user","content":"short ask"}
	]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	d := (&Resolver{}).Resolve(Input{
		Segments:       segs,
		TenantID:       "t1",
		ConversationID: "c1",
		Recurrence:     RecurrenceEvidence{Enabled: true, MinOccurrences: 3, MinByteLength: 32},
	})
	if !d.ClarificationRequired() {
		t.Fatalf("expected clarification, got %+v", d)
	}
	if d.ReasonCode != ReasonColdStart {
		t.Fatalf("reason=%s", d.ReasonCode)
	}
}

func TestRecurrencePromotesStanding(t *testing.T) {
	secret := []byte("test-secret")
	store := NewMemoryStore(secret, 3, 32)
	standing := contentDigest(RoleUser, "injected standing context block that is intentionally long", false)
	for i := 0; i < 3; i++ {
		store.ObserveCompleted("t1", "c1", standing, "neighbor-"+string(rune('a'+i)), 1)
	}
	raw := []byte(`{"messages":[
		{"role":"user","content":"injected standing context block that is intentionally long"},
		{"role":"user","content":"fresh user request"}
	]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	r := &Resolver{Store: store, Secret: secret}
	d := r.Resolve(Input{Segments: segs, TenantID: "t1", ConversationID: "c1"})
	if d.Kind != DecisionResolvedRecurrence {
		t.Fatalf("kind=%s reason=%s evidence=%v", d.Kind, d.ReasonCode, d.Evidence)
	}
	if len(d.CurrentSegments) != 1 || len(d.StandingSegments) != 1 {
		t.Fatalf("current=%v standing=%v", d.CurrentSegments, d.StandingSegments)
	}
}

func TestResolutionTokenRoundTripAndRejectCrossInvocation(t *testing.T) {
	secret := []byte("token-secret")
	cands := []Segment{{ID: "a", ContentDigest: "d1", OriginalIndex: 0}, {ID: "b", ContentDigest: "d2", OriginalIndex: 1}}
	now := time.Unix(1_700_000_000, 0).UTC()
	tok, err := IssueResolutionToken(secret, "t1", "c1", "inv1_abc", "rd1", cands, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := VerifyResolutionToken(secret, tok, "t1", "c1", "inv1_abc", "rd1", now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	d, err := ApplyResolution(payload, ResolutionRequest{CurrentSegments: []string{"b"}, StandingSegments: []string{"a"}})
	if err != nil || d.Kind != DecisionResolvedExplicit {
		t.Fatalf("apply=%v err=%v", d, err)
	}
	if _, err := VerifyResolutionToken(secret, tok, "t1", "c1", "inv1_other", "rd1", now.Add(10*time.Second)); err == nil {
		t.Fatal("cross-invocation reuse must fail")
	}
	if _, err := ApplyResolution(payload, ResolutionRequest{CurrentSegments: []string{"not-a-candidate"}}); err == nil {
		t.Fatal("unknown candidate must fail")
	}
}

func TestClarifyProtocolOutcome(t *testing.T) {
	secret := []byte("token-secret")
	raw := []byte(`{"messages":[{"role":"user","content":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},{"role":"user","content":"b"}]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	r := &Resolver{Secret: secret}
	in := Input{Segments: segs, TenantID: "t1", ConversationID: "c1", InvocationID: "inv1_clarify"}
	d := r.Resolve(in)
	cerr := r.Clarify(in, d, RequestDigest(raw))
	if cerr == nil {
		t.Fatal("expected clarification error")
	}
	out := cerr.ProtocolOutcome()
	if out.Object != "cliproxy.provenance_clarification" || out.StopReason != StopReasonClarification {
		t.Fatalf("outcome=%+v", out)
	}
	if out.ResolutionToken == "" || len(out.Candidates) != 2 {
		t.Fatalf("token/candidates missing: %+v", out)
	}
	if cerr.AuthAttributable() {
		t.Fatal("clarification must not attribute auth")
	}
}

func TestApplyResolutionRejectsIncompleteSelections(t *testing.T) {
	payload := &ResolutionTokenPayload{CandidateIDs: []string{"a", "b"}}
	if _, err := ApplyResolution(payload, ResolutionRequest{}); err == nil {
		t.Fatal("empty selection must fail")
	}
	if _, err := ApplyResolution(payload, ResolutionRequest{CurrentSegments: []string{"a"}}); err == nil {
		t.Fatal("omitted candidate must fail")
	}
	if _, err := ApplyResolution(payload, ResolutionRequest{
		CurrentSegments:  []string{"a"},
		StandingSegments: []string{"a"},
	}); err == nil {
		t.Fatal("overlapping current/standing must fail")
	}
	if _, err := ApplyResolution(payload, ResolutionRequest{
		CurrentSegments:  []string{"a", "a"},
		StandingSegments: []string{"b"},
	}); err == nil {
		t.Fatal("duplicate current ids must fail")
	}
	if _, err := ApplyResolution(payload, ResolutionRequest{
		CurrentSegments:  []string{"a", "b"},
		StandingSegments: []string{"b"},
		MergeGroups:      [][]string{{"a", "b"}},
	}); err == nil {
		t.Fatal("merge conflicting with standing must fail")
	}
	if _, err := ApplyResolution(payload, ResolutionRequest{
		CurrentSegments: []string{"a", "b"},
		MergeGroups:     [][]string{{}},
	}); err == nil {
		t.Fatal("empty merge group must fail")
	}
}

// Incident-style regression: after bridge/process restart, conversation-scoped
// recurrence must still keep a repeated standing block from displacing the
// novel current user request (anonymized OMP/Railway failure shape).
func TestIncidentRestartStandingDoesNotDisplaceCurrent(t *testing.T) {
	secret := []byte("incident-restart-secret")
	store := NewMemoryStore(secret, 3, 32)
	standingText := "injected standing context block that is intentionally long enough"
	standing := contentDigest(RoleUser, standingText, false)
	for i := 0; i < 3; i++ {
		store.ObserveCompleted("tenant-incident", "conv-incident", standing, "neighbor-"+string(rune('a'+i)), 1)
	}

	// Simulate process restart: new resolver, same durable recurrence store.
	raw := []byte(`{"messages":[
		{"role":"user","content":"injected standing context block that is intentionally long enough"},
		{"role":"user","content":"please fix the actual bug now"}
	]}`)
	segs := ExtractSegments(raw, gjson.Result{})
	r := &Resolver{Store: store, Secret: secret}
	d := r.Resolve(Input{Segments: segs, TenantID: "tenant-incident", ConversationID: "conv-incident"})
	if d.Kind != DecisionResolvedRecurrence {
		t.Fatalf("kind=%s reason=%s evidence=%v", d.Kind, d.ReasonCode, d.Evidence)
	}
	if len(d.CurrentSegments) != 1 || len(d.StandingSegments) != 1 {
		t.Fatalf("current=%v standing=%v", d.CurrentSegments, d.StandingSegments)
	}
}

// P2.6: MessageText is the single canonical user-message text parser shared with the
// composer executor's routing/rendering (cursorMessageText delegates here). Non-standard
// part types (e.g. tool_result inside a user message) must NOT contribute, so provenance
// fingerprints and routing decisions see identical text in both subsystems.
func TestMessageTextCanonicalExtraction(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want string
	}{
		{name: "string content", msg: `{"content":"hello"}`, want: "hello"},
		{name: "text parts newline-joined", msg: `{"content":[{"type":"text","text":"foo"},{"type":"text","text":"bar"}]}`, want: "foo\nbar"},
		{name: "non-text parts excluded", msg: `{"content":[{"type":"tool_result","text":"stale"},{"type":"text","text":"fresh"}]}`, want: "fresh"},
		{name: "empty text parts skipped", msg: `{"content":[{"type":"text","text":""},{"type":"text","text":"kept"}]}`, want: "kept"},
		{name: "input_text accepted", msg: `{"content":[{"type":"input_text","text":"in"}]}`, want: "in"},
		{name: "fallback to top-level text", msg: `{"text":"top"}`, want: "top"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MessageText(gjson.Parse(tc.msg)); got != tc.want {
				t.Fatalf("MessageText(%s) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}
