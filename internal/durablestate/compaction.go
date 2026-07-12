package durablestate

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// CompactionAction describes what may be done to an invocation's durable state.
type CompactionAction string

const (
	CompactionRetain             CompactionAction = "retain"
	CompactionPurgeSafe          CompactionAction = "purge_safe"
	CompactionCompactCompleted   CompactionAction = "compact_completed"
	CompactionQuarantineOperator CompactionAction = "quarantine_operator"
)

// CompactionDecision is the evidence-gated lifecycle decision for one invocation.
type CompactionDecision struct {
	InvocationID string
	Phase        cliproxyexecutor.AcceptancePhase
	Action       CompactionAction
	Reason       string
}

// CompactionPolicy controls retention windows for safely purgeable records.
type CompactionPolicy struct {
	RejectedRetention time.Duration
	Now               func() time.Time
}

func (p CompactionPolicy) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

// DecideCompaction applies plan §11 evidence rules. Age alone never purges
// MAYBE_ACCEPTED or ACCEPTED unresolved evidence.
func DecideCompaction(rec *InvocationRecord, policy CompactionPolicy, sendBoundaryProofAbsent bool) CompactionDecision {
	if rec == nil {
		return CompactionDecision{Action: CompactionRetain, Reason: "missing_record"}
	}
	d := CompactionDecision{InvocationID: rec.InvocationID, Phase: rec.Phase}
	switch rec.Phase {
	case cliproxyexecutor.AcceptanceRejectedBeforeSend:
		ret := policy.RejectedRetention
		if ret <= 0 {
			ret = 24 * time.Hour
		}
		if policy.now().Sub(rec.PhaseUpdatedAt) >= ret {
			d.Action = CompactionPurgeSafe
			d.Reason = "rejected_before_send_past_retention"
			return d
		}
		d.Action = CompactionRetain
		d.Reason = "rejected_before_send_within_retention"
		return d
	case cliproxyexecutor.AcceptancePreparedDurable:
		if sendBoundaryProofAbsent {
			d.Action = CompactionPurgeSafe
			d.Reason = "prepared_orphan_send_boundary_not_crossed"
			return d
		}
		d.Action = CompactionRetain
		d.Reason = "prepared_without_negative_send_proof"
		return d
	case cliproxyexecutor.AcceptanceMaybeAccepted, cliproxyexecutor.AcceptanceAccepted:
		d.Action = CompactionRetain
		d.Reason = "unresolved_acceptance_evidence_must_be_retained"
		return d
	case cliproxyexecutor.AcceptanceCompleted:
		d.Action = CompactionCompactCompleted
		d.Reason = "completed_may_drop_envelope_keep_hashes_and_journal"
		return d
	case cliproxyexecutor.AcceptanceNotSent:
		d.Action = CompactionRetain
		d.Reason = "not_sent_awaiting_admission_or_rejection"
		return d
	default:
		d.Action = CompactionRetain
		d.Reason = "unknown_phase_fail_closed"
		return d
	}
}

// UnresolvedInvocation is a redacted operator view (no envelope plaintext).
type UnresolvedInvocation struct {
	InvocationID   string                           `json:"invocation_id"`
	TenantID       string                           `json:"tenant_id,omitempty"`
	ConversationID string                           `json:"conversation_id,omitempty"`
	Phase          cliproxyexecutor.AcceptancePhase `json:"phase"`
	Evidence       cliproxyexecutor.EvidenceCode    `json:"evidence,omitempty"`
	UpdatedAt      time.Time                        `json:"updated_at"`
	JournalCursor  int64                            `json:"journal_cursor"`
}

// ListUnresolved returns invocations that still require recovery attention.
func (c *Coordinator) ListUnresolved(ctx context.Context, limit int) ([]UnresolvedInvocation, error) {
	if limit <= 0 {
		limit = 256
	}
	recs, err := c.backend.ListInvocationsByPhases(ctx, []cliproxyexecutor.AcceptancePhase{
		cliproxyexecutor.AcceptancePreparedDurable,
		cliproxyexecutor.AcceptanceMaybeAccepted,
		cliproxyexecutor.AcceptanceAccepted,
	}, limit)
	if err != nil {
		return nil, err
	}
	out := make([]UnresolvedInvocation, 0, len(recs))
	for i := range recs {
		out = append(out, UnresolvedInvocation{
			InvocationID:   recs[i].InvocationID,
			TenantID:       recs[i].TenantID,
			ConversationID: recs[i].ConversationID,
			Phase:          recs[i].Phase,
			Evidence:       recs[i].Evidence,
			UpdatedAt:      recs[i].UpdatedAt,
			JournalCursor:  recs[i].JournalCursor,
		})
	}
	return out, nil
}

// InspectInvocation returns a redacted operator view of one invocation.
func (c *Coordinator) InspectInvocation(ctx context.Context, invocationID string) (*UnresolvedInvocation, error) {
	rec, err := c.GetInvocation(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "invocation not found"}
	}
	return &UnresolvedInvocation{
		InvocationID:   rec.InvocationID,
		TenantID:       rec.TenantID,
		ConversationID: rec.ConversationID,
		Phase:          rec.Phase,
		Evidence:       rec.Evidence,
		UpdatedAt:      rec.UpdatedAt,
		JournalCursor:  rec.JournalCursor,
	}, nil
}

// ApplyCompaction commits a proof-gated compaction decision.
func (c *Coordinator) ApplyCompaction(ctx context.Context, invocationID string, policy CompactionPolicy, sendBoundaryProofAbsent bool) (*CompactionDecision, error) {
	rec, err := c.GetInvocation(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		d := CompactionDecision{InvocationID: invocationID, Action: CompactionRetain, Reason: "missing_record"}
		return &d, &PhaseTransitionError{Code: CodeNotFound, Message: "invocation not found"}
	}
	d := DecideCompaction(rec, policy, sendBoundaryProofAbsent)
	switch d.Action {
	case CompactionPurgeSafe:
		if err := c.backend.DeleteInvocation(ctx, invocationID); err != nil {
			return &d, err
		}
	case CompactionCompactCompleted:
		if err := c.backend.ClearEnvelope(ctx, invocationID); err != nil {
			return &d, err
		}
	case CompactionQuarantineOperator:
		// Quarantine is an operator-visible retain; no destructive mutation.
	default:
		// retain
	}
	return &d, nil
}

// QuarantineUnresolved marks an irrecoverable unresolved invocation for operator review.
// It never deletes MAYBE_ACCEPTED/ACCEPTED evidence.
func (c *Coordinator) QuarantineUnresolved(ctx context.Context, invocationID string) (*CompactionDecision, error) {
	rec, err := c.GetInvocation(ctx, invocationID)
	if err != nil {
		return nil, err
	}
	if rec == nil {
		return nil, &PhaseTransitionError{Code: CodeNotFound, Message: "invocation not found"}
	}
	switch rec.Phase {
	case cliproxyexecutor.AcceptanceMaybeAccepted, cliproxyexecutor.AcceptanceAccepted, cliproxyexecutor.AcceptancePreparedDurable:
		d := CompactionDecision{
			InvocationID: invocationID,
			Phase:        rec.Phase,
			Action:       CompactionQuarantineOperator,
			Reason:       "operator_quarantine_retain_evidence",
		}
		return &d, nil
	default:
		return nil, &PhaseTransitionError{Code: CodeConflict, Message: "quarantine applies to unresolved acceptance evidence only"}
	}
}

// CheckpointUnresolved returns a drain-time snapshot of unresolved invocations.
func (c *Coordinator) CheckpointUnresolved(ctx context.Context, limit int) ([]UnresolvedInvocation, error) {
	return c.ListUnresolved(ctx, limit)
}

// ReconcileReservations returns used durable bytes for empty (global) or tenant scope.
func (c *Coordinator) ReconcileReservations(ctx context.Context, tenantID string) (int64, error) {
	return c.backend.ReservationBytes(ctx, tenantID)
}
