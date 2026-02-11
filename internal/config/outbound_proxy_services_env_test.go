package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutboundProxyServicesEnvParsing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8080\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	oldProxyURL := os.Getenv("OUTBOUND_PROXY_URL")
	oldServices := os.Getenv("OUTBOUND_PROXY_SERVICES")
	t.Cleanup(func() {
		_ = os.Setenv("OUTBOUND_PROXY_URL", oldProxyURL)
		_ = os.Setenv("OUTBOUND_PROXY_SERVICES", oldServices)
	})

	_ = os.Setenv("OUTBOUND_PROXY_URL", "http://proxy.example:3128")
	_ = os.Setenv("OUTBOUND_PROXY_SERVICES", " CoPiLoT,  codex ,, ")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got, want := len(cfg.ProxyServices), 2; got != want {
		t.Fatalf("ProxyServices len=%d want %d (%v)", got, want, cfg.ProxyServices)
	}
	if cfg.ProxyServices[0] != "copilot" || cfg.ProxyServices[1] != "codex" {
		t.Fatalf("ProxyServices=%v want [copilot codex]", cfg.ProxyServices)
	}

	if !cfg.SDKConfig.ProxyEnabledFor("copilot") {
		t.Fatalf("expected ProxyEnabledFor(copilot)=true")
	}
	if cfg.SDKConfig.ProxyEnabledFor("gemini") {
		t.Fatalf("expected ProxyEnabledFor(gemini)=false")
	}
}

func TestProxyEnabledFor_EmptyAllowlistMeansAll(t *testing.T) {
	cfg := &SDKConfig{ProxyURL: "http://proxy.example:3128"}
	if !cfg.ProxyEnabledFor("copilot") {
		t.Fatalf("expected ProxyEnabledFor(copilot)=true with empty allowlist")
	}
	if !cfg.ProxyEnabledFor("codex") {
		t.Fatalf("expected ProxyEnabledFor(codex)=true with empty allowlist")
	}
}

