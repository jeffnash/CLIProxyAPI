package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	composerBridgeRequiredEnv      = "CURSOR_AGENT_BRIDGE_REQUIRED"
	composerBridgeReadinessTimeout = 3 * time.Second
)

var composerBridgeReadinessClient = &http.Client{
	Transport: &http.Transport{Proxy: nil},
}

func composerBridgeRequired() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(composerBridgeRequiredEnv))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func composerBridgeReadinessURL() (string, error) {
	base := strings.TrimSpace(os.Getenv("CURSOR_AGENT_BRIDGE_URL"))
	if base == "" {
		port := strings.TrimSpace(os.Getenv("CURSOR_AGENT_BRIDGE_PORT"))
		if port == "" {
			port = "9798"
		}
		base = "http://127.0.0.1:" + port
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Cursor bridge URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported Cursor bridge readiness scheme %q", parsed.Scheme)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/ready"
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func checkComposerBridgeReadiness(ctx context.Context) error {
	if !composerBridgeRequired() {
		return nil
	}
	endpoint, err := composerBridgeReadinessURL()
	if err != nil {
		return err
	}
	// Bound only the local health probe. This is comfortably below Railway's
	// 300-second healthcheckTimeout and does not affect established model streams.
	probeCtx, cancel := context.WithTimeout(ctx, composerBridgeReadinessTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build Cursor bridge readiness request: %w", err)
	}
	resp, err := composerBridgeReadinessClient.Do(req)
	if err != nil {
		return fmt.Errorf("Cursor bridge readiness request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Cursor bridge readiness returned HTTP %d", resp.StatusCode)
	}
	return nil
}
