package config

import (
	"os"
	"testing"
)

func TestLoadConfigOptional_StreamingKeepAliveSeconds_SetsFromEnv(t *testing.T) {
	// Ensure env var is cleared after test
	old := os.Getenv("STREAMING_KEEPALIVE_SECONDS")
	t.Cleanup(func() {
		_ = os.Setenv("STREAMING_KEEPALIVE_SECONDS", old)
	})

	// Minimal config file content (no streaming config defined)
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte("port: 8317\n")
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Set env var to 30 seconds
	if err := os.Setenv("STREAMING_KEEPALIVE_SECONDS", "30"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if cfg.Streaming.KeepAliveSeconds != 30 {
		t.Fatalf("expected KeepAliveSeconds=30, got %d", cfg.Streaming.KeepAliveSeconds)
	}
}

func TestLoadConfigOptional_StreamingKeepAliveSeconds_EnvOverridesYAML(t *testing.T) {
	// Ensure env var is cleared after test
	old := os.Getenv("STREAMING_KEEPALIVE_SECONDS")
	t.Cleanup(func() {
		_ = os.Setenv("STREAMING_KEEPALIVE_SECONDS", old)
	})

	// Config file with streaming.keepalive-seconds set to 10
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte(`port: 8317
streaming:
  keepalive-seconds: 10
`)
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Env var should override YAML value
	if err := os.Setenv("STREAMING_KEEPALIVE_SECONDS", "45"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if cfg.Streaming.KeepAliveSeconds != 45 {
		t.Fatalf("expected KeepAliveSeconds=45 (env override), got %d", cfg.Streaming.KeepAliveSeconds)
	}
}

func TestLoadConfigOptional_StreamingKeepAliveSeconds_InvalidEnvIgnored(t *testing.T) {
	// Ensure env var is cleared after test
	old := os.Getenv("STREAMING_KEEPALIVE_SECONDS")
	t.Cleanup(func() {
		_ = os.Setenv("STREAMING_KEEPALIVE_SECONDS", old)
	})

	// Config file with streaming.keepalive-seconds set to 10
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte(`port: 8317
streaming:
  keepalive-seconds: 10
`)
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Invalid env var should be ignored, YAML value preserved
	if err := os.Setenv("STREAMING_KEEPALIVE_SECONDS", "not-a-number"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if cfg.Streaming.KeepAliveSeconds != 10 {
		t.Fatalf("expected KeepAliveSeconds=10 (YAML preserved), got %d", cfg.Streaming.KeepAliveSeconds)
	}
}

func TestLoadConfigOptional_StreamingKeepAliveSeconds_ZeroEnvIgnored(t *testing.T) {
	// Ensure env var is cleared after test
	old := os.Getenv("STREAMING_KEEPALIVE_SECONDS")
	t.Cleanup(func() {
		_ = os.Setenv("STREAMING_KEEPALIVE_SECONDS", old)
	})

	// Config file with streaming.keepalive-seconds set to 10
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte(`port: 8317
streaming:
  keepalive-seconds: 10
`)
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Zero env var should be ignored (keeps YAML value)
	if err := os.Setenv("STREAMING_KEEPALIVE_SECONDS", "0"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if cfg.Streaming.KeepAliveSeconds != 10 {
		t.Fatalf("expected KeepAliveSeconds=10 (YAML preserved when env is 0), got %d", cfg.Streaming.KeepAliveSeconds)
	}
}

func TestLoadConfigOptional_StreamingKeepAliveSeconds_NegativeEnvIgnored(t *testing.T) {
	// Ensure env var is cleared after test
	old := os.Getenv("STREAMING_KEEPALIVE_SECONDS")
	t.Cleanup(func() {
		_ = os.Setenv("STREAMING_KEEPALIVE_SECONDS", old)
	})

	// Config file with streaming.keepalive-seconds set to 10
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte(`port: 8317
streaming:
  keepalive-seconds: 10
`)
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Negative env var should be ignored (keeps YAML value)
	if err := os.Setenv("STREAMING_KEEPALIVE_SECONDS", "-5"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if cfg.Streaming.KeepAliveSeconds != 10 {
		t.Fatalf("expected KeepAliveSeconds=10 (YAML preserved when env is negative), got %d", cfg.Streaming.KeepAliveSeconds)
	}
}
