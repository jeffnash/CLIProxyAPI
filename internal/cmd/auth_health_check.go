package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const defaultAuthHealthTimeout = 90 * time.Second

// AuthHealthOptions controls the deploy-time auth validation command.
type AuthHealthOptions struct {
	OutputPath string
	Timeout    time.Duration
}

type authHealthRecord struct {
	Status   string
	FileName string
	Provider string
	Reason   string
}

// DoAuthHealthCheck validates auth files in cfg.AuthDir and writes a TSV report.
func DoAuthHealthCheck(ctx context.Context, cfg *config.Config, opts AuthHealthOptions) error {
	if cfg == nil {
		return fmt.Errorf("auth health check: config is nil")
	}
	if strings.TrimSpace(cfg.AuthDir) == "" {
		return fmt.Errorf("auth health check: auth dir is empty")
	}
	if strings.TrimSpace(opts.OutputPath) == "" {
		return fmt.Errorf("auth health check: output path is empty")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultAuthHealthTimeout
	}
	if ctx == nil {
		ctx = context.Background()
	}

	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(cfg.AuthDir)
	auths, err := store.List(ctx)
	if err != nil {
		return fmt.Errorf("auth health check: list auths: %w", err)
	}
	sort.Slice(auths, func(i, j int) bool {
		return authRelativePath(auths[i], cfg.AuthDir) < authRelativePath(auths[j], cfg.AuthDir)
	})

	executors := authHealthExecutors(cfg)
	records := make([]authHealthRecord, 0, len(auths))
	for _, auth := range auths {
		record := validateAuthRecord(ctx, store, cfg.AuthDir, executors, timeout, auth)
		records = append(records, record)
	}

	if err = writeAuthHealthReport(opts.OutputPath, records); err != nil {
		return err
	}
	return nil
}

func authHealthExecutors(cfg *config.Config) map[string]coreauth.ProviderExecutor {
	executors := []coreauth.ProviderExecutor{
		executor.NewCodexAutoExecutor(cfg),
		executor.NewClaudeExecutor(cfg),
		executor.NewGeminiExecutor(cfg),
		executor.NewGeminiVertexExecutor(cfg),
		executor.NewGeminiCLIExecutor(cfg),
		executor.NewAntigravityExecutor(cfg),
		executor.NewCopilotExecutor(cfg),
		executor.NewKiroExecutor(cfg),
		executor.NewKimiExecutor(cfg),
		executor.NewXAIExecutor(cfg),
		executor.NewQwenExecutor(cfg),
		executor.NewIFlowExecutor(cfg),
		executor.NewGrokExecutor(cfg),
		executor.NewChutesExecutor(cfg),
		executor.NewAIStudioExecutor(cfg, "aistudio", nil),
		executor.NewCursorExecutor(cfg),
	}
	out := make(map[string]coreauth.ProviderExecutor, len(executors))
	for _, exec := range executors {
		if exec == nil {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(exec.Identifier()))
		if provider != "" {
			out[provider] = exec
		}
	}
	return out
}

func validateAuthRecord(ctx context.Context, store *sdkAuth.FileTokenStore, baseDir string, executors map[string]coreauth.ProviderExecutor, timeout time.Duration, auth *coreauth.Auth) authHealthRecord {
	record := authHealthRecord{
		Status:   "unknown",
		FileName: authRelativePath(auth, baseDir),
		Provider: authProvider(auth),
		Reason:   "not_checked",
	}
	if auth == nil {
		record.Reason = "nil_auth"
		return record
	}
	if auth.Disabled {
		record.Status = "skipped"
		record.Reason = "disabled"
		return record
	}

	exec := executors[record.Provider]
	if exec == nil {
		if status, reason, ok := passiveAuthStatus(auth); ok {
			record.Status = status
			record.Reason = reason
			return record
		}
		record.Reason = "executor_not_registered"
		return record
	}

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	refreshed, err := exec.Refresh(checkCtx, auth.Clone())
	if err != nil {
		if status, reason := classifyAuthRefreshError(err); status != "" {
			record.Status = status
			record.Reason = reason
			return record
		}
		record.Status = "unknown"
		record.Reason = "refresh_error_unclassified"
		return record
	}

	if refreshed == nil {
		refreshed = auth
	}
	if refreshed.Attributes == nil {
		refreshed.Attributes = make(map[string]string)
	}
	if path := authFilePath(auth); path != "" {
		refreshed.Attributes["path"] = path
	}
	if refreshed.FileName == "" {
		refreshed.FileName = auth.FileName
	}
	if refreshed.ID == "" {
		refreshed.ID = auth.ID
	}
	if refreshed.Metadata != nil {
		if _, errSave := store.Save(ctx, refreshed); errSave != nil {
			record.Status = "unknown"
			record.Reason = "save_failed"
			return record
		}
	}

	if hasRefreshMaterial(auth) || hasRefreshMaterial(refreshed) {
		record.Status = "valid"
		record.Reason = "refresh_succeeded"
		return record
	}
	if status, reason, ok := passiveAuthStatus(refreshed); ok {
		record.Status = status
		record.Reason = reason
		return record
	}
	record.Status = "unknown"
	record.Reason = "refresh_noop"
	return record
}

func authProvider(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "" {
		return provider
	}
	if auth.Metadata != nil {
		if raw, ok := auth.Metadata["type"].(string); ok {
			return strings.ToLower(strings.TrimSpace(raw))
		}
	}
	return ""
}

func authRelativePath(auth *coreauth.Auth, baseDir string) string {
	if auth == nil {
		return ""
	}
	if fileName := strings.TrimSpace(auth.FileName); fileName != "" {
		return filepath.Clean(fileName)
	}
	if path := authFilePath(auth); path != "" {
		if rel, err := filepath.Rel(baseDir, path); err == nil && rel != "" && !strings.HasPrefix(rel, "..") {
			return filepath.Clean(rel)
		}
		return filepath.Clean(path)
	}
	return filepath.Clean(strings.TrimSpace(auth.ID))
}

func authFilePath(auth *coreauth.Auth) string {
	if auth == nil || auth.Attributes == nil {
		return ""
	}
	for _, key := range []string{"path", "source"} {
		if value := strings.TrimSpace(auth.Attributes[key]); value != "" {
			return value
		}
	}
	return ""
}

func passiveAuthStatus(auth *coreauth.Auth) (string, string, bool) {
	if auth == nil {
		return "unknown", "nil_auth", true
	}
	expiry, ok := auth.ExpirationTime()
	if !ok {
		return "", "", false
	}
	if expiry.After(time.Now().UTC().Add(30 * time.Second)) {
		return "valid", "access_token_not_expired", true
	}
	return "invalid", "access_token_expired", true
}

func classifyAuthRefreshError(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	var statusCoder interface {
		StatusCode() int
	}
	if errors.As(err, &statusCoder) && statusCoder != nil {
		switch statusCoder.StatusCode() {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "invalid", "unauthorized"
		case http.StatusBadRequest:
			if looksLikeInvalidCredentialError(err.Error()) {
				return "invalid", "refresh_rejected"
			}
			return "unknown", "bad_request"
		case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return "unknown", "provider_unavailable"
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "unknown", "refresh_timeout"
	}
	if looksLikeInvalidCredentialError(err.Error()) {
		return "invalid", "refresh_rejected"
	}
	return "", ""
}

func looksLikeInvalidCredentialError(raw string) bool {
	lower := strings.ToLower(raw)
	fragments := []string{
		"invalid_grant",
		"bad-credentials",
		"bad credentials",
		"refresh token has been revoked",
		"refresh token revoked",
		"refresh_token_reused",
		"refresh token reused",
		"invalid refresh token",
		"invalid or expired token",
		"access_denied",
		"reauthenticate required",
		"re-authenticate required",
	}
	for _, fragment := range fragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func hasRefreshMaterial(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if containsRefreshMaterial(auth.Metadata) {
		return true
	}
	if len(auth.Attributes) == 0 {
		return false
	}
	for _, key := range []string{"refresh_token", "refreshToken", "github_token", "githubToken", "sso_token", "ssoToken", "cookie"} {
		if strings.TrimSpace(auth.Attributes[key]) != "" {
			return true
		}
	}
	return false
}

func containsRefreshMaterial(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, val := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
			switch normalized {
			case "refresh_token", "refreshtoken", "github_token", "githubtoken", "sso_token", "ssotoken", "cookie":
				if nonEmptyAuthHealthValue(val) {
					return true
				}
			}
			if containsRefreshMaterial(val) {
				return true
			}
		}
	case map[string]string:
		for key, val := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
			switch normalized {
			case "refresh_token", "refreshtoken", "github_token", "githubtoken", "sso_token", "ssotoken", "cookie":
				if strings.TrimSpace(val) != "" {
					return true
				}
			}
		}
	case []any:
		for _, val := range typed {
			if containsRefreshMaterial(val) {
				return true
			}
		}
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(typed, &decoded); err == nil {
			return containsRefreshMaterial(decoded)
		}
	}
	return false
}

func nonEmptyAuthHealthValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []byte:
		return strings.TrimSpace(string(typed)) != ""
	case fmt.Stringer:
		return strings.TrimSpace(typed.String()) != ""
	default:
		return strings.TrimSpace(fmt.Sprint(typed)) != ""
	}
}

func writeAuthHealthReport(path string, records []authHealthRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("auth health check: create output dir: %w", err)
	}
	var builder strings.Builder
	for _, record := range records {
		builder.WriteString(cleanAuthHealthField(record.Status))
		builder.WriteByte('\t')
		builder.WriteString(cleanAuthHealthField(record.FileName))
		builder.WriteByte('\t')
		builder.WriteString(cleanAuthHealthField(record.Provider))
		builder.WriteByte('\t')
		builder.WriteString(cleanAuthHealthField(record.Reason))
		builder.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("auth health check: write report: %w", err)
	}
	return nil
}

func cleanAuthHealthField(value string) string {
	value = strings.ReplaceAll(value, "\t", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.TrimSpace(value)
}
