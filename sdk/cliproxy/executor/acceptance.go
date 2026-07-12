package executor

import (
	"fmt"
)

// AcceptancePhase is the durable send-boundary state for one invocation.
// Uncertainty is preserved as typed state until evidence proves recovery,
// reattachment, clarification, or a safe retry.
type AcceptancePhase string

const (
	AcceptanceNotSent            AcceptancePhase = "not_sent"
	AcceptancePreparedDurable    AcceptancePhase = "prepared_durable"
	AcceptanceMaybeAccepted      AcceptancePhase = "maybe_accepted"
	AcceptanceAccepted           AcceptancePhase = "accepted"
	AcceptanceCompleted          AcceptancePhase = "completed"
	AcceptanceRejectedBeforeSend AcceptancePhase = "rejected_before_send"
)

// EvidenceCode classifies why an acceptance phase was recorded.
type EvidenceCode string

const (
	EvidenceNone                    EvidenceCode = ""
	EvidencePreparedEnvelope        EvidenceCode = "prepared_envelope"
	EvidenceMaybeAcceptedCommit     EvidenceCode = "maybe_accepted_commit"
	EvidenceSDKAccepted             EvidenceCode = "sdk_accepted"
	EvidenceCompleted               EvidenceCode = "completed"
	EvidenceRejectedBeforeSend      EvidenceCode = "rejected_before_send"
	EvidenceUnknownFailure          EvidenceCode = "unknown_failure"
	EvidenceProvenanceClarification EvidenceCode = "provenance_clarification"
	EvidenceCompatSelectedExecution EvidenceCode = "compat_selected_execution"
)

// TerminalReason identifies why an invocation stopped without a model completion.
type TerminalReason string

const (
	TerminalReasonNone                    TerminalReason = ""
	TerminalReasonProvenanceClarification TerminalReason = "provenance_clarification"
	TerminalReasonRejectedBeforeSend      TerminalReason = "rejected_before_send"
	TerminalReasonCompleted               TerminalReason = "completed"
	TerminalReasonUnknownFailure          TerminalReason = "unknown_failure"
)

// ExecutionDisposition carries acceptance-phase truth independently from
// credential attribution. New code should prefer this over RetryScope alone.
type ExecutionDisposition struct {
	Phase              AcceptancePhase
	Evidence           EvidenceCode
	AuthAttributed     bool
	ExplicitRetryScope RetryScope
	InvocationID       string
	TerminalReason     TerminalReason
	Message            string
	Cause              error
}

// Error implements the error interface.
func (d *ExecutionDisposition) Error() string {
	if d == nil {
		return "execution disposition <nil>"
	}
	if d.Message != "" {
		return d.Message
	}
	if d.Cause != nil {
		return d.Cause.Error()
	}
	if d.TerminalReason != TerminalReasonNone {
		return string(d.TerminalReason)
	}
	if d.Phase != "" {
		return "execution disposition: " + string(d.Phase)
	}
	return "execution disposition"
}

// Unwrap exposes the wrapped cause when present.
func (d *ExecutionDisposition) Unwrap() error {
	if d == nil {
		return nil
	}
	return d.Cause
}

// RetryScope implements ErrorDisposition.
func (d *ExecutionDisposition) RetryScope() RetryScope {
	if d == nil {
		return RetryScopeDefault
	}
	if d.ExplicitRetryScope != RetryScopeDefault {
		return d.ExplicitRetryScope
	}
	return RetryScopeFromAcceptancePhase(d.Phase)
}

// AuthAttributable implements ErrorDisposition.
func (d *ExecutionDisposition) AuthAttributable() bool {
	if d == nil {
		return true
	}
	return d.AuthAttributed
}

// AcceptancePhase reports the durable phase carried by this disposition.
func (d *ExecutionDisposition) AcceptancePhase() AcceptancePhase {
	if d == nil {
		return DefaultPhaseForUnknownFailure()
	}
	if IsValidAcceptancePhase(d.Phase) {
		return d.Phase
	}
	return DefaultPhaseForUnknownFailure()
}

// AcceptancePhaseCarrier lets errors expose durable send-boundary state
// independently from credential attribution.
type AcceptancePhaseCarrier interface {
	error
	AcceptancePhase() AcceptancePhase
}

// IsValidAcceptancePhase reports whether phase is a known acceptance value.
func IsValidAcceptancePhase(phase AcceptancePhase) bool {
	switch phase {
	case AcceptanceNotSent,
		AcceptancePreparedDurable,
		AcceptanceMaybeAccepted,
		AcceptanceAccepted,
		AcceptanceCompleted,
		AcceptanceRejectedBeforeSend:
		return true
	default:
		return false
	}
}

// CanTransitionAcceptance reports whether from→to is a legal forward transition.
// COMPLETED and REJECTED_BEFORE_SEND are absorbing. Self-transitions are allowed
// for ACCEPTED plus the absorbing terminals so idempotent commits stay legal.
func CanTransitionAcceptance(from, to AcceptancePhase) bool {
	if !IsValidAcceptancePhase(from) || !IsValidAcceptancePhase(to) {
		return false
	}
	if from == to {
		return from == AcceptanceAccepted || from == AcceptanceCompleted || from == AcceptanceRejectedBeforeSend
	}
	switch from {
	case AcceptanceNotSent:
		return to == AcceptancePreparedDurable || to == AcceptanceRejectedBeforeSend
	case AcceptancePreparedDurable:
		return to == AcceptanceMaybeAccepted || to == AcceptanceRejectedBeforeSend
	case AcceptanceMaybeAccepted:
		return to == AcceptanceAccepted
	case AcceptanceAccepted:
		return to == AcceptanceCompleted
	case AcceptanceCompleted, AcceptanceRejectedBeforeSend:
		return false
	default:
		return false
	}
}

// ValidateAcceptanceTransition returns an error when from→to is illegal.
func ValidateAcceptanceTransition(from, to AcceptancePhase) error {
	if CanTransitionAcceptance(from, to) {
		return nil
	}
	return fmt.Errorf("illegal acceptance transition %q -> %q", from, to)
}

// RetryScopeFromAcceptancePhase maps acceptance truth onto the legacy retry scope.
// Once the send boundary may have been crossed, recovery stays on the selected execution.
func RetryScopeFromAcceptancePhase(phase AcceptancePhase) RetryScope {
	switch phase {
	case AcceptanceMaybeAccepted, AcceptanceAccepted, AcceptanceCompleted:
		return RetryScopeSelectedExecution
	default:
		return RetryScopeDefault
	}
}

// AllowsCredentialRotation reports whether credential rotation is permitted for phase.
// Auth-attributable failures only matter before the send boundary may have been crossed.
func AllowsCredentialRotation(phase AcceptancePhase, authAttributable bool) bool {
	switch phase {
	case AcceptanceNotSent, AcceptanceRejectedBeforeSend:
		return authAttributable
	case AcceptancePreparedDurable:
		// Allowed only after a durable pre-send rejection; callers must pass
		// AcceptanceRejectedBeforeSend once that evidence exists.
		return false
	case AcceptanceMaybeAccepted, AcceptanceAccepted, AcceptanceCompleted:
		return false
	default:
		// Unknown phases are treated as potentially accepted.
		return false
	}
}

// DefaultPhaseForUnknownFailure preserves uncertainty: unknown errors are
// treated as potentially accepted unless durable evidence proves otherwise.
func DefaultPhaseForUnknownFailure() AcceptancePhase {
	return AcceptanceMaybeAccepted
}

// DispositionFromErrorDisposition adapts a legacy ErrorDisposition into the phase machine.
// Selected-execution errors without auth blame map to MAYBE_ACCEPTED.
func DispositionFromErrorDisposition(err ErrorDisposition) *ExecutionDisposition {
	if err == nil {
		return nil
	}
	d := &ExecutionDisposition{
		Message:            err.Error(),
		AuthAttributed:     err.AuthAttributable(),
		ExplicitRetryScope: err.RetryScope(),
		Cause:              err,
		Evidence:           EvidenceCompatSelectedExecution,
	}
	switch err.RetryScope() {
	case RetryScopeSelectedExecution:
		d.Phase = AcceptanceMaybeAccepted
	default:
		d.Phase = AcceptanceNotSent
	}
	return d
}
