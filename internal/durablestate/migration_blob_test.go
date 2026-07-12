package durablestate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAdvanceEpochRequiresDrainAndRefusesOlderWriters(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db"), StateEpoch: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	coord := NewCoordinator(backend)
	ctx := context.Background()

	if _, err := coord.AdvanceEpoch(ctx, MigrationPlan{ToEpoch: 2}); err == nil {
		t.Fatal("epoch advance without drain must fail")
	}
	dry, err := coord.AdvanceEpoch(ctx, MigrationPlan{ToEpoch: 2, DryRun: true})
	if err != nil || dry.StateEpoch != 2 {
		t.Fatalf("dry-run: %#v %v", dry, err)
	}
	coord.Admission().BeginDrain()
	after, err := coord.AdvanceEpoch(ctx, MigrationPlan{FromEpoch: 1, ToEpoch: 2})
	if err != nil {
		t.Fatal(err)
	}
	if after.StateEpoch != 2 {
		t.Fatalf("epoch=%d", after.StateEpoch)
	}
	if err := backend.RequireWritableEpoch(1); err == nil {
		t.Fatal("older writer epoch must be refused")
	}
	if _, err := backend.AcquireLease(ctx, LeasePayload{InstanceID: "old", BinaryVersion: "v0", StateEpoch: 1, TTLMilliseconds: 5000}); err == nil {
		t.Fatal("acquire with older epoch must fail")
	}
	if _, err := backend.AcquireLease(ctx, LeasePayload{InstanceID: "new", BinaryVersion: "v2", StateEpoch: 2, TTLMilliseconds: 5000}); err != nil {
		t.Fatal(err)
	}
}

func TestImportCASReceiptsDualRead(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	coord := NewCoordinator(backend)
	ctx := context.Background()

	casDir := filepath.Join(dir, "receipts")
	if err := os.MkdirAll(casDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := map[string]any{
		"version":                5,
		"status":                 "delivering",
		"invocationId":           "inv1_cas_import",
		"requestHash":            "abc",
		"deliveryIdempotencyKey": "idem-1",
		"sessionId":              "conv-1",
		"agentId":                "agent-1",
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(casDir, "r1.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	scanned, err := ScanLegacyCASReceiptDir(casDir)
	if err != nil || len(scanned) != 1 {
		t.Fatalf("scan=%v err=%v", scanned, err)
	}
	if scanned[0].Phase != cliproxyexecutor.AcceptanceMaybeAccepted {
		t.Fatalf("phase=%s", scanned[0].Phase)
	}
	imported, skipped, err := coord.ImportCASReceipts(ctx, scanned)
	if err != nil || imported != 1 || skipped != 0 {
		t.Fatalf("imported=%d skipped=%d err=%v", imported, skipped, err)
	}
	imported, skipped, err = coord.ImportCASReceipts(ctx, scanned)
	if err != nil || imported != 0 || skipped != 1 {
		t.Fatalf("second import imported=%d skipped=%d err=%v", imported, skipped, err)
	}
	got, err := coord.GetInvocation(ctx, "inv1_cas_import")
	if err != nil || got.Phase != cliproxyexecutor.AcceptanceMaybeAccepted {
		t.Fatalf("got=%v err=%v", got, err)
	}
}

func TestBlobStoreRoundTripDedupeAndPersistEnvelope(t *testing.T) {
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x11}, 32)
	provider := &EnvMasterKeyProvider{KeyID: "blob-v1", Key: key}
	store, err := NewBlobStore(filepath.Join(dir, "blobs"), provider)
	if err != nil {
		t.Fatal(err)
	}
	aad := BuildAAD("t", "c", "inv1_blob", 1)
	ref1, dig1, err := store.Put([]byte("envelope-bytes"), aad)
	if err != nil {
		t.Fatal(err)
	}
	ref2, dig2, err := store.Put([]byte("envelope-bytes"), aad)
	if err != nil {
		t.Fatal(err)
	}
	if ref1 != ref2 || dig1 != dig2 {
		t.Fatalf("dedupe mismatch %s/%s", ref1, ref2)
	}
	plain, err := store.Get(ref1, aad)
	if err != nil || string(plain) != "envelope-bytes" {
		t.Fatalf("get=%q err=%v", plain, err)
	}
	if err := store.Verify(ref1); err != nil {
		t.Fatal(err)
	}

	backend, err := OpenSQLite(SQLiteConfig{Path: filepath.Join(dir, "state.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	coord := NewCoordinator(backend).WithBlobStore(store)
	coord.flags.EncryptedState = true
	digest, ref, err := coord.PersistEnvelope("t", "c", "inv1_blob", []byte("envelope-bytes"))
	if err != nil || digest == "" || ref == "" {
		t.Fatalf("persist digest=%q ref=%q err=%v", digest, ref, err)
	}
}

func TestFeatureFlagsAndMetricsSnapshot(t *testing.T) {
	t.Setenv("CLIPROXY_FLAG_ENCRYPTED_STATE", "true")
	t.Setenv("CLIPROXY_FLAG_DURABLE_LIVE_STREAMING", "1")
	flags := LoadFeatureFlagsFromEnv()
	if !flags.EncryptedState || !flags.DurableLiveStreaming {
		t.Fatalf("flags=%+v", flags)
	}
	var m Metrics
	m.PhaseTransitions.Add(2)
	m.JournalAppends.Add(3)
	snap := m.Snapshot()
	if snap["phase_transitions"] != 2 || snap["journal_appends"] != 3 {
		t.Fatalf("snap=%v", snap)
	}
}
