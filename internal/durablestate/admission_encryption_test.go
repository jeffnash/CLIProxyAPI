package durablestate

import (
	"bytes"
	"compress/gzip"
	"context"
	"path/filepath"
	"testing"
)

func TestNextGeometricBytes(t *testing.T) {
	got, err := NextGeometricBytes(0, 100, DefaultAdaptiveMemoryInitialBytes, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultAdaptiveMemoryInitialBytes {
		t.Fatalf("got %d want %d", got, DefaultAdaptiveMemoryInitialBytes)
	}
	got, err = NextGeometricBytes(DefaultAdaptiveMemoryInitialBytes, DefaultAdaptiveMemoryInitialBytes+1, DefaultAdaptiveMemoryInitialBytes, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultAdaptiveMemoryInitialBytes*2 {
		t.Fatalf("got %d", got)
	}
	if _, err := NextGeometricBytes(0, 65<<20, DefaultAdaptiveMemoryInitialBytes, 64<<20); err == nil {
		t.Fatal("expected hard-cap error")
	}
}

func TestAvailableDurableBytesWithholdsEmergencyReserve(t *testing.T) {
	availFresh := AvailableDurableBytes(0, 1000, 200, AdmissionPriorityFresh)
	if availFresh != 800 {
		t.Fatalf("fresh avail=%d", availFresh)
	}
	availRecovery := AvailableDurableBytes(0, 1000, 200, AdmissionPriorityRecovery)
	if availRecovery != 1000 {
		t.Fatalf("recovery avail=%d", availRecovery)
	}
	if AvailableDurableBytes(900, 1000, 200, AdmissionPriorityFresh) != 0 {
		t.Fatal("fresh should be blocked once emergency slice is the only remainder")
	}
	if AvailableDurableBytes(900, 1000, 200, AdmissionPriorityRecovery) != 100 {
		t.Fatal("recovery should still use emergency slice")
	}
}

func TestReservePriorityUsesEmergencyReserve(t *testing.T) {
	dir := t.TempDir()
	backend, err := OpenSQLite(SQLiteConfig{
		Path:                  filepath.Join(dir, "state.db"),
		MaxReservedBytes:      1000,
		EmergencyReserveBytes: 400,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	ctx := context.Background()

	if _, err := backend.Reserve(ctx, ReservePayload{
		InvocationID: "inv1_fresh",
		Bytes:        600,
		Priority:     AdmissionPriorityFresh,
		Kind:         "envelope",
	}); err != nil {
		t.Fatalf("fresh reserve within non-emergency budget: %v", err)
	}
	if _, err := backend.Reserve(ctx, ReservePayload{
		InvocationID: "inv1_fresh2",
		Bytes:        100,
		Priority:     AdmissionPriorityFresh,
		Kind:         "envelope",
	}); err == nil {
		t.Fatal("expected fresh admission to fail against emergency reserve")
	} else if _, ok := err.(*CapacityError); !ok {
		t.Fatalf("want CapacityError, got %T %v", err, err)
	}
	rec, err := backend.Reserve(ctx, ReservePayload{
		InvocationID: "inv1_recovery",
		Bytes:        200,
		Priority:     AdmissionPriorityRecovery,
		Kind:         "envelope",
	})
	if err != nil {
		t.Fatalf("recovery should use emergency slice: %v", err)
	}
	shrunk, err := backend.ResizeReservation(ctx, rec.ID, 50, AdmissionPriorityRecovery)
	if err != nil {
		t.Fatal(err)
	}
	if shrunk.Bytes != 50 {
		t.Fatalf("shrink bytes=%d", shrunk.Bytes)
	}
}

func TestEncryptEnvelopeRoundTripAndTamper(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	provider := &EnvMasterKeyProvider{KeyID: "test-v1", Key: key}
	aad := BuildAAD("tenant", "conv", "inv1_enc", 1)

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte("sensitive envelope")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	enc, err := EncryptEnvelope(provider, compressed.Bytes(), aad)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := DecryptEnvelope(provider, enc, aad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plain, compressed.Bytes()) {
		t.Fatal("plaintext mismatch")
	}
	enc.Blob[len(enc.Blob)-1] ^= 0xff
	if _, err := DecryptEnvelope(provider, enc, aad); err == nil {
		t.Fatal("expected tamper detection")
	}
}

func TestLoadEnvMasterKeyProviderRequired(t *testing.T) {
	t.Setenv("CLIPROXY_STATE_MASTER_KEY", "")
	if _, err := LoadEnvMasterKeyProvider(true); err == nil {
		t.Fatal("expected missing-key failure")
	}
	t.Setenv("CLIPROXY_STATE_MASTER_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	p, err := LoadEnvMasterKeyProvider(true)
	if err != nil || p == nil {
		t.Fatalf("load: %v %#v", err, p)
	}
}
