package durablestate

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Backend persists invocations, reservations, journal events, and writer leases.
// Critical transitions must return only after the required durability commit.
type Backend interface {
	Close() error
	Ping(ctx context.Context) error

	GetInvocation(ctx context.Context, invocationID string) (*InvocationRecord, error)
	ListInvocationsByPhases(ctx context.Context, phases []cliproxyexecutor.AcceptancePhase, limit int) ([]InvocationRecord, error)
	PutInvocation(ctx context.Context, rec *InvocationRecord) error
	DeleteInvocation(ctx context.Context, invocationID string) error
	ClearEnvelope(ctx context.Context, invocationID string) error
	TransitionPhase(ctx context.Context, payload TransitionPhasePayload) (*InvocationRecord, error)

	Reserve(ctx context.Context, payload ReservePayload) (*ReservationRecord, error)
	ResizeReservation(ctx context.Context, reservationID string, exactBytes int64, priority int) (*ReservationRecord, error)
	ReleaseReservation(ctx context.Context, reservationID string) error
	ReservationBytes(ctx context.Context, tenantID string) (int64, error)

	AppendJournal(ctx context.Context, event JournalEvent) (*JournalEvent, error)
	ReadJournal(ctx context.Context, invocationID string, fromSequence int64, limit int) ([]JournalEvent, error)

	AcquireLease(ctx context.Context, payload LeasePayload) (*LeaseRecord, error)
	RenewLease(ctx context.Context, payload LeasePayload) (*LeaseRecord, error)
	ReleaseLease(ctx context.Context, instanceID string) error
	CurrentLease(ctx context.Context) (*LeaseRecord, error)

	StateEpoch(ctx context.Context) (int64, error)
}

// PhaseTransitionError is returned when a requested transition is illegal or conflicts.
type PhaseTransitionError struct {
	Code    string
	Message string
	From    cliproxyexecutor.AcceptancePhase
	To      cliproxyexecutor.AcceptancePhase
	Current cliproxyexecutor.AcceptancePhase
}

func (e *PhaseTransitionError) Error() string {
	if e == nil {
		return "phase transition error"
	}
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// CapacityError is returned when durable capacity cannot be reserved.
type CapacityError struct {
	Message    string
	RetryAfter time.Duration
}

func (e *CapacityError) Error() string {
	if e == nil || e.Message == "" {
		return "durable capacity unavailable"
	}
	return e.Message
}
