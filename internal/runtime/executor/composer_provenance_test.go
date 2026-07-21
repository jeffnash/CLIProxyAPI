package executor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/turnprovenance"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestResolveComposerProvenanceLegacyClientPreservesAmbiguousColdStartForAllSDKModels(t *testing.T) {
	for _, model := range []string{
		"composer-2.5",
		"composer-2.5-fast-high",
		"grok-4.5",
		"grok-4.5-fast-high",
	} {
		t.Run(model, func(t *testing.T) {
			raw := []byte(`{"model":"` + model + `","messages":[
				{"role":"user","content":"standing injected context that is long enough"},
				{"role":"user","content":"do the work"}
			]}`)
			opts := cliproxyexecutor.Options{Headers: make(http.Header), OriginalRequest: raw}

			got, clarification := resolveComposerProvenance("tenant-a", raw, opts, true)
			if clarification != nil {
				t.Fatalf("legacy client received unusable clarification: %v", clarification)
			}
			if !bytes.Equal(got, raw) {
				t.Fatalf("legacy ambiguous request changed:\n got: %s\nwant: %s", got, raw)
			}
		})
	}
}

func TestComposerProvenanceUsesCanonicalClaudeAndHeaderConversationIdentity(t *testing.T) {
	claude := []byte(`{"metadata":{"user_id":"user_abc_account_def_session_new-session"}}`)
	if got := composerConversationIDForProvenance(nil, cliproxyexecutor.Options{OriginalRequest: claude}); got != "claude:new-session" {
		t.Fatalf("Claude provenance identity = %q, want %q", got, "claude:new-session")
	}

	headers := make(http.Header)
	headers.Set("X-Conversation-Id", "header-conversation")
	if got := composerConversationIDForProvenance(nil, cliproxyexecutor.Options{Headers: headers}); got != "header-conversation" {
		t.Fatalf("header provenance identity = %q, want %q", got, "header-conversation")
	}
}

func TestResolveComposerProvenanceCapableClientGetsSignedClarification(t *testing.T) {
	t.Setenv("CLIPROXY_TURN_PROVENANCE_SECRET", "provenance-test-secret")
	composerProvenanceOnce = sync.Once{}
	composerProvenanceResolver = nil
	t.Cleanup(func() {
		composerProvenanceOnce = sync.Once{}
		composerProvenanceResolver = nil
	})
	raw := []byte(`{"metadata":{"conversation_id":"conv-a"},"messages":[
		{"role":"user","content":"standing injected context that is long enough"},
		{"role":"user","content":"do the work"}
	]}`)
	headers := make(http.Header)
	headers.Set(cliproxyexecutor.HeaderCLIProxyCapabilities, cliproxyexecutor.CapabilityProvenanceClarificationV1)
	opts := cliproxyexecutor.Options{Headers: headers, OriginalRequest: raw}

	got, clarification := resolveComposerProvenance("tenant-a", raw, opts, true)
	if !bytes.Equal(got, raw) {
		t.Fatalf("clarification path changed request: %s", got)
	}
	if clarification == nil {
		t.Fatal("capable client must receive clarification")
	}
	if clarification.ResolutionToken == "" {
		t.Fatal("capable client clarification must include a signed resolution token")
	}
}

func TestExtractAndApplySignedResolution(t *testing.T) {
	secret := []byte("composer-provenance-test-secret")
	raw := []byte(`{"messages":[
		{"role":"user","content":"standing injected context that is long enough"},
		{"role":"user","content":"do the work"}
	]}`)
	segs := turnprovenance.ExtractSegments(raw, gjson.Result{})
	if len(segs) < 2 {
		t.Fatalf("segments=%d", len(segs))
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	tok, err := turnprovenance.IssueResolutionToken(secret, "tenant-a", "conv-a", "inv1_abc", turnprovenance.RequestDigest(raw), segs, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{
			"cliproxy": map[string]any{
				"resolution_token":  tok,
				"current_segments":  []string{segs[1].ID},
				"standing_segments": []string{segs[0].ID},
			},
		},
	})
	opts := cliproxyexecutor.Options{OriginalRequest: meta}
	req, ok := extractResolutionRequest(opts, raw)
	if !ok || req.ResolutionToken == "" {
		t.Fatalf("expected resolution request, got %#v", req)
	}
	r := &turnprovenance.Resolver{Secret: secret}
	in := turnprovenance.Input{
		TenantID:       "tenant-a",
		ConversationID: "conv-a",
		InvocationID:   "inv1_abc",
		Segments:       segs,
		Now:            now,
	}
	decision, cerr := applySignedResolution(r, in, turnprovenance.RequestDigest(raw), req)
	if cerr != nil {
		t.Fatalf("apply: %v", cerr)
	}
	rewritten, ok := rewriteStandingContext(raw, segs, decision)
	if !ok {
		t.Fatal("expected standing rewrite")
	}
	roles := gjson.GetBytes(rewritten, "messages.#.role").Array()
	if len(roles) != 2 || roles[0].String() != "system" || roles[1].String() != "user" {
		t.Fatalf("roles=%v rewritten=%s", roles, rewritten)
	}
}

func TestApplySignedResolutionRejectsBadBinding(t *testing.T) {
	secret := []byte("composer-provenance-test-secret")
	raw := []byte(`{"messages":[{"role":"user","content":"a"},{"role":"user","content":"b"}]}`)
	segs := turnprovenance.ExtractSegments(raw, gjson.Result{})
	now := time.Unix(1_700_000_000, 0).UTC()
	tok, err := turnprovenance.IssueResolutionToken(secret, "tenant-a", "conv-a", "inv1_abc", turnprovenance.RequestDigest(raw), segs, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r := &turnprovenance.Resolver{Secret: secret}
	in := turnprovenance.Input{TenantID: "tenant-a", ConversationID: "conv-a", InvocationID: "inv1_OTHER", Segments: segs, Now: now}
	req := turnprovenance.ResolutionRequest{
		ResolutionToken:  tok,
		CurrentSegments:  []string{segs[1].ID},
		StandingSegments: []string{segs[0].ID},
	}
	if _, cerr := applySignedResolution(r, in, turnprovenance.RequestDigest(raw), req); cerr == nil {
		t.Fatal("cross-invocation token must fail")
	}
}
