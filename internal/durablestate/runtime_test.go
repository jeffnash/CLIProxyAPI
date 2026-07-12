package durablestate

import (
	"bytes"
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestOpenRuntimeSQLiteLeaseAndSocket(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "state.sock")
	ctx := context.Background()
	rt, err := OpenRuntime(ctx, RuntimeConfig{
		Socket:        socket,
		SQLitePath:    filepath.Join(dir, "state.db"),
		InstanceID:    "runtime-test-1",
		BinaryVersion: "test",
		StateEpoch:    1,
		LeaseTTL:      3 * time.Second,
	})
	if err != nil {
		t.Fatalf("OpenRuntime: %v", err)
	}
	defer func() { _ = rt.Close(ctx) }()

	client := NewClient(socket)
	if _, err := client.Call(ctx, Request{Op: OpPing}); err != nil {
		t.Fatalf("ping: %v", err)
	}
	leaseResp, err := client.Call(ctx, Request{Op: OpCurrentLease})
	if err != nil {
		t.Fatalf("current lease: %v", err)
	}
	if len(leaseResp.Payload) == 0 {
		t.Fatal("empty lease payload")
	}
	snap, err := rt.MigrationReady(ctx)
	if err != nil || snap == nil || snap.StateEpoch != 1 {
		t.Fatalf("migration ready: %#v %v", snap, err)
	}
}

func TestBlobStoreDualDecryptAndReencryptAll(t *testing.T) {
	dir := t.TempDir()
	oldKey := bytes.Repeat([]byte{0x31}, 32)
	newKey := bytes.Repeat([]byte{0x32}, 32)
	oldProvider := &EnvMasterKeyProvider{KeyID: "env-v1", Key: oldKey}
	store, err := NewBlobStore(filepath.Join(dir, "blobs"), oldProvider)
	if err != nil {
		t.Fatal(err)
	}
	aad := BuildAAD("t", "c", "inv", 1)
	ref, _, err := store.Put([]byte("envelope-v1"), aad)
	if err != nil {
		t.Fatal(err)
	}

	rotating := &RotatingMasterKeyProvider{
		Current:  &EnvMasterKeyProvider{KeyID: "env-v1", Key: newKey},
		Previous: &EnvMasterKeyProvider{KeyID: "env-v0", Key: oldKey},
	}
	store.provider = rotating
	got, err := store.Get(ref, aad)
	if err != nil {
		t.Fatalf("dual get: %v", err)
	}
	if string(got) != "envelope-v1" {
		t.Fatalf("got %q", got)
	}
	n, err := store.ReencryptAll(rotating, aad)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if n < 1 {
		t.Fatalf("expected rewrite, got %d", n)
	}
	// Previous-only must fail after reencrypt.
	store.provider = rotating.Previous
	if _, err := store.Get(ref, aad); err == nil {
		t.Fatal("expected previous-only get to fail after reencrypt")
	}
	store.provider = rotating.Current
	got, err = store.Get(ref, aad)
	if err != nil || string(got) != "envelope-v1" {
		t.Fatalf("current get after reencrypt: %q %v", got, err)
	}
}

func TestAdaptiveReservationLoadUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{
		Path:                  filepath.Join(dir, "load.db"),
		MaxReservedBytes:      8 << 20,
		EmergencyReserveBytes: 2 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backend.Close() }()
	coord := NewCoordinator(backend)
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			priority := AdmissionPriorityFresh
			if i%4 == 0 {
				priority = AdmissionPriorityRecovery
			}
			_, err := coord.ReserveCapacity(ctx, ReservePayload{
				InvocationID: "load-" + strconv.Itoa(i) + "-" + time.Now().Format("150405.000000"),
				TenantID:     "tenant-load",
				Bytes:        64 << 10,
				Priority:     priority,
				Kind:         "stream_tail",
			})
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	rejected := 0
	for err := range errCh {
		if _, ok := err.(*CapacityError); ok {
			rejected++
			continue
		}
		t.Fatalf("unexpected reserve error: %v", err)
	}
	used, err := backend.ReservationBytes(ctx, "tenant-load")
	if err != nil {
		t.Fatal(err)
	}
	if used <= 0 && rejected == 32 {
		t.Fatal("expected some reservations to succeed under load")
	}
}
