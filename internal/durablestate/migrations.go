package durablestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// MigrationSnapshot is a dry-run / verify view of durable state counts.
type MigrationSnapshot struct {
	StateEpoch        int64            `json:"state_epoch"`
	InvocationCounts  map[string]int64 `json:"invocation_counts_by_phase"`
	ReservationCount  int64            `json:"reservation_count"`
	ReservationBytes  int64            `json:"reservation_bytes"`
	JournalEventCount int64            `json:"journal_event_count"`
	TakenAt           time.Time        `json:"taken_at"`
}

// MigrationPlan describes a version-safe epoch advance.
type MigrationPlan struct {
	FromEpoch int64 `json:"from_epoch"`
	ToEpoch   int64 `json:"to_epoch"`
	DryRun    bool  `json:"dry_run"`
}

// SnapshotState counts durable records for migration verification.
func (c *Coordinator) SnapshotState(ctx context.Context) (*MigrationSnapshot, error) {
	epoch, err := c.backend.StateEpoch(ctx)
	if err != nil {
		return nil, err
	}
	snap := &MigrationSnapshot{
		StateEpoch:       epoch,
		InvocationCounts: map[string]int64{},
		TakenAt:          c.now().UTC(),
	}
	phases := []cliproxyexecutor.AcceptancePhase{
		cliproxyexecutor.AcceptanceNotSent,
		cliproxyexecutor.AcceptancePreparedDurable,
		cliproxyexecutor.AcceptanceMaybeAccepted,
		cliproxyexecutor.AcceptanceAccepted,
		cliproxyexecutor.AcceptanceCompleted,
		cliproxyexecutor.AcceptanceRejectedBeforeSend,
	}
	for _, phase := range phases {
		recs, err := c.backend.ListInvocationsByPhases(ctx, []cliproxyexecutor.AcceptancePhase{phase}, 100000)
		if err != nil {
			return nil, err
		}
		snap.InvocationCounts[string(phase)] = int64(len(recs))
	}
	if bytes, err := c.backend.ReservationBytes(ctx, ""); err == nil {
		snap.ReservationBytes = bytes
	}
	if counter, ok := c.backend.(interface {
		CountReservations(ctx context.Context) (int64, error)
		CountJournalEvents(ctx context.Context) (int64, error)
	}); ok {
		if n, err := counter.CountReservations(ctx); err == nil {
			snap.ReservationCount = n
		}
		if n, err := counter.CountJournalEvents(ctx); err == nil {
			snap.JournalEventCount = n
		}
	}
	return snap, nil
}

// AdvanceEpoch bumps the durable state epoch after drain + checkpoint.
// Dry-run returns the planned epochs without mutating meta.
func (c *Coordinator) AdvanceEpoch(ctx context.Context, plan MigrationPlan) (*MigrationSnapshot, error) {
	if !c.gate.Draining() && !plan.DryRun {
		return nil, &PhaseTransitionError{Code: CodeConflict, Message: "begin_drain required before epoch advance"}
	}
	current, err := c.backend.StateEpoch(ctx)
	if err != nil {
		return nil, err
	}
	if plan.FromEpoch > 0 && plan.FromEpoch != current {
		return nil, &PhaseTransitionError{
			Code:    CodeConflict,
			Message: fmt.Sprintf("expected from_epoch %d, current %d", plan.FromEpoch, current),
		}
	}
	to := plan.ToEpoch
	if to <= 0 {
		to = current + 1
	}
	if to <= current {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "to_epoch must exceed current epoch"}
	}
	before, err := c.SnapshotState(ctx)
	if err != nil {
		return nil, err
	}
	if plan.DryRun {
		before.StateEpoch = to
		return before, nil
	}
	advancer, ok := c.backend.(interface {
		SetStateEpoch(ctx context.Context, epoch int64) error
	})
	if !ok {
		return nil, fmt.Errorf("backend does not support epoch advance")
	}
	if err := advancer.SetStateEpoch(ctx, to); err != nil {
		return nil, err
	}
	after, err := c.SnapshotState(ctx)
	if err != nil {
		return nil, err
	}
	return after, nil
}

// CASReceiptImport is one dual-read legacy file receipt candidate.
type CASReceiptImport struct {
	Path           string                           `json:"path"`
	InvocationID   string                           `json:"invocation_id"`
	Phase          cliproxyexecutor.AcceptancePhase `json:"phase"`
	EnvelopeDigest string                           `json:"envelope_digest"`
	RequestHash    string                           `json:"request_hash"`
	TenantID       string                           `json:"tenant_id,omitempty"`
	ConversationID string                           `json:"conversation_id,omitempty"`
	AuthID         string                           `json:"auth_id,omitempty"`
	IdempotencyKey string                           `json:"idempotency_key,omitempty"`
	RawVersion     int                              `json:"raw_version"`
}

// ImportCASReceipts dual-reads legacy CAS receipt files into invocation records.
// Existing records are never smashed; conflicts are reported and skipped.
func (c *Coordinator) ImportCASReceipts(ctx context.Context, receipts []CASReceiptImport) (imported, skipped int, err error) {
	for _, r := range receipts {
		if r.InvocationID == "" || r.Phase == "" {
			skipped++
			continue
		}
		existing, getErr := c.backend.GetInvocation(ctx, r.InvocationID)
		if getErr != nil {
			var pe *PhaseTransitionError
			if errors.As(getErr, &pe) && pe.Code == CodeNotFound {
				existing = nil
			} else {
				return imported, skipped, getErr
			}
		}
		if existing != nil {
			skipped++
			continue
		}
		rec := &InvocationRecord{
			InvocationID:         r.InvocationID,
			TenantID:             r.TenantID,
			ConversationID:       r.ConversationID,
			ClientIdempotencyKey: r.IdempotencyKey,
			CanonicalRequestHash: r.RequestHash,
			Phase:                r.Phase,
			AuthID:               r.AuthID,
			EnvelopeDigest:       r.EnvelopeDigest,
		}
		if err := c.backend.PutInvocation(ctx, rec); err != nil {
			return imported, skipped, err
		}
		imported++
	}
	return imported, skipped, nil
}

// ScanLegacyCASReceiptDir walks a directory of JSON receipt files for dual-read import.
func ScanLegacyCASReceiptDir(root string) ([]CASReceiptImport, error) {
	var out []CASReceiptImport
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || !strings.HasSuffix(strings.ToLower(d.Name()), ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var rec map[string]any
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil // skip malformed during dual-read
		}
		imp := CASReceiptImport{Path: path}
		if v, ok := rec["version"].(float64); ok {
			imp.RawVersion = int(v)
		}
		imp.InvocationID = stringField(rec, "invocationId", "invocation_id", "clientMessageId")
		imp.RequestHash = stringField(rec, "requestHash", "request_hash")
		imp.EnvelopeDigest = stringField(rec, "envelopeDigest", "envelope_digest", "deliveryIdempotencyKey")
		imp.AuthID = stringField(rec, "agentId", "auth_id")
		imp.IdempotencyKey = stringField(rec, "deliveryIdempotencyKey", "idempotencyKey")
		imp.ConversationID = stringField(rec, "sessionId", "conversation_id")
		phase := stringField(rec, "acceptancePhase", "acceptance_phase")
		if phase == "" {
			// Conservative dual-read of legacy status fields.
			status := strings.ToLower(stringField(rec, "status"))
			switch status {
			case "completed":
				phase = string(cliproxyexecutor.AcceptanceCompleted)
			case "running":
				phase = string(cliproxyexecutor.AcceptanceAccepted)
			case "unknown", "delivering", "failed":
				phase = string(cliproxyexecutor.AcceptanceMaybeAccepted)
			default:
				phase = string(cliproxyexecutor.AcceptancePreparedDurable)
			}
		}
		imp.Phase = cliproxyexecutor.AcceptancePhase(strings.ToLower(phase))
		out = append(out, imp)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func stringField(rec map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := rec[k]; ok {
			switch t := v.(type) {
			case string:
				if strings.TrimSpace(t) != "" {
					return t
				}
			}
		}
	}
	return ""
}
