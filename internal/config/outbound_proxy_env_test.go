package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOutboundProxyEnvOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8080\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	oldOutbound := os.Getenv("OUTBOUND_PROXY_URL")
	oldHTTPS := os.Getenv("HTTPS_PROXY")
	oldHTTP := os.Getenv("HTTP_PROXY")
	t.Cleanup(func() {
		_ = os.Setenv("OUTBOUND_PROXY_URL", oldOutbound)
		_ = os.Setenv("HTTPS_PROXY", oldHTTPS)
		_ = os.Setenv("HTTP_PROXY", oldHTTP)
	})

	_ = os.Setenv("HTTPS_PROXY", "http://should-not-win.example:8080")
	_ = os.Setenv("HTTP_PROXY", "http://should-not-win.example:8080")
	if err := os.Setenv("OUTBOUND_PROXY_URL", "socks5://user:pass@proxy.example:1080"); err != nil {
		t.Fatalf("set OUTBOUND_PROXY_URL: %v", err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ProxyURL != "socks5://user:pass@proxy.example:1080" {
		t.Fatalf("ProxyURL mismatch: got %q", cfg.ProxyURL)
	}
}

func TestOutboundProxyEnvFallbackStandard(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("port: 8080\nproxy-url: \"\"\n"), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	oldOutbound := os.Getenv("OUTBOUND_PROXY_URL")
	oldHTTPS := os.Getenv("HTTPS_PROXY")
	oldHTTP := os.Getenv("HTTP_PROXY")
	t.Cleanup(func() {
		_ = os.Setenv("OUTBOUND_PROXY_URL", oldOutbound)
		_ = os.Setenv("HTTPS_PROXY", oldHTTPS)
		_ = os.Setenv("HTTP_PROXY", oldHTTP)
	})

	_ = os.Unsetenv("OUTBOUND_PROXY_URL")
	if err := os.Setenv("HTTPS_PROXY", "http://proxy.example:3128"); err != nil {
		t.Fatalf("set HTTPS_PROXY: %v", err)
	}
	_ = os.Unsetenv("HTTP_PROXY")

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ProxyURL != "http://proxy.example:3128" {
		t.Fatalf("ProxyURL mismatch: got %q", cfg.ProxyURL)
	}
}

