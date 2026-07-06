package management

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteConfigUsesPrivateAtomicWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := WriteConfig(path, []byte("debug: true\n")); err != nil {
		t.Fatalf("WriteConfig() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "debug: true\n" {
		t.Fatalf("config contents = %q", data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config mode = %v, want 0600", got)
	}
}
