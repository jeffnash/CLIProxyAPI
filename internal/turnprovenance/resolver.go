package turnprovenance

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Resolver evaluates the provenance ladder for one inbound turn.
type Resolver struct {
	Store    Store
	Secret   []byte
	TokenTTL time.Duration
}

// Resolve runs explicit → fragments → recurrence → clarification.
func (r *Resolver) Resolve(in Input) Decision {
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	if len(in.Segments) == 0 {
		return Decision{Kind: DecisionResolvedExplicit, ReasonCode: ReasonExplicitRoles}
	}

	if d, ok := resolveExplicit(in.Segments); ok {
		return d
	}
	if d, ok := resolveFragments(in.Segments); ok {
		return d
	}

	// Attach store evidence when available and conversation is stable.
	if r != nil && r.Store != nil && in.TenantID != "" && in.ConversationID != "" {
		in.Recurrence = r.Store.Lookup(in.TenantID, in.ConversationID)
	}
	if d, ok := resolveRecurrence(in, r.secret()); ok {
		return d
	}

	if AmbiguousTrailingUsers(in.Segments) {
		reason := ReasonAmbiguousAdjacentUsers
		if in.ConversationID == "" {
			reason = ReasonNoStableConversation
		} else if !in.Recurrence.Enabled || len(in.Recurrence.Observations) == 0 {
			reason = ReasonColdStart
		}
		return Decision{
			Kind:       DecisionClarificationRequired,
			ReasonCode: reason,
			Candidates: userCandidates(in.Segments),
			Evidence: []Evidence{{
				Class: EvidenceAmbiguous,
				Code:  reason,
			}},
		}
	}

	// Single trailing user (or no competing users): treat as resolved by roles.
	if ids := trailingUserIDs(in.Segments); len(ids) <= 1 {
		var standing []string
		for _, seg := range in.Segments {
			if seg.Role == RoleSystem || seg.Role == RoleDeveloper {
				standing = append(standing, seg.ID)
			}
		}
		return Decision{
			Kind:             DecisionResolvedExplicit,
			CurrentSegments:  ids,
			StandingSegments: standing,
			ReasonCode:       ReasonExplicitRoles,
			Candidates:       userCandidates(in.Segments),
		}
	}

	return Decision{
		Kind:       DecisionClarificationRequired,
		ReasonCode: ReasonAmbiguousAdjacentUsers,
		Candidates: userCandidates(in.Segments),
		Evidence: []Evidence{{
			Class: EvidenceAmbiguous,
			Code:  ReasonAmbiguousAdjacentUsers,
		}},
	}
}

// ObserveCompletedTurn records standing/current fingerprints after a COMPLETED turn.
// Non-completed / rejected turns must not call this.
func (r *Resolver) ObserveCompletedTurn(in Input) {
	if r == nil || r.Store == nil || in.TenantID == "" || in.ConversationID == "" {
		return
	}
	users := trailingUserSegments(in.Segments)
	if len(users) < 2 {
		return
	}
	standing := users[len(users)-2]
	current := users[len(users)-1]
	r.Store.ObserveCompleted(in.TenantID, in.ConversationID, standing.ContentDigest, current.ContentDigest, 1)
}

// Clarify builds a typed clarification error with a signed resolution token when possible.
func (r *Resolver) Clarify(in Input, decision Decision, requestDigest string) *ClarificationError {
	if !decision.ClarificationRequired() {
		return nil
	}
	err := &ClarificationError{
		Decision:     decision,
		InvocationID: in.InvocationID,
		Message:      "provenance clarification required",
	}
	secret := r.secret()
	if len(secret) == 0 {
		return err
	}
	token, issueErr := IssueResolutionToken(secret, in.TenantID, in.ConversationID, in.InvocationID, requestDigest, decision.Candidates, in.Now, r.TokenTTL)
	if issueErr == nil {
		err.ResolutionToken = token
	}
	return err
}

// ProtocolOutcome builds the capable-client typed clarification payload.
func (e *ClarificationError) ProtocolOutcome() cliproxyexecutor.ProtocolOutcome {
	if e == nil {
		return cliproxyexecutor.ProtocolOutcome{}
	}
	cands := make([]cliproxyexecutor.ProtocolCandidateSegment, 0, len(e.Decision.Candidates))
	for _, c := range e.Decision.Candidates {
		cands = append(cands, cliproxyexecutor.ProtocolCandidateSegment{
			ID:            c.ID,
			Digest:        c.ContentDigest,
			OriginalIndex: c.OriginalIndex,
			Preview:       c.TextPreview,
		})
	}
	return cliproxyexecutor.ProtocolOutcome{
		Object:          "cliproxy.provenance_clarification",
		Kind:            cliproxyexecutor.ProtocolOutcomeProvenanceClarification,
		StopReason:      StopReasonClarification,
		InvocationID:    e.InvocationID,
		Candidates:      cands,
		ResolutionToken: e.ResolutionToken,
		Instructions:    "Select current/standing/merge groups from the signed candidate set and resubmit with the resolution token.",
	}
}

func (r *Resolver) secret() []byte {
	if r != nil && len(r.Secret) > 0 {
		return r.Secret
	}
	if s, err := TokenSecret(); err == nil {
		return s
	}
	return nil
}

// RequestDigest hashes canonical request bytes for token binding.
func RequestDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
