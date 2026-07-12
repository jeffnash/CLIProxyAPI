package executor

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

type legacyDispositionError struct {
	msg          string
	scope        RetryScope
	attributable bool
}

func (e *legacyDispositionError) Error() string          { return e.msg }
func (e *legacyDispositionError) RetryScope() RetryScope { return e.scope }
func (e *legacyDispositionError) AuthAttributable() bool { return e.attributable }

func TestAcceptancePhaseTransitions(t *testing.T) {
	legal := []struct {
		from, to AcceptancePhase
	}{
		{AcceptanceNotSent, AcceptancePreparedDurable},
		{AcceptanceNotSent, AcceptanceRejectedBeforeSend},
		{AcceptancePreparedDurable, AcceptanceMaybeAccepted},
		{AcceptancePreparedDurable, AcceptanceRejectedBeforeSend},
		{AcceptanceMaybeAccepted, AcceptanceAccepted},
		{AcceptanceAccepted, AcceptanceCompleted},
		{AcceptanceAccepted, AcceptanceAccepted},
		{AcceptanceCompleted, AcceptanceCompleted},
		{AcceptanceRejectedBeforeSend, AcceptanceRejectedBeforeSend},
	}
	for _, tc := range legal {
		if err := ValidateAcceptanceTransition(tc.from, tc.to); err != nil {
			t.Fatalf("expected legal %q -> %q: %v", tc.from, tc.to, err)
		}
	}

	illegal := []struct {
		from, to AcceptancePhase
	}{
		{AcceptanceNotSent, AcceptanceMaybeAccepted},
		{AcceptanceNotSent, AcceptanceAccepted},
		{AcceptancePreparedDurable, AcceptanceAccepted},
		{AcceptanceMaybeAccepted, AcceptanceCompleted},
		{AcceptanceMaybeAccepted, AcceptanceNotSent},
		{AcceptanceAccepted, AcceptanceMaybeAccepted},
		{AcceptanceCompleted, AcceptanceAccepted},
		{AcceptanceRejectedBeforeSend, AcceptancePreparedDurable},
		{AcceptanceNotSent, AcceptanceNotSent},
	}
	for _, tc := range illegal {
		if CanTransitionAcceptance(tc.from, tc.to) {
			t.Fatalf("expected illegal %q -> %q", tc.from, tc.to)
		}
	}
}

func TestAllowsCredentialRotationByPhase(t *testing.T) {
	cases := []struct {
		phase            AcceptancePhase
		authAttributable bool
		want             bool
	}{
		{AcceptanceNotSent, true, true},
		{AcceptanceNotSent, false, false},
		{AcceptanceRejectedBeforeSend, true, true},
		{AcceptancePreparedDurable, true, false},
		{AcceptanceMaybeAccepted, true, false},
		{AcceptanceAccepted, true, false},
		{AcceptanceCompleted, true, false},
	}
	for _, tc := range cases {
		if got := AllowsCredentialRotation(tc.phase, tc.authAttributable); got != tc.want {
			t.Fatalf("AllowsCredentialRotation(%q,%v)=%v want %v", tc.phase, tc.authAttributable, got, tc.want)
		}
	}
}

func TestRetryScopeFromAcceptancePhase(t *testing.T) {
	if got := RetryScopeFromAcceptancePhase(AcceptanceMaybeAccepted); got != RetryScopeSelectedExecution {
		t.Fatalf("maybe_accepted scope = %v", got)
	}
	if got := RetryScopeFromAcceptancePhase(AcceptanceNotSent); got != RetryScopeDefault {
		t.Fatalf("not_sent scope = %v", got)
	}
	if got := DefaultPhaseForUnknownFailure(); got != AcceptanceMaybeAccepted {
		t.Fatalf("unknown failure default = %q", got)
	}
}

func TestDispositionFromErrorDispositionAdapter(t *testing.T) {
	legacy := &legacyDispositionError{
		msg:          "selected",
		scope:        RetryScopeSelectedExecution,
		attributable: false,
	}
	d := DispositionFromErrorDisposition(legacy)
	if d == nil {
		t.Fatal("expected disposition")
	}
	if d.Phase != AcceptanceMaybeAccepted {
		t.Fatalf("phase = %q", d.Phase)
	}
	if d.RetryScope() != RetryScopeSelectedExecution {
		t.Fatalf("retry scope = %v", d.RetryScope())
	}
	if d.AuthAttributable() {
		t.Fatal("expected non-auth attributable")
	}
	var asDisposition ErrorDisposition = d
	if asDisposition.RetryScope() != RetryScopeSelectedExecution {
		t.Fatal("ExecutionDisposition must satisfy ErrorDisposition")
	}
	if !errors.Is(d, legacy) {
		t.Fatal("expected unwrap to legacy cause")
	}
}

func TestResolveExecutionIdentityPrecedence(t *testing.T) {
	headers := http.Header{}
	headers.Set(HeaderIdempotencyKey, "inv1_idem-0001")
	headers.Set(HeaderCLIProxyInvocationID, "inv1_cliproxy-0002")
	headers.Set(HeaderClientTurnID, "inv1_client-0003")
	body := []byte(`{"metadata":{"turn_id":"inv1_turn-0004","invocation_id":"inv1_meta-0005"}}`)

	got, err := ResolveExecutionIdentity(headers, body)
	if err != nil {
		t.Fatalf("ResolveExecutionIdentity: %v", err)
	}
	if got.InvocationID != "inv1_idem-0001" {
		t.Fatalf("Idempotency-Key should win, got %q", got.InvocationID)
	}
	if got.ClientIdempotencyKey != "inv1_idem-0001" {
		t.Fatalf("ClientIdempotencyKey = %q", got.ClientIdempotencyKey)
	}
	if got.ServerIssued {
		t.Fatal("client-supplied id must not be marked server-issued")
	}

	headers = http.Header{}
	headers.Set(HeaderCLIProxyInvocationID, "inv1_cliproxy-0002")
	got, err = ResolveExecutionIdentity(headers, body)
	if err != nil {
		t.Fatalf("ResolveExecutionIdentity: %v", err)
	}
	if got.InvocationID != "inv1_cliproxy-0002" {
		t.Fatalf("X-CLIProxy-Invocation-ID should win next, got %q", got.InvocationID)
	}

	got, err = ResolveExecutionIdentity(nil, body)
	if err != nil {
		t.Fatalf("ResolveExecutionIdentity: %v", err)
	}
	if got.InvocationID != "inv1_turn-0004" {
		t.Fatalf("metadata.turn_id should win without headers, got %q", got.InvocationID)
	}

	got, err = ResolveExecutionIdentity(nil, nil)
	if err != nil {
		t.Fatalf("ResolveExecutionIdentity: %v", err)
	}
	if !got.ServerIssued || !ValidInvocationID(got.InvocationID) {
		t.Fatalf("expected server-issued valid id, got %+v", got)
	}
	if !strings.HasPrefix(got.InvocationID, "inv1_") {
		t.Fatalf("server id prefix = %q", got.InvocationID)
	}
	if got.InvocationID == "inv1_00000000000000000000000000000000" {
		t.Fatal("must not emit constant fallback invocation id")
	}
}

func TestCapabilityAndPreferParsing(t *testing.T) {
	headers := http.Header{}
	headers.Set(HeaderCLIProxyCapabilities, "stream-resume-v1, invocation-id-v1")
	if !HasCapability(headers, CapabilityInvocationIDV1) {
		t.Fatal("expected invocation-id-v1")
	}
	headers.Set(HeaderPrefer, "return=minimal, invocation-handshake; wait=10")
	if !PrefersInvocationHandshake(headers) {
		t.Fatal("expected Prefer invocation-handshake")
	}
}

func TestApplyExecutionIdentityMetadata(t *testing.T) {
	identity := ExecutionIdentity{
		InvocationID:         "inv1_meta-0001",
		ClientIdempotencyKey: "inv1_meta-0001",
		ServerIssued:         false,
	}
	meta := ApplyExecutionIdentityMetadata(nil, identity)
	if meta[InvocationIDMetadataKey] != "inv1_meta-0001" {
		t.Fatalf("invocation metadata = %v", meta[InvocationIDMetadataKey])
	}
	if meta[ClientIdempotencyKeyMetadataKey] != "inv1_meta-0001" {
		t.Fatalf("idempotency metadata = %v", meta[ClientIdempotencyKeyMetadataKey])
	}
}
