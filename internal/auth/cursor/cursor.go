// Package cursor provides authentication helpers for the Cursor Composer API.
// Cursor uses a two-step auth flow: exchange a user API key (crsr_*) for an
// internal access token, then use that token for chat requests.
package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	// CursorAPIBase is the public Cursor API base URL for auth exchange and account info.
	CursorAPIBase = "https://api.cursor.com"

	// CursorClientVersion is the client version header sent to Cursor.
	// Must be a CURRENT Cursor IDE version. composer-api hardcodes "2.6.22"
	// but that is too old: Cursor's server accepts it for single-turn but
	// rejects multi-turn/agentic actions with ERROR_OUTDATED_CLIENT
	// ("Your version of Cursor no longer supports this action"). The latest
	// stable IDE version is fetched from
	// https://cursor.com/api/download?platform=linux-x64&releaseTrack=stable
	// (returns e.g. Cursor-3.5.38-x86_64.AppImage). Bump this when Cursor
	// ships a new release and the server starts rejecting again.
	// Override at runtime with the CURSOR_CLIENT_VERSION env var.
	CursorClientVersion = "3.5.38"

	// CursorClientType identifies the client type header. Must be "ide" to
	// match composer-api (which is the wire shape we emit). See note on
	// CursorClientVersion above for why we don't use "cli".
	CursorClientType = "ide"

	// DefaultBackendBaseURL is the direct Cursor Connect RPC endpoint.
	// The Go executor handles protobuf encoding and Connect framing natively.
	// Set to http://127.0.0.1:9797 in config to use the Node sidecar instead.
	DefaultBackendBaseURL = "https://api2.cursor.sh"

	// DefaultChatEndpoint is the OpenAI-compatible chat completions path (sidecar).
	DefaultChatEndpoint = "/v1/chat/completions"

	// DefaultCursorDirectChatEndpoint is the Connect RPC path for direct cursor.sh backends.
	DefaultCursorDirectChatEndpoint = "/aiserver.v1.ChatService/StreamUnifiedChatWithTools"
)

// CursorTokenStorage implements auth.TokenStorage for persisting Cursor credentials.
type CursorTokenStorage struct {
	APIKey string `json:"api_key"`
}

// SaveTokenToFile persists the Cursor token to the specified file path.
func (s *CursorTokenStorage) SaveTokenToFile(authFilePath string) error {
	return nil
}

// CursorAPIKeyFromAuth extracts the Cursor API key from an auth entry.
func CursorAPIKeyFromAuth(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["api_key"]); v != "" {
			return v
		}
	}
	if auth.Metadata == nil {
		return ""
	}
	v, _ := auth.Metadata["api_key"].(string)
	return strings.TrimSpace(v)
}

func resolveCursorConfigValue(auth *cliproxyauth.Auth, cfg *config.Config, attrKey string, fromEntry func(*config.CursorKey) string, fallback string) string {
	if auth != nil && auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes[attrKey]); v != "" {
			return v
		}
	}
	if cfg != nil {
		for i := range cfg.CursorKey {
			if v := fromEntry(&cfg.CursorKey[i]); v != "" {
				return v
			}
		}
	}
	return fallback
}

// ResolveBackendBaseURL returns the backend base URL for the given auth entry,
// falling back to the default.
func ResolveBackendBaseURL(auth *cliproxyauth.Auth, cfg *config.Config) string {
	return resolveCursorConfigValue(auth, cfg, "backend_base_url", func(e *config.CursorKey) string { return e.BackendBaseURL }, DefaultBackendBaseURL)
}

// ResolveChatEndpoint returns the chat endpoint path for the given auth entry,
// falling back to the default.
func ResolveChatEndpoint(auth *cliproxyauth.Auth, cfg *config.Config) string {
	return resolveCursorConfigValue(auth, cfg, "chat_endpoint", func(e *config.CursorKey) string { return e.ChatEndpoint }, DefaultChatEndpoint)
}

// ResolveCursorChatEndpoint returns the correct chat endpoint path for the
// given backend base URL. cursor.sh backends use Connect RPC; everything else
// uses the OpenAI-compatible sidecar path.
func ResolveCursorChatEndpoint(auth *cliproxyauth.Auth, cfg *config.Config) string {
	if explicit := ResolveChatEndpoint(auth, cfg); explicit != DefaultChatEndpoint {
		return explicit
	}
	backendBase := ResolveBackendBaseURL(auth, cfg)
	if strings.Contains(strings.ToLower(strings.TrimSpace(backendBase)), "cursor.sh") {
		return DefaultCursorDirectChatEndpoint
	}
	return DefaultChatEndpoint
}

// ResolveCursorClientVersion returns the configured Cursor client version,
// honoring the CURSOR_CLIENT_VERSION env var override. Strips any legacy
// "cli-" prefix from env overrides so old configs still produce a clean
// IDE-style version string.
func ResolveCursorClientVersion() string {
	version := CursorClientVersion
	if env := strings.TrimSpace(os.Getenv("CURSOR_CLIENT_VERSION")); env != "" {
		version = strings.TrimPrefix(env, "cli-")
	}
	return version
}

// ResolveCursorClientType returns the Cursor client type header value.
// Centralized so direct chat and auth exchange cannot drift apart.
func ResolveCursorClientType() string {
	return CursorClientType
}

// BuildCursorIdentityHeaders returns the standard Cursor identity headers used
// by both the auth exchange and chat requests. This is the single source of truth
// for client version/type so they can never drift apart. The version can be
// overridden at runtime via the CURSOR_CLIENT_VERSION env var.
//
// We identify as IDE (not cli) — see CursorClientType doc comment for the
// server-side multi-turn gating reason.
func BuildCursorIdentityHeaders() map[string]string {
	return map[string]string{
		"x-cursor-client-type":    ResolveCursorClientType(),
		"x-cursor-client-version": ResolveCursorClientVersion(),
		"x-ghost-mode":            "true",
	}
}

// ExchangeCursorApiKey exchanges a Cursor user API key for an internal access token
// against the direct Cursor backend (api2.cursor.sh). httpClient is injected by
// the executor to ensure proxy awareness.
func ExchangeCursorApiKey(ctx context.Context, apiKey string, httpClient *http.Client) (string, error) {
	return exchangeCursorApiKey(ctx, apiKey, DefaultBackendBaseURL, httpClient)
}

func cursorHTTPClient(httpClient *http.Client) *http.Client {
	if httpClient != nil {
		return httpClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func cursorHTTPError(resp *http.Response, unauthorizedMsg, op string) error {
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("cursor: %s", unauthorizedMsg)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cursor: %s failed with status %d: %s", op, resp.StatusCode, string(body))
	}
	return nil
}

// exchangeCursorApiKey exchanges a Cursor user API key for an internal access token.
func exchangeCursorApiKey(ctx context.Context, apiKey, backendBaseURL string, httpClient *http.Client) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	httpClient = cursorHTTPClient(httpClient)
	if backendBaseURL == "" {
		backendBaseURL = DefaultBackendBaseURL
	}
	backendBaseURL = strings.TrimRight(backendBaseURL, "/")

	url := backendBaseURL + "/auth/exchange_user_api_key"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", fmt.Errorf("cursor: failed to create exchange request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range BuildCursorIdentityHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("cursor: exchange request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("cursor: close exchange response body error: %v", errClose)
		}
	}()

	if err := cursorHTTPError(resp, "invalid API key", "exchange"); err != nil {
		return "", err
	}

	var result struct {
		AccessToken string `json:"accessToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("cursor: failed to decode exchange response: %w", err)
	}
	if result.AccessToken == "" {
		return "", fmt.Errorf("cursor: exchange response missing accessToken")
	}

	return result.AccessToken, nil
}

// VerifyCursorApiKey validates a Cursor API key by calling GET /v1/me.
// httpClient is injected by the executor to ensure proxy awareness.
func VerifyCursorApiKey(ctx context.Context, apiKey string, httpClient *http.Client) error {
	if ctx == nil {
		ctx = context.Background()
	}

	url := CursorAPIBase + "/v1/me"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("cursor: failed to create verify request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+apiKey)
	for k, v := range BuildCursorIdentityHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := cursorHTTPClient(httpClient).Do(req)
	if err != nil {
		return fmt.Errorf("cursor: verify request failed: %w", err)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("cursor: close verify response body error: %v", errClose)
		}
	}()

	return cursorHTTPError(resp, "invalid API key", "verify")
}
