package durablestate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestSQLiteAcceptancePhaseMachine(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db"), MaxReservedBytes: 1 << 20})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	coord := NewCoordinator(backend)
	ctx := context.Background()

	rec, err := coord.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID:    "inv1_phase-0001",
		From:            cliproxyexecutor.AcceptanceNotSent,
		To:              cliproxyexecutor.AcceptancePreparedDurable,
		Evidence:        cliproxyexecutor.EvidencePreparedEnvelope,
		CreateIfMissing: true,
		TenantID:        "tenant-a",
		ConversationID:  "conv-a",
	})
	if err != nil {
		t.Fatalf("prepared_durable: %v", err)
	}
	if rec.Phase != cliproxyexecutor.AcceptancePreparedDurable {
		t.Fatalf("phase = %q", rec.Phase)
	}

	rec, err = coord.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID: "inv1_phase-0001",
		From:         cliproxyexecutor.AcceptancePreparedDurable,
		To:           cliproxyexecutor.AcceptanceMaybeAccepted,
		Evidence:     cliproxyexecutor.EvidenceMaybeAcceptedCommit,
	})
	if err != nil {
		t.Fatalf("maybe_accepted: %v", err)
	}

	_, err = coord.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID: "inv1_phase-0001",
		From:         cliproxyexecutor.AcceptanceMaybeAccepted,
		To:           cliproxyexecutor.AcceptanceCompleted, // illegal skip
	})
	if err == nil {
		t.Fatal("expected illegal transition")
	}
	phaseErr, ok := err.(*PhaseTransitionError)
	if !ok || phaseErr.Code != CodeIllegalTransition {
		t.Fatalf("expected illegal_transition, got %T %#v", err, err)
	}

	rec, err = coord.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID: "inv1_phase-0001",
		From:         cliproxyexecutor.AcceptanceMaybeAccepted,
		To:           cliproxyexecutor.AcceptanceAccepted,
		Evidence:     cliproxyexecutor.EvidenceSDKAccepted,
	})
	if err != nil {
		t.Fatalf("accepted: %v", err)
	}
	rec, err = coord.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID: "inv1_phase-0001",
		From:         cliproxyexecutor.AcceptanceAccepted,
		To:           cliproxyexecutor.AcceptanceCompleted,
		Evidence:     cliproxyexecutor.EvidenceCompleted,
	})
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if rec.Phase != cliproxyexecutor.AcceptanceCompleted {
		t.Fatalf("final phase = %q", rec.Phase)
	}
}

func TestJournalBeforeExposeAndResume(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	coord := NewCoordinator(backend)
	ctx := context.Background()

	if _, err := coord.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID:    "inv1_journal-0001",
		To:              cliproxyexecutor.AcceptanceNotSent,
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Move to accepted so journal events are meaningful.
	for _, step := range []struct {
		from, to cliproxyexecutor.AcceptancePhase
	}{
		{cliproxyexecutor.AcceptanceNotSent, cliproxyexecutor.AcceptancePreparedDurable},
		{cliproxyexecutor.AcceptancePreparedDurable, cliproxyexecutor.AcceptanceMaybeAccepted},
		{cliproxyexecutor.AcceptanceMaybeAccepted, cliproxyexecutor.AcceptanceAccepted},
	} {
		if _, err := coord.TransitionPhase(ctx, TransitionPhasePayload{
			InvocationID: "inv1_journal-0001",
			From:         step.from,
			To:           step.to,
		}); err != nil {
			t.Fatalf("transition %s->%s: %v", step.from, step.to, err)
		}
	}

	payload := json.RawMessage(`{"text":"hello"}`)
	sum := sha256.Sum256(payload)
	ev, err := coord.AppendJournal(ctx, JournalEvent{
		InvocationID:    "inv1_journal-0001",
		Type:            "text",
		Payload:         payload,
		PayloadDigest:   hex.EncodeToString(sum[:]),
		AcceptancePhase: cliproxyexecutor.AcceptanceAccepted,
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if ev.Sequence != 1 {
		t.Fatalf("sequence = %d", ev.Sequence)
	}
	events, err := coord.ReadJournal(ctx, "inv1_journal-0001", 0, 10)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 || events[0].Sequence != 1 {
		t.Fatalf("events = %#v", events)
	}
	events, err = coord.ReadJournal(ctx, "inv1_journal-0001", 1, 10)
	if err != nil {
		t.Fatalf("resume read: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("resume should be empty, got %#v", events)
	}
}

func TestUnixSocketProtocolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	coord := NewCoordinator(backend)
	socket := filepath.Join(dir, "state.sock")
	srv, err := ListenUnix(coord, socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = srv.Close() }()

	client := NewClient(socket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ping, err := client.Call(ctx, Request{ID: "1", Op: OpPing})
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !ping.OK {
		t.Fatalf("ping not ok: %#v", ping)
	}

	payload, _ := json.Marshal(TransitionPhasePayload{
		InvocationID:    "inv1_socket-0001",
		From:            cliproxyexecutor.AcceptanceNotSent,
		To:              cliproxyexecutor.AcceptancePreparedDurable,
		CreateIfMissing: true,
	})
	resp, err := client.Call(ctx, Request{ID: "2", Op: OpTransitionPhase, Payload: payload})
	if err != nil {
		t.Fatalf("transition: %v", err)
	}
	var rec InvocationRecord
	if err := json.Unmarshal(resp.Payload, &rec); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.Phase != cliproxyexecutor.AcceptancePreparedDurable {
		t.Fatalf("phase = %q", rec.Phase)
	}
}

func TestReservationCapacityGuard(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db"), MaxReservedBytes: 100})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	coord := NewCoordinator(backend)
	ctx := context.Background()

	rec, err := coord.ReserveCapacity(ctx, ReservePayload{InvocationID: "inv1_cap-1", Bytes: 80, Kind: "envelope"})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := coord.ReserveCapacity(ctx, ReservePayload{InvocationID: "inv1_cap-2", Bytes: 30, Kind: "envelope"}); err == nil {
		t.Fatal("expected capacity error")
	}
	if err := coord.ReleaseReservation(ctx, rec.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := coord.ReserveCapacity(ctx, ReservePayload{InvocationID: "inv1_cap-2", Bytes: 30, Kind: "envelope"}); err != nil {
		t.Fatalf("reserve after release: %v", err)
	}
}

func TestPutInvocationDoesNotSmashPhase(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	ctx := context.Background()

	if err := backend.PutInvocation(ctx, &InvocationRecord{
		InvocationID:         "inv1_put-0001",
		TenantID:             "tenant-a",
		ConversationID:       "conv-a",
		ClientIdempotencyKey: "idem-1",
		CanonicalRequestHash: "hash-1",
		Phase:                cliproxyexecutor.AcceptanceNotSent,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := backend.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID: "inv1_put-0001",
		From:         cliproxyexecutor.AcceptanceNotSent,
		To:           cliproxyexecutor.AcceptancePreparedDurable,
	}); err != nil {
		t.Fatalf("transition: %v", err)
	}

	if err := backend.PutInvocation(ctx, &InvocationRecord{
		InvocationID:         "inv1_put-0001",
		TenantID:             "tenant-a",
		ConversationID:       "conv-a",
		ClientIdempotencyKey: "idem-1",
		CanonicalRequestHash: "hash-1",
		Phase:                cliproxyexecutor.AcceptanceCompleted, // must not overwrite
	}); err != nil {
		t.Fatalf("idempotent put: %v", err)
	}
	got, err := backend.GetInvocation(ctx, "inv1_put-0001")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Phase != cliproxyexecutor.AcceptancePreparedDurable {
		t.Fatalf("phase smashed to %q", got.Phase)
	}

	err = backend.PutInvocation(ctx, &InvocationRecord{
		InvocationID:         "inv1_put-0001",
		TenantID:             "tenant-b",
		ConversationID:       "conv-a",
		ClientIdempotencyKey: "idem-1",
		CanonicalRequestHash: "hash-1",
		Phase:                cliproxyexecutor.AcceptanceNotSent,
	})
	var phaseErr *PhaseTransitionError
	if !errors.As(err, &phaseErr) || phaseErr.Code != CodeConflict {
		t.Fatalf("expected identity conflict, got %v", err)
	}
}

func TestTransitionPhaseRejectsIdentityReuse(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	ctx := context.Background()

	if _, err := backend.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID:         "inv1_id-0001",
		To:                   cliproxyexecutor.AcceptanceNotSent,
		CreateIfMissing:      true,
		TenantID:             "tenant-a",
		ConversationID:       "conv-a",
		ClientIdempotencyKey: "idem-1",
		CanonicalRequestHash: "hash-1",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = backend.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID:         "inv1_id-0001",
		From:                 cliproxyexecutor.AcceptanceNotSent,
		To:                   cliproxyexecutor.AcceptancePreparedDurable,
		TenantID:             "tenant-a",
		ConversationID:       "conv-b",
		ClientIdempotencyKey: "idem-1",
		CanonicalRequestHash: "hash-1",
	})
	var phaseErr *PhaseTransitionError
	if !errors.As(err, &phaseErr) || phaseErr.Code != CodeConflict {
		t.Fatalf("expected identity conflict, got %v", err)
	}
}

func TestLeaseRenewRequiresFencingGeneration(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	ctx := context.Background()

	lease, err := backend.AcquireLease(ctx, LeasePayload{InstanceID: "inst-1", BinaryVersion: "v1", TTLMilliseconds: 5000})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := backend.RenewLease(ctx, LeasePayload{InstanceID: "inst-1", TTLMilliseconds: 5000}); err == nil {
		t.Fatal("expected renew without fencing to fail")
	}
	if _, err := backend.RenewLease(ctx, LeasePayload{InstanceID: "inst-1", FencingGen: lease.FencingGeneration + 1, TTLMilliseconds: 5000}); err == nil {
		t.Fatal("expected stale fencing renew to fail")
	}
	renewed, err := backend.RenewLease(ctx, LeasePayload{InstanceID: "inst-1", FencingGen: lease.FencingGeneration, TTLMilliseconds: 5000})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed.FencingGeneration != lease.FencingGeneration {
		t.Fatalf("renew bumped fencing %d -> %d", lease.FencingGeneration, renewed.FencingGeneration)
	}
	if _, err := backend.AcquireLease(ctx, LeasePayload{InstanceID: "inst-1", BinaryVersion: "v1", FencingGen: 0, TTLMilliseconds: 5000}); err == nil {
		t.Fatal("expected live reacquire without fencing to fail")
	}
}

func TestListenUnixKeepsLiveSocket(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	coord := NewCoordinator(backend)
	socket := filepath.Join(dir, "state.sock")
	srv, err := ListenUnix(coord, socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = srv.Close() }()

	_, err = ListenUnix(coord, socket)
	if err == nil {
		t.Fatal("expected live socket listen to fail")
	}
	if !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("live socket removed: %v", err)
	}
}

func TestListenUnixRemovesStaleSocket(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	coord := NewCoordinator(backend)
	socket := filepath.Join(dir, "state.sock")
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale socket: %v", err)
	}
	srv, err := ListenUnix(coord, socket)
	if err != nil {
		t.Fatalf("listen over stale socket: %v", err)
	}
	defer func() { _ = srv.Close() }()
}

func TestUnixRequestLineSizeBound(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	coord := NewCoordinator(backend)
	socket := filepath.Join(dir, "state.sock")
	srv, err := ListenUnix(coord, socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = srv.Close() }()

	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(strings.Repeat("a", maxUnixRequestLineBytes+8) + "\n")); err != nil {
		t.Fatalf("write oversized: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var resp Response
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		t.Fatalf("decode: %v (%q)", err, string(buf[:n]))
	}
	if resp.OK || resp.Code != CodeInvalidRequest {
		t.Fatalf("expected invalid_request, got %#v", resp)
	}
}

func TestAppendJournalRejectsBadDigest(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() { _ = backend.Close() }()
	ctx := context.Background()
	if _, err := backend.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID:    "inv1_digest-0001",
		To:              cliproxyexecutor.AcceptanceNotSent,
		CreateIfMissing: true,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	_, err = backend.AppendJournal(ctx, JournalEvent{
		InvocationID:  "inv1_digest-0001",
		Type:          "text",
		Payload:       json.RawMessage(`{"text":"hello"}`),
		PayloadDigest: "not-the-digest",
	})
	var phaseErr *PhaseTransitionError
	if !errors.As(err, &phaseErr) || phaseErr.Code != CodeInvalidRequest {
		t.Fatalf("expected digest invalid_request, got %v", err)
	}
}
