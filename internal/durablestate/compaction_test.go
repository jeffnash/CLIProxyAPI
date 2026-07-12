package durablestate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestDecideCompactionNeverAgePurgesMaybeAccepted(t *testing.T) {
	old := time.Now().UTC().Add(-365 * 24 * time.Hour)
	rec := &InvocationRecord{
		InvocationID:   "inv1_maybe",
		Phase:          cliproxyexecutor.AcceptanceMaybeAccepted,
		PhaseUpdatedAt: old,
		UpdatedAt:      old,
	}
	d := DecideCompaction(rec, CompactionPolicy{RejectedRetention: time.Hour, Now: func() time.Time { return time.Now().UTC() }}, true)
	if d.Action != CompactionRetain {
		t.Fatalf("action=%s reason=%s", d.Action, d.Reason)
	}
}

func TestDecideCompactionPurgesRejectedAfterRetention(t *testing.T) {
	now := time.Now().UTC()
	rec := &InvocationRecord{
		InvocationID:   "inv1_rej",
		Phase:          cliproxyexecutor.AcceptanceRejectedBeforeSend,
		PhaseUpdatedAt: now.Add(-48 * time.Hour),
	}
	d := DecideCompaction(rec, CompactionPolicy{RejectedRetention: 24 * time.Hour, Now: func() time.Time { return now }}, false)
	if d.Action != CompactionPurgeSafe {
		t.Fatalf("action=%s", d.Action)
	}
}

func TestDecideCompactionPreparedRequiresNegativeProof(t *testing.T) {
	rec := &InvocationRecord{InvocationID: "inv1_prep", Phase: cliproxyexecutor.AcceptancePreparedDurable}
	if DecideCompaction(rec, CompactionPolicy{}, false).Action != CompactionRetain {
		t.Fatal("prepared without proof must retain")
	}
	if DecideCompaction(rec, CompactionPolicy{}, true).Action != CompactionPurgeSafe {
		t.Fatal("prepared with negative send proof may purge")
	}
}

func TestDecideCompactionCompletedCompacts(t *testing.T) {
	rec := &InvocationRecord{InvocationID: "inv1_done", Phase: cliproxyexecutor.AcceptanceCompleted}
	if DecideCompaction(rec, CompactionPolicy{}, false).Action != CompactionCompactCompleted {
		t.Fatal("completed should compact")
	}
}

func TestListUnresolvedAndApplyCompaction(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	coord := NewCoordinator(backend)
	ctx := context.Background()

	now := time.Now().UTC()
	mustPut := func(rec *InvocationRecord) {
		t.Helper()
		if err := backend.PutInvocation(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	mustPut(&InvocationRecord{
		InvocationID:    "inv1_maybe",
		Phase:           cliproxyexecutor.AcceptanceMaybeAccepted,
		EnvelopeDigest:  "digest-maybe",
		EnvelopeBlobRef: "blob-maybe",
		UpdatedAt:       now.Add(-365 * 24 * time.Hour),
		PhaseUpdatedAt:  now.Add(-365 * 24 * time.Hour),
	})
	mustPut(&InvocationRecord{
		InvocationID:    "inv1_done",
		Phase:           cliproxyexecutor.AcceptanceCompleted,
		EnvelopeDigest:  "digest-done",
		EnvelopeBlobRef: "blob-done",
	})
	mustPut(&InvocationRecord{
		InvocationID:   "inv1_rej",
		Phase:          cliproxyexecutor.AcceptanceRejectedBeforeSend,
		PhaseUpdatedAt: now.Add(-48 * time.Hour),
		UpdatedAt:      now.Add(-48 * time.Hour),
	})

	unresolved, err := coord.ListUnresolved(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 1 || unresolved[0].InvocationID != "inv1_maybe" {
		t.Fatalf("unresolved=%v", unresolved)
	}

	// Age alone must not purge maybe-accepted.
	d, err := coord.ApplyCompaction(ctx, "inv1_maybe", CompactionPolicy{RejectedRetention: time.Hour}, true)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != CompactionRetain {
		t.Fatalf("maybe accepted action=%s", d.Action)
	}
	if got, _ := backend.GetInvocation(ctx, "inv1_maybe"); got == nil {
		t.Fatal("maybe accepted purged")
	}

	d, err = coord.ApplyCompaction(ctx, "inv1_done", CompactionPolicy{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != CompactionCompactCompleted {
		t.Fatalf("completed action=%s", d.Action)
	}
	done, err := backend.GetInvocation(ctx, "inv1_done")
	if err != nil || done == nil {
		t.Fatal(err)
	}
	if done.EnvelopeBlobRef != "" || done.EnvelopeDigest != "digest-done" {
		t.Fatalf("compacted envelope digest=%q blob=%q", done.EnvelopeDigest, done.EnvelopeBlobRef)
	}

	d, err = coord.ApplyCompaction(ctx, "inv1_rej", CompactionPolicy{RejectedRetention: 24 * time.Hour}, false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != CompactionPurgeSafe {
		t.Fatalf("rejected action=%s", d.Action)
	}
	if got, _ := backend.GetInvocation(ctx, "inv1_rej"); got != nil {
		t.Fatal("rejected should be purged")
	}
}

func TestAdmissionDrainBlocksFreshOnly(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db"), MaxReservedBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	coord := NewCoordinator(backend)
	ctx := context.Background()
	coord.Admission().BeginDrain()
	if _, err := coord.ReserveCapacity(ctx, ReservePayload{InvocationID: "inv1_fresh", Bytes: 10, Priority: AdmissionPriorityFresh}); err == nil {
		t.Fatal("fresh admission should fail while draining")
	}
	if _, err := coord.ReserveCapacity(ctx, ReservePayload{InvocationID: "inv1_rec", Bytes: 10, Priority: AdmissionPriorityRecovery}); err != nil {
		t.Fatalf("recovery admission must continue: %v", err)
	}
	cp, err := coord.CheckpointUnresolved(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	_ = cp
	coord.Admission().EndDrain()
	if _, err := coord.ReserveCapacity(ctx, ReservePayload{InvocationID: "inv1_fresh2", Bytes: 10, Priority: AdmissionPriorityFresh}); err != nil {
		t.Fatalf("fresh after end drain: %v", err)
	}
}
