package durablestate

import (
	"context"
	"path/filepath"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// Acceptance crash-matrix: each crash point must leave recovery-safe evidence.
func TestAcceptanceCrashMatrixPhases(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "crash.db"), MaxReservedBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	coord := NewCoordinator(backend)
	ctx := context.Background()

	type step struct {
		name string
		to   cliproxyexecutor.AcceptancePhase
	}
	steps := []step{
		{name: "after_prepared", to: cliproxyexecutor.AcceptancePreparedDurable},
		{name: "after_maybe_accepted", to: cliproxyexecutor.AcceptanceMaybeAccepted},
		{name: "after_accepted", to: cliproxyexecutor.AcceptanceAccepted},
		{name: "after_completed", to: cliproxyexecutor.AcceptanceCompleted},
	}

	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			id := "inv1_" + s.name
			if _, err := coord.TransitionPhase(ctx, TransitionPhasePayload{
				InvocationID:    id,
				To:              cliproxyexecutor.AcceptanceNotSent,
				CreateIfMissing: true,
				AuthID:          "auth-1",
				EnvelopeDigest:  "env-" + id,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := coord.ReserveCapacity(ctx, ReservePayload{
				InvocationID: id, Bytes: 32, Kind: "envelope", Priority: AdmissionPriorityFresh,
			}); err != nil {
				t.Fatal(err)
			}
			phase := cliproxyexecutor.AcceptanceNotSent
			for _, next := range []cliproxyexecutor.AcceptancePhase{
				cliproxyexecutor.AcceptancePreparedDurable,
				cliproxyexecutor.AcceptanceMaybeAccepted,
				cliproxyexecutor.AcceptanceAccepted,
				cliproxyexecutor.AcceptanceCompleted,
			} {
				if _, err := coord.TransitionPhase(ctx, TransitionPhasePayload{
					InvocationID:   id,
					From:           phase,
					To:             next,
					Evidence:       cliproxyexecutor.EvidencePreparedEnvelope,
					EnvelopeDigest: "env-" + id,
					AuthID:         "auth-1",
				}); err != nil {
					t.Fatal(err)
				}
				phase = next
				if next == s.to {
					break
				}
			}

			restarted := NewCoordinator(backend)
			got, err := restarted.GetInvocation(ctx, id)
			if err != nil || got == nil {
				t.Fatalf("missing after crash: %v", err)
			}
			if got.Phase != s.to {
				t.Fatalf("phase=%s want=%s", got.Phase, s.to)
			}
			switch got.Phase {
			case cliproxyexecutor.AcceptanceMaybeAccepted, cliproxyexecutor.AcceptanceAccepted:
				if got.AuthID != "auth-1" || got.EnvelopeDigest == "" {
					t.Fatalf("unresolved evidence incomplete: %+v", got)
				}
				d := DecideCompaction(got, CompactionPolicy{}, true)
				if d.Action != CompactionRetain {
					t.Fatalf("must retain unresolved: %s", d.Action)
				}
			case cliproxyexecutor.AcceptancePreparedDurable:
				d := DecideCompaction(got, CompactionPolicy{}, true)
				if d.Action != CompactionPurgeSafe {
					t.Fatalf("prepared with negative send proof should purge-safe: %s", d.Action)
				}
			case cliproxyexecutor.AcceptanceCompleted:
				if _, err := restarted.AppendJournal(ctx, JournalEvent{
					InvocationID:    id,
					Type:            "terminal",
					AcceptancePhase: cliproxyexecutor.AcceptanceCompleted,
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestJournalBeforeExposeCrashRetainsSequence(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "journal.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	coord := NewCoordinator(backend)
	ctx := context.Background()
	id := "inv1_stream"
	if _, err := coord.TransitionPhase(ctx, TransitionPhasePayload{
		InvocationID: id, To: cliproxyexecutor.AcceptanceNotSent, CreateIfMissing: true,
	}); err != nil {
		t.Fatal(err)
	}
	for _, next := range []cliproxyexecutor.AcceptancePhase{
		cliproxyexecutor.AcceptancePreparedDurable,
		cliproxyexecutor.AcceptanceMaybeAccepted,
		cliproxyexecutor.AcceptanceAccepted,
	} {
		cur, err := coord.GetInvocation(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := coord.TransitionPhase(ctx, TransitionPhasePayload{
			InvocationID: id, From: cur.Phase, To: next,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := coord.AppendJournal(ctx, JournalEvent{
		InvocationID:    id,
		Type:            "text",
		AcceptancePhase: cliproxyexecutor.AcceptanceAccepted,
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewCoordinator(backend)
	events, err := restarted.ReadJournal(ctx, id, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Sequence != ev.Sequence {
		t.Fatalf("journal replay=%v", events)
	}
}
