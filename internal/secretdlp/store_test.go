package secretdlp

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestFileMappingStoreEncryptsAndRestores(t *testing.T) {
	cfg := testFileStoreConfig(t)
	store, err := newFileMappingStore(cfg)
	if err != nil {
		t.Fatalf("newFileMappingStore(): %v", err)
	}

	placeholder := "__CPA_DLP_v1_client_nonce_001_random__"
	secret := []byte("sk-testdlpfixture0000000000000000000000000000")
	if err := store.Put(context.Background(), storedMapping{
		Placeholder: placeholder,
		Secret:      secret,
		SessionID:   "session-id",
		ClientID:    "client-id",
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Put(): %v", err)
	}

	path := store.pathForPlaceholder(placeholder)
	if strings.Contains(path, placeholder) {
		t.Fatalf("file path contains placeholder: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(): %v", err)
	}
	if bytes.Contains(data, secret) {
		t.Fatalf("file mapping contains plaintext secret: %q", data)
	}

	got, ok, err := store.Get(context.Background(), placeholder, "client-id", time.Now())
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if !ok {
		t.Fatal("Get() ok = false, want true")
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("Get() secret = %q, want %q", got, secret)
	}
	if _, ok, err := store.Get(context.Background(), placeholder, "other-client", time.Now()); err != nil || ok {
		t.Fatalf("Get() with other client = ok %v err %v, want no restore", ok, err)
	}
	if _, ok, err := store.Get(context.Background(), placeholder, "", time.Now()); err != nil || ok {
		t.Fatalf("Get() without client = ok %v err %v, want no restore", ok, err)
	}
}

func TestFileMappingStoreWrongMasterKeyCannotRestore(t *testing.T) {
	cfg := testFileStoreConfig(t)
	store, err := newFileMappingStore(cfg)
	if err != nil {
		t.Fatalf("newFileMappingStore(): %v", err)
	}

	placeholder := "__CPA_DLP_v1_client_nonce_001_random__"
	if err := store.Put(context.Background(), storedMapping{
		Placeholder: placeholder,
		Secret:      []byte("sk-testdlpfixture0000000000000000000000000000"),
		ExpiresAt:   time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("Put(): %v", err)
	}

	cfg.MasterKey = []byte("different-master-key-with-enough-bytes")
	wrongKeyStore, err := newFileMappingStore(cfg)
	if err != nil {
		t.Fatalf("newFileMappingStore(wrong key): %v", err)
	}
	got, ok, err := wrongKeyStore.Get(context.Background(), placeholder, "", time.Now())
	if err == nil && ok && bytes.Equal(got, []byte("sk-testdlpfixture0000000000000000000000000000")) {
		t.Fatal("Get() restored secret with wrong master key")
	}
}

func TestFileMappingStoreExpiresAndDeletes(t *testing.T) {
	cfg := testFileStoreConfig(t)
	store, err := newFileMappingStore(cfg)
	if err != nil {
		t.Fatalf("newFileMappingStore(): %v", err)
	}

	placeholder := "__CPA_DLP_v1_client_nonce_001_random__"
	if err := store.Put(context.Background(), storedMapping{
		Placeholder: placeholder,
		Secret:      []byte("sk-testdlpfixture0000000000000000000000000000"),
		ExpiresAt:   time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("Put(): %v", err)
	}
	path := store.pathForPlaceholder(placeholder)

	_, ok, err := store.Get(context.Background(), placeholder, "", time.Now())
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if ok {
		t.Fatal("Get() ok = true, want false for expired mapping")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expired mapping file still exists, stat err=%v", err)
	}
}

func TestFileMappingStoreRequiresStableMasterKey(t *testing.T) {
	_, err := newFileMappingStore(Config{
		Store:              storeFile,
		FileDir:            t.TempDir(),
		MasterKey:          []byte("boot-generated-master-key"),
		MasterKeyGenerated: true,
	})
	if err == nil {
		t.Fatal("newFileMappingStore() err = nil, want stable master key error")
	}
}

func TestServiceRestoresFromFileStoreAfterMemoryMappingCleared(t *testing.T) {
	svc, err := New(Config{
		Enabled:        true,
		Mode:           ModeRestore,
		MasterKey:      []byte("stable-master-key-for-file-store"),
		TTL:            time.Hour,
		MaxFindings:    10,
		MinValueLength: 12,
		Scanner:        "builtin",
		Store:          storeFile,
		FileDir:        t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}()

	session := NewSession([]byte("stable-master-key-for-file-store"), "client-key", time.Hour, ModeRestore)
	body := []byte(`{"message":"use sk-testdlpfixture0000000000000000000000000000"}`)
	redacted, mappings := session.RedactRawWithMappings(body, []Finding{{
		Secret: "sk-testdlpfixture0000000000000000000000000000",
		RuleID: "test",
		Source: "test",
	}})
	if err := svc.persistMappings(context.Background(), session, mappings); err != nil {
		t.Fatalf("persistMappings(): %v", err)
	}

	restoreSession := NewSession([]byte("stable-master-key-for-file-store"), "client-key", time.Hour, ModeRestore)
	restored := svc.RestoreResponse(WithSession(context.Background(), restoreSession), redacted)
	if string(restored) != string(body) {
		t.Fatalf("RestoreResponse() = %q, want %q", restored, body)
	}
}

func TestServiceStoreFailureHonorsFailClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, tc := range []struct {
		name       string
		failClosed bool
		wantErr    bool
	}{
		{name: "fail-open", failClosed: false, wantErr: false},
		{name: "fail-closed", failClosed: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := New(Config{
				Enabled:         true,
				Mode:            ModeRestore,
				MasterKey:       []byte("stable-master-key-for-file-store"),
				TTL:             time.Hour,
				MaxFindings:     10,
				MinValueLength:  12,
				Scanner:         "builtin",
				Store:           storeMemory,
				StoreFailClosed: tc.failClosed,
			})
			if err != nil {
				t.Fatalf("New(): %v", err)
			}
			defer func() {
				if err := svc.Close(); err != nil {
					t.Fatalf("Close(): %v", err)
				}
			}()
			svc.store = failingMappingStore{}

			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			redacted, session, err := svc.RedactGinPayload(c, []byte(`{"messages":[{"role":"user","content":"sk-testdlpfixture0000000000000000000000000000"}]}`))
			if tc.wantErr {
				if err == nil {
					t.Fatal("RedactGinPayload() err = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("RedactGinPayload(): %v", err)
			}
			if session == nil {
				t.Fatal("RedactGinPayload() session = nil, want session")
			}
			if bytes.Contains(redacted, []byte("sk-testdlpfixture0000000000000000000000000000")) {
				t.Fatalf("redacted body contains raw secret: %q", redacted)
			}
		})
	}
}

func TestServiceDrainWaitsForActiveRequest(t *testing.T) {
	svc := newTestServiceForDrain(t)
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}()

	end := svc.BeginRequest()
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		drained <- svc.Drain(ctx)
	}()

	if got := svc.ActiveRequests(); got != 1 {
		t.Fatalf("ActiveRequests() = %d, want 1", got)
	}
	end()

	if err := <-drained; err != nil {
		t.Fatalf("Drain(): %v", err)
	}
}

func TestServiceDrainTimesOutWithActiveRequest(t *testing.T) {
	svc := newTestServiceForDrain(t)
	defer func() {
		if err := svc.Close(); err != nil {
			t.Fatalf("Close(): %v", err)
		}
	}()

	end := svc.BeginRequest()
	defer end()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	if err := svc.Drain(ctx); err == nil {
		t.Fatal("Drain() err = nil, want timeout")
	}
}

func testFileStoreConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		Store:     storeFile,
		FileDir:   t.TempDir(),
		MasterKey: []byte("stable-master-key-for-file-store"),
	}
}

func newTestServiceForDrain(t *testing.T) *Service {
	t.Helper()
	svc, err := New(Config{
		Enabled:        true,
		Mode:           ModeRestore,
		MasterKey:      []byte("stable-master-key-for-file-store"),
		TTL:            time.Hour,
		MaxFindings:    10,
		MinValueLength: 12,
		Scanner:        "builtin",
		Store:          storeMemory,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return svc
}

type failingMappingStore struct{}

func (f failingMappingStore) Put(ctx context.Context, mapping storedMapping) error {
	return errors.New("store unavailable")
}

func (f failingMappingStore) Get(ctx context.Context, placeholder string, clientID string, now time.Time) ([]byte, bool, error) {
	return nil, false, errors.New("store unavailable")
}

func (f failingMappingStore) CleanupExpired(ctx context.Context, now time.Time) error {
	return nil
}

func (f failingMappingStore) Close() error {
	return nil
}
