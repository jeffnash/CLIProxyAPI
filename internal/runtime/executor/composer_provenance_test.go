package executor

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/turnprovenance"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

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
