package config

import (
	"os"
	"testing"
)

func TestLoadConfigOptional_PassthruModelsJSON_MergesRoutes(t *testing.T) {
	// Ensure env var is cleared after test
	old := os.Getenv("PASSTHRU_MODELS_JSON")
	t.Cleanup(func() {
		_ = os.Setenv("PASSTHRU_MODELS_JSON", old)
	})

	// Minimal config file content (no passthru routes defined in file)
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte("port: 8317\n")
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Provide routes via env
	jsonValue := `[
  {
    "model": "glm-4.7",
    "model-routing-name": "zai-glm-4.7",
    "protocol": "claude",
    "base-url": "https://api.z.ai/api/anthropic",
    "api-key": "za-123",
    "upstream-model": "glm-4.7",
    "context-window": 128000,
    "max-tokens": 32000,
    "headers": {"X-Test": "1"}
  }
]`
	if err := os.Setenv("PASSTHRU_MODELS_JSON", jsonValue); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if len(cfg.Passthru) != 1 {
		t.Fatalf("expected 1 passthru route, got %d", len(cfg.Passthru))
	}
	r := cfg.Passthru[0]
	if r.Model != "glm-4.7" {
		t.Fatalf("expected model glm-4.7, got %q", r.Model)
	}
	if r.ModelRoutingName != "zai-glm-4.7" {
		t.Fatalf("expected model-routing-name zai-glm-4.7, got %q", r.ModelRoutingName)
	}
	if r.Protocol != "claude" {
		t.Fatalf("expected protocol claude, got %q", r.Protocol)
	}
	if r.BaseURL != "https://api.z.ai/api/anthropic" {
		t.Fatalf("expected base-url https://api.z.ai/api/anthropic, got %q", r.BaseURL)
	}
	if r.APIKey != "za-123" {
		t.Fatalf("expected api-key za-123, got %q", r.APIKey)
	}
	if r.UpstreamModel != "glm-4.7" {
		t.Fatalf("expected upstream-model glm-4.7, got %q", r.UpstreamModel)
	}
	if r.ContextWindow != 128000 {
		t.Fatalf("expected context-window 128000, got %d", r.ContextWindow)
	}
	if r.MaxTokens != 32000 {
		t.Fatalf("expected max-tokens 32000, got %d", r.MaxTokens)
	}
	if r.Headers == nil || r.Headers["X-Test"] != "1" {
		t.Fatalf("expected headers to include X-Test: 1, got %#v", r.Headers)
	}
}

func TestLoadConfigOptional_PassthruModelsJSON_InvalidJSON_OptionalConfigDoesNotError(t *testing.T) {
	old := os.Getenv("PASSTHRU_MODELS_JSON")
	t.Cleanup(func() {
		_ = os.Setenv("PASSTHRU_MODELS_JSON", old)
	})

	// Config is optional=true, so invalid env JSON should not error
	if err := os.Setenv("PASSTHRU_MODELS_JSON", "not-json"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional("/does/not/exist.yaml", true)
	if err != nil {
		t.Fatalf("expected no error for optional config, got %v", err)
	}
	_ = cfg
}

func TestLoadConfigOptional_PassthruModelsJSON_InvalidJSON_NonOptionalErrors(t *testing.T) {
	old := os.Getenv("PASSTHRU_MODELS_JSON")
	t.Cleanup(func() {
		_ = os.Setenv("PASSTHRU_MODELS_JSON", old)
	})

	// Create minimal config file
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte("port: 8317\n")
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Non-optional config: invalid JSON should error
	if err := os.Setenv("PASSTHRU_MODELS_JSON", "not-json"); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	_, err := LoadConfigOptional(path, false)
	if err == nil {
		t.Fatal("expected error for invalid PASSTHRU_MODELS_JSON")
	}
}

func TestLoadConfigOptional_PassthruModelsJSON_ApiKeysArray_MergesRoutes(t *testing.T) {
	// Ensure env var is cleared after test
	old := os.Getenv("PASSTHRU_MODELS_JSON")
	t.Cleanup(func() {
		_ = os.Setenv("PASSTHRU_MODELS_JSON", old)
	})

	// Minimal config file content (no passthru routes defined in file)
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte("port: 8317\n")
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Provide routes via env with api-keys array for fallback support
	jsonValue := `[
  {
    "model": "gpt-4",
    "protocol": "openai",
    "base-url": "https://api.example.com/v1",
    "api-keys": ["key-primary", "key-backup-1", "key-backup-2"],
    "upstream-model": "gpt-4-turbo"
  }
]`
	if err := os.Setenv("PASSTHRU_MODELS_JSON", jsonValue); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if len(cfg.Passthru) != 1 {
		t.Fatalf("expected 1 passthru route, got %d", len(cfg.Passthru))
	}
	r := cfg.Passthru[0]
	if r.Model != "gpt-4" {
		t.Fatalf("expected model gpt-4, got %q", r.Model)
	}
	if len(r.APIKeys) != 3 {
		t.Fatalf("expected 3 api-keys, got %d", len(r.APIKeys))
	}
	expectedKeys := []string{"key-primary", "key-backup-1", "key-backup-2"}
	for i, key := range r.APIKeys {
		if key != expectedKeys[i] {
			t.Fatalf("expected api-key[%d] to be %q, got %q", i, expectedKeys[i], key)
		}
	}
	if r.UpstreamModel != "gpt-4-turbo" {
		t.Fatalf("expected upstream-model gpt-4-turbo, got %q", r.UpstreamModel)
	}
}

func TestLoadConfigOptional_PassthruModelsJSON_ApiKeyAndApiKeys_Combined(t *testing.T) {
	// Ensure env var is cleared after test
	old := os.Getenv("PASSTHRU_MODELS_JSON")
	t.Cleanup(func() {
		_ = os.Setenv("PASSTHRU_MODELS_JSON", old)
	})

	// Minimal config file content
	tmp := t.TempDir()
	path := tmp + "/config.yaml"
	cfgYAML := []byte("port: 8317\n")
	if err := os.WriteFile(path, cfgYAML, 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	// Provide routes via env with both api-key (single) and api-keys (array)
	jsonValue := `[
  {
    "model": "claude-3-opus",
    "protocol": "claude",
    "base-url": "https://api.anthropic.com",
    "api-key": "sk-primary",
    "api-keys": ["sk-backup-1"],
    "upstream-model": "claude-3-opus-20240229"
  }
]`
	if err := os.Setenv("PASSTHRU_MODELS_JSON", jsonValue); err != nil {
		t.Fatalf("failed to set env: %v", err)
	}

	cfg, err := LoadConfigOptional(path, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected cfg")
	}
	if len(cfg.Passthru) != 1 {
		t.Fatalf("expected 1 passthru route, got %d", len(cfg.Passthru))
	}
	r := cfg.Passthru[0]
	// Single api-key should be preserved
	if r.APIKey != "sk-primary" {
		t.Fatalf("expected api-key sk-primary, got %q", r.APIKey)
	}
	// api-keys array should also be preserved
	if len(r.APIKeys) != 1 {
		t.Fatalf("expected 1 api-key in api-keys array, got %d", len(r.APIKeys))
	}
	if r.APIKeys[0] != "sk-backup-1" {
		t.Fatalf("expected api-keys[0] to be sk-backup-1, got %q", r.APIKeys[0])
	}
}
