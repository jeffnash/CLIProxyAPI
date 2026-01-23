package config

import (
	"os"
	"testing"
)

func TestLoadConfigOptional_StreamingDisableProxyBuffering_SetsFromEnv(t *testing.T) {
	old := os.Getenv("STREAMING_DISABLE_PROXY_BUFFERING")
	t.Cleanup(func() {
		_ = os.Setenv("STREAMING_DISABLE_PROXY_BUFFERING", old)
	})

	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	if err := os.Setenv("STREAMING_DISABLE_PROXY_BUFFERING", "true"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if !cfg.Streaming.DisableProxyBuffering {
		t.Fatal("expected DisableProxyBuffering=true")
	}
}

func TestLoadConfigOptional_StreamingDisableProxyBuffering_EnvOverridesYAML(t *testing.T) {
	old := os.Getenv("STREAMING_DISABLE_PROXY_BUFFERING")
	t.Cleanup(func() {
		_ = os.Setenv("STREAMING_DISABLE_PROXY_BUFFERING", old)
	})

	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte(`port: 8317
streaming:
  disable-proxy-buffering: true
`)
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	if err := os.Setenv("STREAMING_DISABLE_PROXY_BUFFERING", "0"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if cfg.Streaming.DisableProxyBuffering {
		t.Fatal("expected DisableProxyBuffering=false (env override)")
	}
}

func TestLoadConfigOptional_StreamingDisableProxyBuffering_CaseInsensitive(t *testing.T) {
	old := os.Getenv("STREAMING_DISABLE_PROXY_BUFFERING")
	t.Cleanup(func() {
		_ = os.Setenv("STREAMING_DISABLE_PROXY_BUFFERING", old)
	})

	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	if err := os.WriteFile(path, []byte("port: 8317\n"), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	if err := os.Setenv("STREAMING_DISABLE_PROXY_BUFFERING", "TrUe"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if !cfg.Streaming.DisableProxyBuffering {
		t.Fatal("expected DisableProxyBuffering=true")
	}
}
