package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// DoCodeBuddyExport prints one stored CodeBuddy auth record as compact JSON
// for copy/paste deployment via CODEBUDDY_AUTH_JSON. An empty realm exports
// the only record; multiple records require --codebuddy-realm. Only the JSON
// goes to stdout so it can be captured directly.
func DoCodeBuddyExport(cfg *config.Config, realm string) error {
	if cfg == nil {
		return fmt.Errorf("codebuddy: configuration is required")
	}
	dir := strings.TrimSpace(cfg.AuthDir)
	if dir == "" {
		return fmt.Errorf("codebuddy: auth dir is required")
	}
	realm = strings.ToLower(strings.TrimSpace(realm))
	pattern := "codebuddy-*.json"
	if realm != "" {
		pattern = "codebuddy-" + realm + "-*.json"
	}
	matches, err := filepath.Glob(filepath.Join(dir, pattern))
	if err != nil {
		return fmt.Errorf("codebuddy: list auth records: %w", err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		if realm == "" {
			return fmt.Errorf("codebuddy: no auth record found in %s; run --codebuddy-login first", dir)
		}
		return fmt.Errorf("codebuddy: no %s auth record found in %s", realm, dir)
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, match := range matches {
			names = append(names, filepath.Base(match))
		}
		return fmt.Errorf("codebuddy: multiple auth records (%s); specify --codebuddy-realm", strings.Join(names, ", "))
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		return fmt.Errorf("codebuddy: read auth record: %w", err)
	}
	if int64(len(raw)) > codeBuddyImportFileLimit {
		return fmt.Errorf("codebuddy: auth record exceeds 1 MiB")
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return fmt.Errorf("codebuddy: auth record is not valid JSON: %w", err)
	}
	if typeName, _ := metadata["type"].(string); !strings.EqualFold(strings.TrimSpace(typeName), codebuddy.Provider) {
		return fmt.Errorf("codebuddy: %s is not a codebuddy auth record", filepath.Base(matches[0]))
	}
	if _, err := codebuddy.CredentialsFromMetadata(metadata); err != nil {
		return fmt.Errorf("codebuddy: auth record has invalid credentials: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return fmt.Errorf("codebuddy: auth record is not valid JSON: %w", err)
	}
	fmt.Println(compact.String())
	return nil
}

// codeBuddyImportFileLimit bounds explicitly imported credential files.
const codeBuddyImportFileLimit = int64(1 << 20)

// codeBuddyImportHTTPClient builds the import HTTP client: proxy-aware with
// zero timeout. Credential calls apply their own per-request deadline.
// Tests override this factory to inject a fake transport.
var codeBuddyImportHTTPClient = func(cfg *config.Config) *http.Client {
	client := &http.Client{}
	var sdkCfg config.SDKConfig
	if cfg != nil {
		sdkCfg = cfg.SDKConfig
		if strings.TrimSpace(sdkCfg.ProxyURL) == "" {
			sdkCfg.ProxyURL = strings.TrimSpace(cfg.ProxyURL)
		}
	}
	return util.SetProxy(&sdkCfg, client)
}

// DoCodeBuddyLogin runs browser authorization for the given realm profile and
// saves the account with its discovered catalog. An empty realm defaults to
// CN. It builds its own signal-cancelable context.
func DoCodeBuddyLogin(cfg *config.Config, options *LoginOptions, realm string) error {
	if cfg == nil {
		return fmt.Errorf("codebuddy: configuration is required")
	}
	if options == nil {
		options = &LoginOptions{}
	}
	manager := newAuthManager()
	authOpts := &sdkAuth.LoginOptions{
		NoBrowser: options.NoBrowser,
		Metadata:  map[string]string{"realm": strings.TrimSpace(realm)},
		Prompt:    options.Prompt,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	record, savedPath, err := manager.Login(ctx, codebuddy.Provider, cfg, authOpts)
	if err != nil {
		return err
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if record != nil && record.Label != "" {
		fmt.Printf("Authenticated as %s\n", record.Label)
	}
	return nil
}

// DoCodeBuddyImport imports an explicitly supplied credential file: an
// external WorkBuddy credential JSON or a canonical CLIProxyAPI CodeBuddy
// auth file for catalog refresh. Exactly PATH is read; directories are never
// scanned and the external source file is never modified. Discovery runs
// before persistence, and a failed discovery leaves any previous canonical
// file untouched.
//
// One writer per credential owns the import window: pause the process doing
// token rotation for that credential, or import before server startup. The
// import aborts when the canonical target changes mid-flight instead of
// overwriting the newer file.
func DoCodeBuddyImport(ctx context.Context, cfg *config.Config, path, realmOverride string) error {
	if cfg == nil {
		return fmt.Errorf("codebuddy: configuration is required")
	}
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("codebuddy: import path is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("codebuddy: resolve import path: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("codebuddy: stat import file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("codebuddy: import path is not a regular file")
	}
	raw, err := readCodeBuddyImportFile(resolved)
	if err != nil {
		return err
	}
	now := time.Now()
	credentials, err := codebuddy.ImportCredentials(raw, realmOverride, now)
	if err != nil {
		return err
	}
	target := filepath.Join(strings.TrimSpace(cfg.AuthDir), codebuddy.Filename(credentials))
	snapshot, snapshotExists := readCodeBuddyImportSnapshot(target)
	client := codebuddy.NewClient(codeBuddyImportHTTPClient(cfg))
	if credentials.Expired.IsZero() || credentials.Expired.Before(now) {
		if strings.TrimSpace(credentials.RefreshToken) == "" {
			return fmt.Errorf("codebuddy: credential expired and no refresh token is available; re-login is required")
		}
		credentials, err = client.Refresh(ctx, credentials)
		if err != nil {
			return fmt.Errorf("codebuddy: import refresh failed: %w", err)
		}
	}
	catalog, err := client.FetchCatalog(ctx, credentials)
	if err != nil {
		return fmt.Errorf("codebuddy: import discovery failed: %w", err)
	}
	record := sdkAuth.NewCodeBuddyAuthRecord(credentials, catalog, time.Now())
	if snapshotExists {
		var existingMap map[string]any
		if err := json.Unmarshal(snapshot, &existingMap); err == nil && len(existingMap) > 0 {
			coreauth.MergeExistingAuthMetadata(record, existingMap)
			if disabled, _ := existingMap["disabled"].(bool); disabled {
				record.Disabled = true
			}
		}
	}
	latest, latestExists := readCodeBuddyImportSnapshot(target)
	if latestExists != snapshotExists || !bytes.Equal(latest, snapshot) {
		return fmt.Errorf("codebuddy: credential file changed during import; retry after the active refresh completes")
	}
	store := sdkAuth.GetTokenStore()
	if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
		dirSetter.SetBaseDir(cfg.AuthDir)
	}
	savedPath, err := store.Save(ctx, record)
	if err != nil {
		return fmt.Errorf("codebuddy: save imported account: %w", err)
	}
	if savedPath != "" {
		fmt.Printf("Authentication saved to %s\n", savedPath)
	}
	if len(catalog.Models) == 0 {
		fmt.Println("CodeBuddy account saved; no HY4 models are available in its discovered catalog.")
	} else {
		fmt.Println("CodeBuddy import successful!")
	}
	return nil
}

// readCodeBuddyImportFile reads exactly one file with an overflow check.
func readCodeBuddyImportFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: open import file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()
	raw, err := io.ReadAll(io.LimitReader(file, codeBuddyImportFileLimit+1))
	if err != nil {
		return nil, fmt.Errorf("codebuddy: read import file: %w", err)
	}
	if int64(len(raw)) > codeBuddyImportFileLimit {
		return nil, fmt.Errorf("codebuddy: import file exceeds %d bytes", codeBuddyImportFileLimit)
	}
	return raw, nil
}

// readCodeBuddyImportSnapshot reads the current canonical target bytes for
// concurrent-change detection. Comparison only; bytes are never logged.
func readCodeBuddyImportSnapshot(target string) ([]byte, bool) {
	raw, err := os.ReadFile(target)
	if err != nil {
		return nil, false
	}
	return raw, true
}
