package durablestate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Coordinator is the Go-owned durable-state service entrypoint.
type Coordinator struct {
	backend Backend
	now     func() time.Time
	gate    AdmissionGate
	blobs   *BlobStore
	flags   FeatureFlags
	metrics *Metrics
}

// NewCoordinator wraps a backend. Callers must hold an exclusive writer lease
// before mutating shared state in multi-writer deployments.
func NewCoordinator(backend Backend) *Coordinator {
	return &Coordinator{
		backend: backend,
		now:     time.Now,
		flags:   LoadFeatureFlagsFromEnv(),
		metrics: &DefaultMetrics,
	}
}

// WithBlobStore attaches an encrypted content-addressed blob store.
func (c *Coordinator) WithBlobStore(store *BlobStore) *Coordinator {
	if c != nil {
		c.blobs = store
	}
	return c
}

// Flags returns rollout feature flags.
func (c *Coordinator) Flags() FeatureFlags {
	if c == nil {
		return FeatureFlags{}
	}
	return c.flags
}

// PersistEnvelope stores plaintext via the blob store when encryption is enabled.
func (c *Coordinator) PersistEnvelope(tenantID, conversationID, invocationID string, plaintext []byte) (digest, ref string, err error) {
	if c == nil {
		return "", "", fmt.Errorf("coordinator required")
	}
	store := c.blobs
	if !c.flags.EncryptedState {
		store = nil
	}
	digest, ref, err = PersistEnvelopeBlob(store, tenantID, conversationID, invocationID, plaintext)
	if err != nil {
		return "", "", err
	}
	if c.metrics != nil {
		c.metrics.BlobPuts.Add(1)
	}
	return digest, ref, nil
}

// Admission returns the restart/migration admission gate.
func (c *Coordinator) Admission() *AdmissionGate {
	if c == nil {
		return nil
	}
	return &c.gate
}

// Backend exposes the underlying store for readiness probes.
func (c *Coordinator) Backend() Backend {
	if c == nil {
		return nil
	}
	return c.backend
}

// Close releases backend resources.
func (c *Coordinator) Close() error {
	if c == nil || c.backend == nil {
		return nil
	}
	return c.backend.Close()
}

// GetInvocation returns the durable invocation record.
func (c *Coordinator) GetInvocation(ctx context.Context, invocationID string) (*InvocationRecord, error) {
	if invocationID == "" {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invocation_id required"}
	}
	return c.backend.GetInvocation(ctx, invocationID)
}

// TransitionPhase validates and commits an acceptance-phase transition.
// The call returns only after the backend acknowledges durability.
func (c *Coordinator) TransitionPhase(ctx context.Context, payload TransitionPhasePayload) (*InvocationRecord, error) {
	if payload.InvocationID == "" {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invocation_id required"}
	}
	if !cliproxyexecutor.IsValidAcceptancePhase(payload.To) {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invalid target phase", To: payload.To}
	}
	if payload.From != "" && !cliproxyexecutor.CanTransitionAcceptance(payload.From, payload.To) {
		return nil, &PhaseTransitionError{
			Code:    CodeIllegalTransition,
			Message: fmt.Sprintf("illegal acceptance transition %q -> %q", payload.From, payload.To),
			From:    payload.From,
			To:      payload.To,
		}
	}
	return c.backend.TransitionPhase(ctx, payload)
}

// ReserveCapacity reserves exact durable bytes and returns only after commit.
func (c *Coordinator) ReserveCapacity(ctx context.Context, payload ReservePayload) (*ReservationRecord, error) {
	if payload.InvocationID == "" || payload.Bytes <= 0 {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invocation_id and positive bytes required"}
	}
	if err := c.gate.AllowAdmission(payload.Priority); err != nil {
		return nil, err
	}
	if payload.Kind == "" {
		payload.Kind = "envelope"
	}
	return c.backend.Reserve(ctx, payload)
}

func (c *Coordinator) ReleaseReservation(ctx context.Context, reservationID string) error {
	if reservationID == "" {
		return &PhaseTransitionError{Code: CodeInvalidRequest, Message: "reservation id required"}
	}
	return c.backend.ReleaseReservation(ctx, reservationID)
}

// ResizeReservation shrinks a reservation to the exact persisted size (or grows under admission rules).
func (c *Coordinator) ResizeReservation(ctx context.Context, reservationID string, exactBytes int64, priority int) (*ReservationRecord, error) {
	if reservationID == "" || exactBytes <= 0 {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "reservation id and positive bytes required"}
	}
	return c.backend.ResizeReservation(ctx, reservationID, exactBytes, priority)
}

// AppendJournal commits one stream event before the caller may expose it.
func (c *Coordinator) AppendJournal(ctx context.Context, event JournalEvent) (*JournalEvent, error) {
	if event.InvocationID == "" || event.Type == "" {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invocation_id and type required"}
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = c.now().UTC()
	}
	return c.backend.AppendJournal(ctx, event)
}

// ReadJournal returns events strictly after fromSequence.
func (c *Coordinator) ReadJournal(ctx context.Context, invocationID string, fromSequence int64, limit int) ([]JournalEvent, error) {
	if invocationID == "" {
		return nil, &PhaseTransitionError{Code: CodeInvalidRequest, Message: "invocation_id required"}
	}
	if limit <= 0 {
		limit = 256
	}
	return c.backend.ReadJournal(ctx, invocationID, fromSequence, limit)
}

// HandleRequest dispatches one protocol request. Used by the Unix socket server.
func (c *Coordinator) HandleRequest(ctx context.Context, req Request) Response {
	resp := Response{Version: ProtocolVersion, ID: req.ID}
	if req.Version != 0 && req.Version != ProtocolVersion {
		resp.Error = fmt.Sprintf("unsupported protocol version %d", req.Version)
		resp.Code = CodeInvalidRequest
		return resp
	}

	switch req.Op {
	case OpPing:
		payload, _ := json.Marshal(map[string]any{"pong": true, "ts": c.now().UTC()})
		resp.OK = true
		resp.Payload = payload
		return resp
	case OpGetInvocation:
		var p struct {
			InvocationID string `json:"invocation_id"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		rec, err := c.GetInvocation(ctx, p.InvocationID)
		return c.encodeResult(resp, rec, err)
	case OpTransitionPhase:
		var p TransitionPhasePayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		rec, err := c.TransitionPhase(ctx, p)
		return c.encodeResult(resp, rec, err)
	case OpReserve:
		var p ReservePayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		rec, err := c.ReserveCapacity(ctx, p)
		return c.encodeResult(resp, rec, err)
	case OpReleaseReservation:
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		err := c.ReleaseReservation(ctx, p.ID)
		return c.encodeResult(resp, map[string]bool{"released": err == nil}, err)
	case OpAppendJournal:
		var p AppendJournalPayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		ev, err := c.AppendJournal(ctx, p.Event)
		return c.encodeResult(resp, ev, err)
	case OpReadJournal:
		var p ReadJournalPayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		events, err := c.ReadJournal(ctx, p.InvocationID, p.FromSequence, p.Limit)
		return c.encodeResult(resp, map[string]any{"events": events}, err)
	case OpAcquireLease:
		var p LeasePayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		lease, err := c.backend.AcquireLease(ctx, p)
		return c.encodeResult(resp, lease, err)
	case OpRenewLease:
		var p LeasePayload
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		lease, err := c.backend.RenewLease(ctx, p)
		return c.encodeResult(resp, lease, err)
	case OpReleaseLease:
		var p struct {
			InstanceID string `json:"instance_id"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		err := c.backend.ReleaseLease(ctx, p.InstanceID)
		return c.encodeResult(resp, map[string]bool{"released": err == nil}, err)
	case OpCurrentLease:
		lease, err := c.backend.CurrentLease(ctx)
		return c.encodeResult(resp, lease, err)
	case OpListUnresolved:
		var p struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(req.Payload, &p)
		out, err := c.ListUnresolved(ctx, p.Limit)
		return c.encodeResult(resp, map[string]any{"invocations": out}, err)
	case OpInspectInvocation:
		var p struct {
			InvocationID string `json:"invocation_id"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		out, err := c.InspectInvocation(ctx, p.InvocationID)
		return c.encodeResult(resp, out, err)
	case OpReconcileReservations:
		var p struct {
			TenantID string `json:"tenant_id"`
		}
		_ = json.Unmarshal(req.Payload, &p)
		used, err := c.ReconcileReservations(ctx, p.TenantID)
		return c.encodeResult(resp, map[string]any{"bytes": used}, err)
	case OpApplyCompaction:
		var p struct {
			InvocationID            string `json:"invocation_id"`
			SendBoundaryProofAbsent bool   `json:"send_boundary_proof_absent"`
			RejectedRetentionHours  int    `json:"rejected_retention_hours"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		policy := CompactionPolicy{}
		if p.RejectedRetentionHours > 0 {
			policy.RejectedRetention = time.Duration(p.RejectedRetentionHours) * time.Hour
		}
		out, err := c.ApplyCompaction(ctx, p.InvocationID, policy, p.SendBoundaryProofAbsent)
		return c.encodeResult(resp, out, err)
	case OpQuarantine:
		var p struct {
			InvocationID string `json:"invocation_id"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		out, err := c.QuarantineUnresolved(ctx, p.InvocationID)
		return c.encodeResult(resp, out, err)
	case OpBeginDrain:
		c.gate.BeginDrain()
		return c.encodeResult(resp, map[string]any{"draining": true}, nil)
	case OpEndDrain:
		c.gate.EndDrain()
		return c.encodeResult(resp, map[string]any{"draining": false}, nil)
	case OpCheckpointUnresolved:
		var p struct {
			Limit int `json:"limit"`
		}
		_ = json.Unmarshal(req.Payload, &p)
		out, err := c.CheckpointUnresolved(ctx, p.Limit)
		if err == nil && c.metrics != nil {
			c.metrics.UnresolvedListed.Add(int64(len(out)))
		}
		return c.encodeResult(resp, map[string]any{"invocations": out, "draining": c.gate.Draining()}, err)
	case OpSnapshotState:
		out, err := c.SnapshotState(ctx)
		return c.encodeResult(resp, out, err)
	case OpAdvanceEpoch:
		var p MigrationPlan
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		out, err := c.AdvanceEpoch(ctx, p)
		if err == nil && !p.DryRun && c.metrics != nil {
			c.metrics.EpochAdvances.Add(1)
		}
		return c.encodeResult(resp, out, err)
	case OpImportCASReceipts:
		var p struct {
			Dir      string             `json:"dir"`
			Receipts []CASReceiptImport `json:"receipts"`
		}
		if err := json.Unmarshal(req.Payload, &p); err != nil {
			resp.Code = CodeInvalidRequest
			resp.Error = err.Error()
			return resp
		}
		receipts := p.Receipts
		if p.Dir != "" {
			scanned, err := ScanLegacyCASReceiptDir(p.Dir)
			if err != nil {
				return c.encodeResult(resp, nil, err)
			}
			receipts = append(receipts, scanned...)
		}
		imported, skipped, err := c.ImportCASReceipts(ctx, receipts)
		if err == nil && c.metrics != nil {
			c.metrics.CASImports.Add(int64(imported))
		}
		return c.encodeResult(resp, map[string]any{"imported": imported, "skipped": skipped}, err)
	case OpMetrics:
		snap := map[string]int64{}
		if c.metrics != nil {
			snap = c.metrics.Snapshot()
		}
		return c.encodeResult(resp, map[string]any{"metrics": snap, "flags": c.flags}, nil)
	default:
		resp.Code = CodeInvalidRequest
		resp.Error = "unknown op: " + req.Op
		return resp
	}
}

func (c *Coordinator) encodeResult(resp Response, payload any, err error) Response {
	if err != nil {
		resp.OK = false
		resp.Error = err.Error()
		switch e := err.(type) {
		case *PhaseTransitionError:
			resp.Code = e.Code
		case *CapacityError:
			resp.Code = CodeCapacity
		default:
			resp.Code = CodeInternal
		}
		return resp
	}
	raw, marshalErr := json.Marshal(payload)
	if marshalErr != nil {
		resp.OK = false
		resp.Code = CodeInternal
		resp.Error = marshalErr.Error()
		return resp
	}
	resp.OK = true
	resp.Payload = raw
	return resp
}
