package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

func TestResolveAndIssueExecutionIdentityFromHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(coreexecutor.HeaderIdempotencyKey, "inv1_handler-0001")
	req.Header.Set(coreexecutor.HeaderCLIProxyCapabilities, coreexecutor.CapabilityInvocationIDV1)
	c.Request = req
	ctx := context.WithValue(context.Background(), "gin", c)

	ctx, identity, err := IssueExecutionIdentity(ctx, nil)
	if err != nil {
		t.Fatalf("IssueExecutionIdentity: %v", err)
	}
	if identity.InvocationID != "inv1_handler-0001" {
		t.Fatalf("identity = %q", identity.InvocationID)
	}
	if identity.ServerIssued {
		t.Fatal("expected client-supplied identity")
	}
	if got := recorder.Header().Get(coreexecutor.HeaderCLIProxyInvocationID); got != "inv1_handler-0001" {
		t.Fatalf("response header = %q", got)
	}
	if got := recorder.Header().Get(coreexecutor.HeaderCLIProxyCapabilities); got == "" {
		t.Fatal("expected capability advertisement")
	}
	if again, ok := executionIdentityFromContext(ctx); !ok || again.InvocationID != identity.InvocationID {
		t.Fatalf("context identity = %+v", again)
	}
}

func TestInvocationHandshakePreferReturnsWithoutExecution(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(coreexecutor.HeaderPrefer, coreexecutor.PreferInvocationHandshake)
	req.Header.Set(coreexecutor.HeaderCLIProxyCapabilities, coreexecutor.CapabilityInvocationIDV1)
	c.Request = req
	ctx := context.WithValue(context.Background(), "gin", c)

	_, body, headers, errMsg, handled := maybeInvocationHandshake(ctx, nil, false)
	if !handled {
		t.Fatal("expected handshake to handle request")
	}
	if errMsg != nil {
		t.Fatalf("unexpected error: %v", errMsg.Error)
	}
	var outcome coreexecutor.ProtocolOutcome
	if err := json.Unmarshal(body, &outcome); err != nil {
		t.Fatalf("decode handshake: %v", err)
	}
	if outcome.Object != "cliproxy.invocation_handshake" {
		t.Fatalf("object = %q", outcome.Object)
	}
	if !coreexecutor.ValidInvocationID(outcome.InvocationID) {
		t.Fatalf("invocation id = %q", outcome.InvocationID)
	}
	if got := headers.Get(coreexecutor.HeaderCLIProxyInvocationID); got != outcome.InvocationID {
		t.Fatalf("header = %q want %q", got, outcome.InvocationID)
	}
}

func TestFirstStreamInvocationControlEventRequiresCapability(t *testing.T) {
	identity := coreexecutor.ExecutionIdentity{InvocationID: "inv1_stream-0001"}
	if got := firstStreamInvocationControlEvent(identity, nil); got != nil {
		t.Fatalf("legacy clients must not receive control event, got %q", got)
	}
	headers := http.Header{}
	headers.Set(coreexecutor.HeaderCLIProxyCapabilities, coreexecutor.CapabilityInvocationIDV1)
	got := firstStreamInvocationControlEvent(identity, headers)
	if len(got) == 0 {
		t.Fatal("expected control event")
	}
	if !bytes.Contains(got, []byte("event: cliproxy.invocation\n")) {
		t.Fatalf("control event missing event name: %q", got)
	}
	if !bytes.Contains(got, []byte(`"invocation_id":"inv1_stream-0001"`)) {
		t.Fatalf("control event missing invocation id: %q", got)
	}
}
