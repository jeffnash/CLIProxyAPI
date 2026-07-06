package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveAuthFilesAbsentFromBucket(t *testing.T) {
	authDir := t.TempDir()
	kept := filepath.Join(authDir, "provider", "kept.json")
	stale := filepath.Join(authDir, "provider", "stale.json")
	if err := os.MkdirAll(filepath.Dir(kept), 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(kept, []byte(`{"id":"kept"}`), 0o600); err != nil {
		t.Fatalf("write kept auth: %v", err)
	}
	if err := os.WriteFile(stale, []byte(`{"id":"stale"}`), 0o600); err != nil {
		t.Fatalf("write stale auth: %v", err)
	}

	if err := removeAuthFilesAbsentFromBucket(authDir, map[string]struct{}{
		filepath.Join("provider", "kept.json"): {},
	}); err != nil {
		t.Fatalf("removeAuthFilesAbsentFromBucket() error = %v", err)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("kept auth stat error = %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale auth stat error = %v, want not exist", err)
	}
}
