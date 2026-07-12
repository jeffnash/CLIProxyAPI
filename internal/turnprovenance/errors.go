package turnprovenance

import (
	"encoding/json"
	"fmt"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Error codes for typed clarification / legacy 422 mapping.
const (
	ErrTypeProvenanceAmbiguous = "provenance_ambiguous"
	StopReasonClarification    = "provenance_clarification"
)

// ClarificationError is returned when provenance cannot be resolved safely.
// Capable clients receive a typed ProtocolOutcome; legacy clients get HTTP 422.
type ClarificationError struct {
	Decision        Decision
	InvocationID    string
	ResolutionToken string
	Message         string
}

func (e *ClarificationError) Error() string {
	if e == nil {
		return "provenance clarification required"
	}
	payload := map[string]any{
		"error": map[string]any{
			"type":             ErrTypeProvenanceAmbiguous,
			"stop_reason":      StopReasonClarification,
			"resolution_token": e.ResolutionToken,
			"reason_code":      e.Decision.ReasonCode,
			"invocation_id":    e.InvocationID,
			"message":          e.message(),
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return e.message()
	}
	return string(b)
}

func (e *ClarificationError) message() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Decision.ReasonCode != "" {
		return fmt.Sprintf("provenance clarification required: %s", e.Decision.ReasonCode)
	}
	return "provenance clarification required"
}

// StatusCode returns the legacy HTTP status for non-capable clients.
func (e *ClarificationError) StatusCode() int { return 422 }

// AuthAttributable implements cliproxy ErrorDisposition compatibility.
func (e *ClarificationError) AuthAttributable() bool { return false }

// RetryScope keeps clarification off credential failover paths.
func (e *ClarificationError) RetryScope() cliproxyexecutor.RetryScope {
	return cliproxyexecutor.RetryScopeSelectedExecution
}

// AcceptancePhase records that clarification never crossed the send boundary.
func (e *ClarificationError) AcceptancePhase() cliproxyexecutor.AcceptancePhase {
	return cliproxyexecutor.AcceptanceNotSent
}
