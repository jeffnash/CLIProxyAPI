package durablestate

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRotatingMasterKeyDualDecryptAndReencrypt(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x11}, 32)
	newKey := bytes.Repeat([]byte{0x22}, 32)
	aad := BuildAAD("tenant", "conv", "inv-rot", 1)

	oldProvider := &EnvMasterKeyProvider{KeyID: "env-v1", Key: oldKey}
	enc, err := EncryptEnvelope(oldProvider, []byte(`{"phase":"maybe_accepted"}`), aad)
	if err != nil {
		t.Fatalf("encrypt with old key: %v", err)
	}

	rotating := &RotatingMasterKeyProvider{
		Current:  &EnvMasterKeyProvider{KeyID: "env-v1", Key: newKey},
		Previous: &EnvMasterKeyProvider{KeyID: "env-v0", Key: oldKey},
	}
	plain, err := DecryptEnvelopeDual(rotating, enc, aad)
	if err != nil {
		t.Fatalf("dual decrypt: %v", err)
	}
	if string(plain) != `{"phase":"maybe_accepted"}` {
		t.Fatalf("unexpected plaintext %q", plain)
	}

	reenc, err := ReencryptBlob(rotating, enc, aad)
	if err != nil {
		t.Fatalf("reencrypt: %v", err)
	}
	if reenc.KeyID != "env-v1" {
		t.Fatalf("reencrypt key id = %q", reenc.KeyID)
	}
	if _, err := DecryptEnvelope(rotating.Previous, reenc, aad); err == nil {
		t.Fatal("expected previous-only decrypt of reencrypted blob to fail")
	}
	got, err := DecryptEnvelope(rotating.Current, reenc, aad)
	if err != nil {
		t.Fatalf("current decrypt after reencrypt: %v", err)
	}
	if string(got) != `{"phase":"maybe_accepted"}` {
		t.Fatalf("reencrypt plaintext mismatch %q", got)
	}
}

func TestLoadRotatingMasterKeyProviderFromEnv(t *testing.T) {
	t.Setenv("CLIPROXY_STATE_MASTER_KEY", string(bytes.Repeat([]byte("N"), 32)))
	t.Setenv("CLIPROXY_STATE_MASTER_KEY_PREVIOUS", string(bytes.Repeat([]byte("O"), 32)))
	provider, err := LoadRotatingMasterKeyProvider(true)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if provider.Current == nil || provider.Previous == nil {
		t.Fatal("expected current and previous keys")
	}
	if provider.Previous.KeyID != "env-v0" {
		t.Fatalf("previous key id = %q", provider.Previous.KeyID)
	}
}

func TestRestartBackoffJitterAndReset(t *testing.T) {
	b := RestartBackoff{Base: time.Second, Max: 8 * time.Second, StableAfter: time.Minute}
	d1 := b.Next()
	if d1 < 500*time.Millisecond || d1 > time.Second {
		t.Fatalf("first delay out of range: %v", d1)
	}
	d2 := b.Next()
	if d2 < time.Second || d2 > 2*time.Second {
		t.Fatalf("second delay out of range: %v", d2)
	}
	for i := 0; i < 8; i++ {
		_ = b.Next()
	}
	dMax := b.Next()
	if dMax < 4*time.Second || dMax > 8*time.Second {
		t.Fatalf("capped delay out of range: %v", dMax)
	}
	if !b.ShouldReset(time.Minute) {
		t.Fatal("expected ShouldReset after stable window")
	}
	b.Reset()
	if b.Attempt != 0 {
		t.Fatalf("attempt after reset = %d", b.Attempt)
	}
}

func TestPostgresFencingSkipsWithoutDSN(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("CLIPROXY_TEST_POSTGRES_DSN"))
	if dsn == "" {
		t.Skip("CLIPROXY_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	backend, err := OpenPostgres(PostgresConfig{DSN: dsn, StateEpoch: 2})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer func() { _ = backend.Close() }()

	// Isolate from leftover leases in shared CI databases.
	_ = backend.ReleaseLease(ctx, "pg-fence-1")
	_ = backend.ReleaseLease(ctx, "pg-fence-2")

	lease, err := backend.AcquireLease(ctx, LeasePayload{
		InstanceID: "pg-fence-1", BinaryVersion: "test", StateEpoch: 2, TTLMilliseconds: 5000,
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := backend.AcquireLease(ctx, LeasePayload{
		InstanceID: "pg-fence-2", BinaryVersion: "test", StateEpoch: 2, TTLMilliseconds: 5000,
	}); err == nil {
		t.Fatal("expected other instance acquire to fail while lease live")
	}
	if _, err := backend.AcquireLease(ctx, LeasePayload{
		InstanceID: "pg-fence-1", BinaryVersion: "test", StateEpoch: 1, TTLMilliseconds: 5000,
		FencingGen: lease.FencingGeneration,
	}); err == nil {
		t.Fatal("expected older epoch acquire to fail")
	}
	if _, err := backend.RenewLease(ctx, LeasePayload{
		InstanceID: "pg-fence-1", FencingGen: lease.FencingGeneration + 1, TTLMilliseconds: 5000,
	}); err == nil {
		t.Fatal("expected stale fencing renew to fail")
	}
	if _, err := backend.RenewLease(ctx, LeasePayload{
		InstanceID: "pg-fence-1", FencingGen: lease.FencingGeneration, TTLMilliseconds: 5000,
	}); err != nil {
		t.Fatalf("renew with fencing: %v", err)
	}
	_ = backend.ReleaseLease(ctx, "pg-fence-1")
}
