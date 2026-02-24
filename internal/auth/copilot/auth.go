package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	copilotshared "github.com/router-for-me/CLIProxyAPI/v6/internal/copilot"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	log "github.com/sirupsen/logrus"
)

// Copilot uses a two-step OAuth (GitHub device code -> Copilot token) plus account-type-specific
// base URLs and strict header requirements. This file centralizes that multi-hop flow so both
// CLI and management endpoints can trigger auth without duplicating device code polling,
// token exchange, or account-type handling.
// DeviceCodeResponse represents the response from GitHub's device code endpoint.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// AccessTokenResponse represents the response from GitHub's access token endpoint.
type AccessTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`
	Error       string `json:"error"`
}

// CopilotTokenResponse represents the response from GitHub's Copilot token endpoint.
type CopilotTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expires_at"`
	RefreshIn int    `json:"refresh_in"`
}

// GitHubUserResponse represents the response from GitHub's user endpoint.
type GitHubUserResponse struct {
	Login string `json:"login"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

// CopilotAuth handles the GitHub Copilot OAuth2 device code authentication flow.
type CopilotAuth struct {
	httpClient    *http.Client
	vsCodeVersion string
	proxyURL      string
	noProxy       string
}

var (
	githubCredentialIndexMu   = make(chan struct{}, 1)
	githubCredentialIndexByID = map[string]int{}
	nextGitHubCredentialIndex = 1
)

func credentialIndexForID(id string) int {
	id = strings.TrimSpace(id)
	if id == "" {
		return 0
	}
	githubCredentialIndexMu <- struct{}{}
	defer func() { <-githubCredentialIndexMu }()
	if idx, ok := githubCredentialIndexByID[id]; ok {
		return idx
	}
	idx := nextGitHubCredentialIndex
	nextGitHubCredentialIndex++
	githubCredentialIndexByID[id] = idx
	return idx
}

// NewCopilotAuth creates a new CopilotAuth service instance.
// It initializes an HTTP client with proxy settings from the provided configuration.
func NewCopilotAuth(cfg *config.Config) *CopilotAuth {
	proxyURL := ""
	noProxy := ""
	httpClient := &http.Client{Timeout: 30 * time.Second}

	if cfg != nil && strings.TrimSpace(cfg.ProxyURL) != "" && cfg.SDKConfig.ProxyEnabledFor("copilot") {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
		httpClient = util.SetProxyForService(&cfg.SDKConfig, "copilot", httpClient)
	}

	noProxy = strings.TrimSpace(os.Getenv("NO_PROXY"))
	if noProxy == "" {
		noProxy = strings.TrimSpace(os.Getenv("no_proxy"))
	}
	return &CopilotAuth{
		httpClient:    httpClient,
		vsCodeVersion: DefaultVSCodeVersion,
		proxyURL:      proxyURL,
		noProxy:       noProxy,
	}
}

func (a *CopilotAuth) logProxyUsedForRefresh(urlStr string) {
	// Caller asked for a single, explicit log line each refresh.
	host := ""
	if u, err := url.Parse(urlStr); err == nil && u != nil {
		host = strings.TrimSpace(u.Hostname())
	}

	used := false
	if strings.TrimSpace(a.proxyURL) != "" {
		// Best-effort NO_PROXY evaluation.
		if host != "" && util.ShouldBypassProxy(host, util.ParseNoProxyList(a.noProxy)) {
			used = false
		} else {
			used = true
		}
	}

	log.Infof("copilot auth refresh: proxy_used=%t proxy=%s host=%s no_proxy=%q",
		used,
		util.MaskProxyURL(a.proxyURL),
		host,
		strings.TrimSpace(a.noProxy),
	)
}

// GetDeviceCode initiates the device code flow by requesting a device code from GitHub.
func (a *CopilotAuth) GetDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	reqBody := map[string]string{
		"client_id": GitHubClientID,
		"scope":     GitHubAppScopes,
	}
	bodyBytes, _ := json.Marshal(reqBody)

	log.Infof("copilot github call: endpoint=%s credential=<device-flow>", DeviceCodePath)
	req, err := http.NewRequestWithContext(ctx, "POST", GitHubBaseURL+DeviceCodePath, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, NewAuthenticationError(ErrDeviceCodeFailed, err)
	}

	for k, v := range StandardHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, NewAuthenticationError(ErrDeviceCodeFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, NewAuthenticationError(ErrDeviceCodeFailed, fmt.Errorf("failed to read response: %w", err))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, NewAuthenticationError(ErrDeviceCodeFailed, fmt.Errorf("status %d: %s", resp.StatusCode, string(body)))
	}

	var deviceCode DeviceCodeResponse
	if err = json.Unmarshal(body, &deviceCode); err != nil {
		return nil, NewAuthenticationError(ErrDeviceCodeFailed, fmt.Errorf("failed to parse response: %w", err))
	}

	log.Debugf("Device code response: %+v", deviceCode)
	return &deviceCode, nil
}

// PollAccessToken polls GitHub for an access token after the user has entered the device code.
// It implements exponential backoff and handles various error conditions.
func (a *CopilotAuth) PollAccessToken(ctx context.Context, deviceCode *DeviceCodeResponse) (string, error) {
	if deviceCode == nil {
		return "", NewAuthenticationError(ErrTokenExchangeFailed, fmt.Errorf("device code is nil"))
	}

	// Add 1 second buffer to the interval
	interval := time.Duration(deviceCode.Interval+1) * time.Second
	expiry := time.Now().Add(time.Duration(deviceCode.ExpiresIn) * time.Second)

	log.Debugf("Polling access token with interval %v", interval)

	for time.Now().Before(expiry) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}

		token, err := a.tryGetAccessToken(ctx, deviceCode.DeviceCode)
		if err == nil && token != "" {
			return token, nil
		}

		// Prefer structured AuthenticationError types.
		var authErr *AuthenticationError
		if errors.As(err, &authErr) {
			switch authErr.Type {
			case ErrAuthorizationPending.Type:
				log.Debug("Authorization pending, continuing to poll...")
				continue
			case ErrSlowDown.Type:
				interval += 5 * time.Second
				log.Debugf("Slowing down, new interval: %v", interval)
				continue
			case ErrAccessDenied.Type, ErrDeviceCodeExpired.Type:
				return "", authErr
			default:
				log.Warnf("Error polling access token: %v", authErr)
			}
		} else if err != nil {
			log.Warnf("Error polling access token: %v", err)
		}
	}

	return "", ErrDeviceCodeExpired
}

func (a *CopilotAuth) tryGetAccessToken(ctx context.Context, deviceCode string) (string, error) {
	reqBody := map[string]string{
		"client_id":   GitHubClientID,
		"device_code": deviceCode,
		"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
	}
	bodyBytes, _ := json.Marshal(reqBody)

	log.Infof("copilot github call: endpoint=%s credential=<device-code>", AccessTokenPath)
	req, err := http.NewRequestWithContext(ctx, "POST", GitHubBaseURL+AccessTokenPath, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}

	for k, v := range StandardHeaders() {
		req.Header.Set(k, v)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var tokenResp AccessTokenResponse
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		// Try parsing as URL-encoded form (GitHub sometimes returns this format)
		values, parseErr := url.ParseQuery(string(body))
		if parseErr != nil {
			return "", fmt.Errorf("failed to parse token response as JSON (%v) or form-urlencoded (%w)", err, parseErr)
		}
		tokenResp.AccessToken = values.Get("access_token")
		tokenResp.Error = values.Get("error")
	}

	log.Debugf("Access token response received (token: %s, error: %s)", MaskToken(tokenResp.AccessToken), tokenResp.Error)

	switch tokenResp.Error {
	case "":
		if tokenResp.AccessToken != "" {
			return tokenResp.AccessToken, nil
		}
		return "", ErrAuthorizationPending
	case ErrAuthorizationPending.Type:
		return "", ErrAuthorizationPending
	case ErrSlowDown.Type:
		return "", ErrSlowDown
	case ErrAccessDenied.Type:
		return "", ErrAccessDenied
	case ErrDeviceCodeExpired.Type:
		return "", ErrDeviceCodeExpired
	default:
		return "", NewAuthenticationError(ErrTokenExchangeFailed, fmt.Errorf("oauth error: %s", tokenResp.Error))
	}
}

// GetCopilotToken exchanges a GitHub access token for a Copilot API token.
func (a *CopilotAuth) GetCopilotToken(ctx context.Context, githubToken string) (*CopilotTokenResponse, error) {
	if githubToken == "" {
		return nil, ErrNoGitHubToken
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(githubToken))
	credID := fmt.Sprintf("gh_%x", h.Sum64())
	credIndex := credentialIndexForID(credID)
	log.Infof("copilot github call: endpoint=%s credential=%s index=%d", CopilotTokenPath, credID, credIndex)

	req, err := http.NewRequestWithContext(ctx, "GET", GitHubAPIBaseURL+CopilotTokenPath, nil)
	if err != nil {
		return nil, NewAuthenticationError(ErrCopilotTokenFailed, err)
	}

	for k, v := range GitHubHeaders(githubToken, a.vsCodeVersion) {
		req.Header.Set(k, v)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, NewAuthenticationError(ErrCopilotTokenFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, NewAuthenticationError(ErrCopilotTokenFailed, fmt.Errorf("failed to read response: %w", err))
	}

	// Return structured auth errors for auth-related status codes.
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, NewAuthenticationError(ErrNoCopilotSubscription, fmt.Errorf("status %d: %s", resp.StatusCode, string(body)))
	}

	if resp.StatusCode != http.StatusOK {
		return nil, NewAuthenticationError(ErrCopilotTokenFailed, fmt.Errorf("status %d: %s", resp.StatusCode, string(body)))
	}

	var tokenResp CopilotTokenResponse
	if err = json.Unmarshal(body, &tokenResp); err != nil {
		return nil, NewAuthenticationError(ErrCopilotTokenFailed, fmt.Errorf("failed to parse response: %w", err))
	}

	log.Debug("Copilot token fetched successfully")
	return &tokenResp, nil
}

// GetGitHubUser fetches the authenticated user's information from GitHub.
func (a *CopilotAuth) GetGitHubUser(ctx context.Context, githubToken string) (*GitHubUserResponse, error) {
	if githubToken == "" {
		return nil, ErrNoGitHubToken
	}

	h := fnv.New64a()
	_, _ = h.Write([]byte(githubToken))
	credID := fmt.Sprintf("gh_%x", h.Sum64())
	credIndex := credentialIndexForID(credID)
	log.Infof("copilot github call: endpoint=%s credential=%s index=%d", UserInfoPath, credID, credIndex)

	req, err := http.NewRequestWithContext(ctx, "GET", GitHubAPIBaseURL+UserInfoPath, nil)
	if err != nil {
		return nil, err
	}

	// Use simpler headers for the GitHub user API - only authorization and standard headers
	req.Header.Set("Authorization", fmt.Sprintf("token %s", githubToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", CopilotUserAgentValue())

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		log.Debugf("GitHub user API error response: %s", string(body))
		return nil, fmt.Errorf("failed to get user info: status %d, body: %s", resp.StatusCode, string(body))
	}

	var user GitHubUserResponse
	if err = json.Unmarshal(body, &user); err != nil {
		return nil, err
	}

	return &user, nil
}

// CopilotModel represents a model available through the Copilot API.
type CopilotModel struct {
	ID                 string              `json:"id"`
	Name               string              `json:"name"`
	Object             string              `json:"object"`
	Version            string              `json:"version"`
	Vendor             string              `json:"vendor"`
	Preview            bool                `json:"preview"`
	ModelPickerEnabled bool                `json:"model_picker_enabled"`
	Capabilities       CopilotCapabilities `json:"capabilities"`
}

// CopilotCapabilities describes the capabilities of a Copilot model.
type CopilotCapabilities struct {
	Family    string          `json:"family"`
	Type      string          `json:"type"`
	Tokenizer string          `json:"tokenizer"`
	Limits    CopilotLimits   `json:"limits"`
	Supports  CopilotSupports `json:"supports"`
}

// CopilotLimits describes the token limits for a Copilot model.
type CopilotLimits struct {
	MaxContextWindowTokens int `json:"max_context_window_tokens"`
	MaxOutputTokens        int `json:"max_output_tokens"`
	MaxPromptTokens        int `json:"max_prompt_tokens"`
}

// CopilotSupports describes the features supported by a Copilot model.
type CopilotSupports struct {
	ToolCalls         bool `json:"tool_calls"`
	ParallelToolCalls bool `json:"parallel_tool_calls"`
}

// CopilotModelsResponse represents the response from the Copilot models endpoint.
type CopilotModelsResponse struct {
	Data   []CopilotModel `json:"data"`
	Object string         `json:"object"`
}

// GetModels fetches the available models from the Copilot API.
func (a *CopilotAuth) GetModels(ctx context.Context, copilotToken string, accountType AccountType) (*CopilotModelsResponse, error) {
	if copilotToken == "" {
		return nil, ErrNoCopilotToken
	}

	baseURL := CopilotBaseURL(accountType)
	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create models request: %w", err)
	}

	for k, v := range CopilotHeaders(copilotToken, a.vsCodeVersion, false) {
		req.Header.Set(k, v)
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("models request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read models response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models request failed with status %d: %s", resp.StatusCode, string(body))
	}

	var modelsResp CopilotModelsResponse
	if err = json.Unmarshal(body, &modelsResp); err != nil {
		return nil, fmt.Errorf("failed to parse models response: %w", err)
	}

	log.Debugf("Fetched %d models from Copilot API", len(modelsResp.Data))
	return &modelsResp, nil
}

// RefreshCopilotToken refreshes the Copilot token using the stored GitHub token.
func (a *CopilotAuth) RefreshCopilotToken(ctx context.Context, storage *CopilotTokenStorage) error {
	if storage == nil || storage.GitHubToken == "" {
		return ErrNoGitHubToken
	}

	// Log proxy usage for this refresh call (explicitly requested).
	a.logProxyUsedForRefresh(GitHubAPIBaseURL + path.Clean(CopilotTokenPath))

	tokenResp, err := a.GetCopilotToken(ctx, storage.GitHubToken)
	if err != nil {
		return err
	}

	storage.CopilotToken = tokenResp.Token
	storage.CopilotTokenExpiry = time.Unix(tokenResp.ExpiresAt, 0).Format(time.RFC3339)
	storage.RefreshIn = tokenResp.RefreshIn
	storage.LastRefresh = time.Now().Format(time.RFC3339)

	return nil
}

// PerformFullAuth performs the complete authentication flow.
func (a *CopilotAuth) PerformFullAuth(ctx context.Context, accountType AccountType, onDeviceCode func(*DeviceCodeResponse)) (*CopilotTokenStorage, error) {
	deviceCode, err := a.GetDeviceCode(ctx)
	if err != nil {
		return nil, err
	}

	if onDeviceCode != nil {
		onDeviceCode(deviceCode)
	}

	result, err := a.finalizeAuth(ctx, deviceCode, accountType)
	if err != nil {
		return nil, err
	}
	return result.Storage, nil
}

// AccountTypeValidationResult aliases the shared validation result type.
type AccountTypeValidationResult = copilotshared.AccountTypeValidationResult

// ParseAccountType delegates to the shared Copilot account type parser.
func ParseAccountType(s string) (AccountType, bool) { return copilotshared.ParseAccountType(s) }

// ValidateAccountType delegates to the shared Copilot account type validator.
func ValidateAccountType(s string) AccountTypeValidationResult {
	return copilotshared.ValidateAccountType(s)
}

type AuthResult struct {
	// Storage contains the token data to be persisted.
	Storage *CopilotTokenStorage
	// SuggestedFilename is the recommended filename for saving the token.
	SuggestedFilename string
}

func (a *CopilotAuth) PerformFullAuthWithFilename(ctx context.Context, accountType AccountType, onDeviceCode func(*DeviceCodeResponse)) (*AuthResult, error) {
	deviceCode, err := a.GetDeviceCode(ctx)
	if err != nil {
		return nil, err
	}

	if onDeviceCode != nil {
		onDeviceCode(deviceCode)
	}

	return a.finalizeAuth(ctx, deviceCode, accountType)
}

func (a *CopilotAuth) CompleteAuthWithDeviceCode(ctx context.Context, deviceCode *DeviceCodeResponse, accountType AccountType) (*AuthResult, error) {
	return a.finalizeAuth(ctx, deviceCode, accountType)
}

// finalizeAuth performs: Poll -> Exchange -> User Info -> Storage Build -> Filename Gen
func (a *CopilotAuth) finalizeAuth(ctx context.Context, deviceCode *DeviceCodeResponse, accountType AccountType) (*AuthResult, error) {
	// 1. Poll GitHub Token
	githubToken, err := a.PollAccessToken(ctx, deviceCode)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain GitHub token: %w", err)
	}
	log.Info("GitHub authentication successful")

	// 2. Exchange for Copilot Token
	copilotTokenResp, err := a.GetCopilotToken(ctx, githubToken)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain Copilot token: %w", err)
	}
	log.Info("Copilot token obtained successfully")

	// 3. Get User Info (best effort)
	userInfo, err := a.GetGitHubUser(ctx, githubToken)
	if err != nil {
		log.Warnf("Failed to get user info: %v", err)
		userInfo = &GitHubUserResponse{}
	}
	if userInfo == nil {
		userInfo = &GitHubUserResponse{}
	}

	// 4. Build Storage
	storage := &CopilotTokenStorage{
		GitHubToken:        githubToken,
		CopilotToken:       copilotTokenResp.Token,
		CopilotTokenExpiry: time.Unix(copilotTokenResp.ExpiresAt, 0).Format(time.RFC3339),
		AccountType:        string(accountType),
		Username:           userInfo.Login,
		Email:              userInfo.Email,
		RefreshIn:          copilotTokenResp.RefreshIn,
		Type:               "copilot",
		LastRefresh:        time.Now().Format(time.RFC3339),
	}

	if userInfo.Login != "" {
		log.Infof("Logged in as %s", userInfo.Login)
	}

	// 5. Generate Filename
	filename := fmt.Sprintf("copilot_%s_%s.json", accountType, userInfo.Login)

	return &AuthResult{Storage: storage, SuggestedFilename: filename}, nil
}
