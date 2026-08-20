package secretdlp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSessionRestoresModelMutatedVerdictPlaceholderAcrossStreamChunks(t *testing.T) {
	const artifactID = "ra_79704d89709f47a897bc71f151b60c05"

	session := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	redacted := redactRawForTest(t, session, []byte(artifactID), []Finding{{
		Secret: artifactID,
		RuleID: "test",
		Source: "test",
	}})
	placeholder := extractPlaceholderForTest(t, string(redacted))
	authSeparator := len(placeholder) - len("_12345678901__")
	mutated := placeholder[:authSeparator+1] + "_" + placeholder[authSeparator+1:]
	svc := newSegmentPolicyTestService(t)
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}()
	if err := svc.persistMappings(context.Background(), session, []Mapping{{
		Placeholder: placeholder,
		Secret:      []byte(artifactID),
	}}); err != nil {
		t.Fatalf("persistMappings(): %v", err)
	}
	sameClient := NewSession([]byte("master-key"), "client-key", time.Minute, ModeRestore)
	ctx := WithSession(context.Background(), sameClient)

	chunk := `data: {"id":"chatcmpl-judge","object":"chat.completion.chunk","choices":[{"delta":{"content":"VERDICT ` + mutated + ` approve: viable"}}]}` + "\n\n"
	split := strings.Index(chunk, mutated) + len(mutated)/2

	var restored []byte
	restored = append(restored, svc.RestoreStreamChunk(ctx, []byte(chunk[:split]))...)
	restored = append(restored, svc.RestoreStreamChunk(ctx, []byte(chunk[split:]))...)
	restored = append(restored, svc.FlushStream(ctx)...)

	if strings.Contains(string(restored), placeholderPrefix) {
		t.Fatalf("streamed verdict still contains DLP placeholder: %s", restored)
	}
	if !strings.Contains(string(restored), "VERDICT "+artifactID+" approve:") {
		t.Fatalf("streamed verdict = %q, want restored artifact id %q", restored, artifactID)
	}

	otherClient := NewSession([]byte("master-key"), "other-client-key", time.Minute, ModeRestore)
	untrusted := svc.RestoreResponse(WithSession(context.Background(), otherClient), []byte(`{"content":"VERDICT `+mutated+` approve"}`))
	if strings.Contains(string(untrusted), artifactID) {
		t.Fatalf("other-client response restored authenticated artifact id: %s", untrusted)
	}
}
