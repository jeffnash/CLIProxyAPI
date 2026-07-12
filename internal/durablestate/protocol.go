// Package durablestate owns Go-side durable acceptance, reservation, and
// stream-journal metadata. The cursor bridge talks to it over a private Unix
// socket; Node's event loop must not perform synchronous fsync work for
// critical transitions.
package durablestate

import (
	"encoding/json"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// ProtocolVersion is the Unix-socket protocol version.
const ProtocolVersion = 1

// Op names for the versioned request/response protocol.
const (
	OpPing                  = "ping"
	OpBegin                 = "begin"
	OpCommit                = "commit"
	OpAbort                 = "abort"
	OpGetInvocation         = "get_invocation"
	OpTransitionPhase       = "transition_phase"
	OpReserve               = "reserve"
	OpReleaseReservation    = "release_reservation"
	OpAppendJournal         = "append_journal"
	OpReadJournal           = "read_journal"
	OpAcquireLease          = "acquire_lease"
	OpRenewLease            = "renew_lease"
	OpReleaseLease          = "release_lease"
	OpCurrentLease          = "current_lease"
	OpListUnresolved        = "list_unresolved"
	OpInspectInvocation     = "inspect_invocation"
	OpReconcileReservations = "reconcile_reservations"
	OpApplyCompaction       = "apply_compaction"
	OpQuarantine            = "quarantine"
	OpBeginDrain            = "begin_drain"
	OpEndDrain              = "end_drain"
	OpCheckpointUnresolved  = "checkpoint_unresolved"
	OpSnapshotState         = "snapshot_state"
	OpAdvanceEpoch          = "advance_epoch"
	OpImportCASReceipts     = "import_cas_receipts"
	OpMetrics               = "metrics"
)

// Request is one framed protocol request from the bridge.
type Request struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	Op      string          `json:"op"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Response is one framed protocol response.
type Response struct {
	Version int             `json:"version"`
	ID      string          `json:"id"`
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Code    string          `json:"code,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// InvocationRecord is the durable acceptance truth for one invocation.
type InvocationRecord struct {
	InvocationID         string                           `json:"invocation_id"`
	TenantID             string                           `json:"tenant_id,omitempty"`
	ConversationID       string                           `json:"conversation_id,omitempty"`
	ClientIdempotencyKey string                           `json:"client_idempotency_key,omitempty"`
	CanonicalRequestHash string                           `json:"canonical_request_hash,omitempty"`
	Phase                cliproxyexecutor.AcceptancePhase `json:"phase"`
	Evidence             cliproxyexecutor.EvidenceCode    `json:"evidence,omitempty"`
	TerminalReason       cliproxyexecutor.TerminalReason  `json:"terminal_reason,omitempty"`
	AuthID               string                           `json:"auth_id,omitempty"`
	EnvelopeDigest       string                           `json:"envelope_digest,omitempty"`
	EnvelopeBlobRef      string                           `json:"envelope_blob_ref,omitempty"`
	JournalCursor        int64                            `json:"journal_cursor"`
	CreatedAt            time.Time                        `json:"created_at"`
	UpdatedAt            time.Time                        `json:"updated_at"`
	PhaseUpdatedAt       time.Time                        `json:"phase_updated_at"`
}

// TransitionPhasePayload requests a legal acceptance-phase transition.
type TransitionPhasePayload struct {
	InvocationID    string                           `json:"invocation_id"`
	From            cliproxyexecutor.AcceptancePhase `json:"from"`
	To              cliproxyexecutor.AcceptancePhase `json:"to"`
	Evidence        cliproxyexecutor.EvidenceCode    `json:"evidence,omitempty"`
	TerminalReason  cliproxyexecutor.TerminalReason  `json:"terminal_reason,omitempty"`
	AuthID          string                           `json:"auth_id,omitempty"`
	EnvelopeDigest  string                           `json:"envelope_digest,omitempty"`
	EnvelopeBlobRef string                           `json:"envelope_blob_ref,omitempty"`
	// CreateIfMissing inserts NOT_SENT when the record does not yet exist.
	CreateIfMissing      bool   `json:"create_if_missing,omitempty"`
	TenantID             string `json:"tenant_id,omitempty"`
	ConversationID       string `json:"conversation_id,omitempty"`
	ClientIdempotencyKey string `json:"client_idempotency_key,omitempty"`
	CanonicalRequestHash string `json:"canonical_request_hash,omitempty"`
}

// ReservePayload reserves durable capacity for an invocation.
type ReservePayload struct {
	InvocationID string `json:"invocation_id"`
	TenantID     string `json:"tenant_id,omitempty"`
	Bytes        int64  `json:"bytes"`
	Priority     int    `json:"priority"`
	Kind         string `json:"kind"` // envelope|journal|tool_round
}

// ReservationRecord is a held durable capacity reservation.
type ReservationRecord struct {
	ID           string    `json:"id"`
	InvocationID string    `json:"invocation_id"`
	TenantID     string    `json:"tenant_id,omitempty"`
	Bytes        int64     `json:"bytes"`
	Priority     int       `json:"priority"`
	Kind         string    `json:"kind"`
	CreatedAt    time.Time `json:"created_at"`
}

// JournalEvent is one durable stream journal entry.
type JournalEvent struct {
	InvocationID    string                           `json:"invocation_id"`
	Sequence        int64                            `json:"sequence"`
	Type            string                           `json:"type"`
	Payload         json.RawMessage                  `json:"payload,omitempty"`
	PayloadDigest   string                           `json:"payload_digest,omitempty"`
	AcceptancePhase cliproxyexecutor.AcceptancePhase `json:"acceptance_phase,omitempty"`
	CreatedAt       time.Time                        `json:"created_at"`
}

// AppendJournalPayload appends one journal event and returns only after commit.
type AppendJournalPayload struct {
	Event JournalEvent `json:"event"`
}

// ReadJournalPayload reads journal events strictly after FromSequence.
type ReadJournalPayload struct {
	InvocationID string `json:"invocation_id"`
	FromSequence int64  `json:"from_sequence"`
	Limit        int    `json:"limit,omitempty"`
}

// LeasePayload acquires or renews a writer fencing lease.
type LeasePayload struct {
	InstanceID      string `json:"instance_id"`
	BinaryVersion   string `json:"binary_version"`
	StateEpoch      int64  `json:"state_epoch"`
	FencingGen      int64  `json:"fencing_generation,omitempty"`
	TTLMilliseconds int64  `json:"ttl_ms,omitempty"`
}

// LeaseRecord is the active writer lease.
type LeaseRecord struct {
	InstanceID        string    `json:"instance_id"`
	BinaryVersion     string    `json:"binary_version"`
	StateEpoch        int64     `json:"state_epoch"`
	FencingGeneration int64     `json:"fencing_generation"`
	ExpiresAt         time.Time `json:"expires_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// Error codes returned on the protocol wire.
const (
	CodeIllegalTransition = "illegal_transition"
	CodeNotFound          = "not_found"
	CodeConflict          = "conflict"
	CodeCapacity          = "capacity"
	CodeLeaseLost         = "lease_lost"
	CodeInvalidRequest    = "invalid_request"
	CodeInternal          = "internal"
)
