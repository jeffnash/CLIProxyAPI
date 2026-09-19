package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// CodeBuddyAuthJSONEnv carries one exported CodeBuddy auth record (exactly
// the auth-dir file content, as printed by --codebuddy-export) for hosts
// without a login browser, such as Railway containers.
const CodeBuddyAuthJSONEnv = "CODEBUDDY_AUTH_JSON"

// codeBuddyEnvSeedLimit bounds the pasted bundle.
const codeBuddyEnvSeedLimit = int64(1 << 20)

// SeedCodeBuddyAuthFromEnv writes the CODEBUDDY_AUTH_JSON bundle into authDir
// when no record file exists for it yet, and reports whether it wrote one.
// An existing file always wins so a rotated token on disk is never clobbered
// by a stale paste; delete the file to force a reseed.
func SeedCodeBuddyAuthFromEnv(authDir string) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(CodeBuddyAuthJSONEnv))
	if raw == "" {
		return false, nil
	}
	if int64(len(raw)) > codeBuddyEnvSeedLimit {
		return false, fmt.Errorf("codebuddy: %s exceeds 1 MiB", CodeBuddyAuthJSONEnv)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil || metadata == nil {
		return false, fmt.Errorf("codebuddy: %s is not a JSON object: %v", CodeBuddyAuthJSONEnv, err)
	}
	if typeName, _ := metadata["type"].(string); !strings.EqualFold(strings.TrimSpace(typeName), codebuddy.Provider) {
		return false, fmt.Errorf("codebuddy: %s is not a codebuddy auth record", CodeBuddyAuthJSONEnv)
	}
	credentials, err := codebuddy.CredentialsFromMetadata(metadata)
	if err != nil {
		return false, fmt.Errorf("codebuddy: %s has invalid credentials: %w", CodeBuddyAuthJSONEnv, err)
	}
	catalog, err := codebuddy.CatalogFromMetadata(metadata)
	if err != nil || len(catalog.Models) == 0 {
		return false, fmt.Errorf("codebuddy: %s has no usable model catalog", CodeBuddyAuthJSONEnv)
	}
	dir := strings.TrimSpace(authDir)
	if dir == "" {
		return false, fmt.Errorf("codebuddy: auth dir is required to seed %s", CodeBuddyAuthJSONEnv)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, fmt.Errorf("codebuddy: create auth dir: %w", err)
	}
	target := filepath.Join(dir, codebuddy.Filename(credentials))
	if _, err := os.Stat(target); err == nil {
		log.Infof("codebuddy: leaving existing auth record %s", target)
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("codebuddy: stat auth record: %w", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(raw)); err != nil {
		return false, fmt.Errorf("codebuddy: %s is not valid JSON: %v", CodeBuddyAuthJSONEnv, err)
	}
	compact.WriteByte('\n')
	if err := os.WriteFile(target, compact.Bytes(), 0o600); err != nil {
		return false, fmt.Errorf("codebuddy: write seeded auth record: %w", err)
	}
	log.Infof("codebuddy: seeded auth record from %s", CodeBuddyAuthJSONEnv)
	return true, nil
}

// codeBuddyRefreshLead is the duration before token expiry when refresh should occur.
var codeBuddyRefreshLead = 5 * time.Minute

// codeBuddyPollInterval is the delay between authorization polls.
var codeBuddyPollInterval = 2 * time.Second

// codeBuddyHTTPClientFactory builds the provider HTTP client. Production uses
// a proxy-aware zero-timeout client; auth calls apply their own per-request
// deadline. Tests override this factory to inject a fake transport.
var codeBuddyHTTPClientFactory = func(cfg *config.Config) *http.Client {
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

// codeBuddyOpenBrowser opens the authorization URL. Tests override it.
var codeBuddyOpenBrowser = func(url string) error {
	return browser.OpenURL(url)
}

// codeBuddyBrowserAvailable reports browser availability. Tests override it.
var codeBuddyBrowserAvailable = func() bool {
	return browser.IsAvailable()
}

// codeBuddyWaitForPoll waits between authorization polls. Tests override it.
var codeBuddyWaitForPoll = func(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// CodeBuddyAuthenticator implements browser authorization login for CodeBuddy.
type CodeBuddyAuthenticator struct{}

// NewCodeBuddyAuthenticator constructs a new CodeBuddy authenticator.
func NewCodeBuddyAuthenticator() Authenticator {
	return &CodeBuddyAuthenticator{}
}

// Provider returns the provider key for codebuddy.
func (CodeBuddyAuthenticator) Provider() string {
	return codebuddy.Provider
}

// RefreshLead returns the duration before token expiry when refresh should occur.
func (CodeBuddyAuthenticator) RefreshLead() *time.Duration {
	return &codeBuddyRefreshLead
}

// Login runs the browser authorization flow: start a session, print the
// authorization URL once, optionally open it, poll until ready, then discover
// the account's HY4 catalog. A failed discovery prevents persistence and
// returns a retryable error; the login is not described as installed.
func (a CodeBuddyAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}
	realmName := ""
	if opts.Metadata != nil {
		realmName = strings.TrimSpace(opts.Metadata["realm"])
	}
	if realmName == "" {
		realmName = string(codebuddy.RealmCN)
	}
	realm, err := codebuddy.ParseRealm(realmName, "")
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := codebuddy.NewClient(codeBuddyHTTPClientFactory(cfg))

	fmt.Println("Starting CodeBuddy authentication...")
	session, err := client.StartLogin(ctx, realm)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: %w", err)
	}
	fmt.Printf("\nTo authenticate, please visit:\n%s\n\n", session.AuthURL)
	if !opts.NoBrowser && codeBuddyBrowserAvailable() {
		if errOpen := codeBuddyOpenBrowser(session.AuthURL); errOpen != nil {
			log.Warnf("Failed to open browser automatically: %v", errOpen)
		} else {
			fmt.Println("Browser opened automatically.")
		}
	}
	fmt.Println("Waiting for authorization...")
	credentials, err := codeBuddyPollUntilReady(ctx, client, session)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: %w", err)
	}
	catalog, err := client.FetchCatalog(ctx, credentials)
	if err != nil {
		return nil, fmt.Errorf("codebuddy: authorization succeeded but model discovery failed (retryable): %w", err)
	}
	now := time.Now()
	if len(catalog.Models) == 0 {
		fmt.Println("CodeBuddy account saved; no HY4 models are available in its discovered catalog.")
	} else {
		fmt.Println("\nCodeBuddy authentication successful!")
	}
	return NewCodeBuddyAuthRecord(credentials, catalog, now), nil
}

// codeBuddyPollUntilReady polls a login session until the operator authorizes,
// the context cancels, or the session expires.
func codeBuddyPollUntilReady(ctx context.Context, client *codebuddy.Client, session codebuddy.LoginSession) (codebuddy.Credentials, error) {
	for {
		credentials, ready, err := client.PollLogin(ctx, session)
		if err != nil {
			return codebuddy.Credentials{}, err
		}
		if ready {
			return credentials, nil
		}
		if err := codeBuddyWaitForPoll(ctx, codeBuddyPollInterval); err != nil {
			return codebuddy.Credentials{}, err
		}
	}
}

// NewCodeBuddyAuthRecord builds a metadata-only auth record for acquired
// credentials and their discovered catalog snapshot.
func NewCodeBuddyAuthRecord(c codebuddy.Credentials, catalog codebuddy.Catalog, at time.Time) *coreauth.Auth {
	filename := codebuddy.Filename(c)
	label := strings.TrimSpace(c.Nickname)
	if label == "" {
		label = "CodeBuddy " + string(c.Realm)
	}
	return &coreauth.Auth{
		ID: filename, Provider: codebuddy.Provider, FileName: filename,
		Label: label, Metadata: codebuddy.Metadata(c, catalog, at),
	}
}
