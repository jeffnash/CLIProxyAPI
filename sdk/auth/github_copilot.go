package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/auth/copilot"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/browser"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// GitHubCopilotAuthenticator implements the OAuth device flow login for GitHub Copilot.
type GitHubCopilotAuthenticator struct{}

// NewGitHubCopilotAuthenticator constructs a new GitHub Copilot authenticator.
func NewGitHubCopilotAuthenticator() Authenticator {
	return &GitHubCopilotAuthenticator{}
}

// Provider returns the provider key for Copilot.
func (GitHubCopilotAuthenticator) Provider() string {
	return "copilot"
}

// RefreshLead returns nil since GitHub OAuth tokens don't expire in the traditional sense.
// The token remains valid until the user revokes it or the Copilot subscription expires.
func (GitHubCopilotAuthenticator) RefreshLead() *time.Duration {
	return nil
}

// Login initiates the GitHub device flow authentication for Copilot access.
func (a GitHubCopilotAuthenticator) Login(ctx context.Context, cfg *config.Config, opts *LoginOptions) (*coreauth.Auth, error) {
	if cfg == nil {
		return nil, fmt.Errorf("cliproxy auth: configuration is required")
	}
	if opts == nil {
		opts = &LoginOptions{}
	}

	authSvc := copilot.NewCopilotAuth(cfg)

	// Use the Copilot authenticator flow; store this as "copilot" provider.
	fmt.Println("Starting GitHub Copilot authentication...")
	result, err := authSvc.PerformFullAuthWithFilename(ctx, copilot.AccountTypeIndividual, func(dc *copilot.DeviceCodeResponse) {
		fmt.Printf("\nTo authenticate, please visit: %s\n", dc.VerificationURI)
		fmt.Printf("And enter the code: %s\n\n", dc.UserCode)
		if !opts.NoBrowser {
			if browser.IsAvailable() {
				if errOpen := browser.OpenURL(dc.VerificationURI); errOpen != nil {
					log.Warnf("Failed to open browser automatically: %v", errOpen)
				}
			}
		}
		fmt.Println("Waiting for GitHub authorization...")
		fmt.Printf("(This will timeout in %d seconds if not authorized)\n", dc.ExpiresIn)
	})
	if err != nil {
		errMsg := copilot.GetUserFriendlyMessage(err)
		return nil, fmt.Errorf("github-copilot: %s", errMsg)
	}
	if result == nil || result.Storage == nil {
		return nil, fmt.Errorf("github-copilot: authentication failed: no token storage returned")
	}

	tokenStorage := result.Storage
	fileName := result.SuggestedFilename

	metadata := map[string]any{
		"email":                tokenStorage.Email,
		"username":             tokenStorage.Username,
		"account_type":         tokenStorage.AccountType,
		"copilot_token_expiry": tokenStorage.CopilotTokenExpiry,
		"github_token":         tokenStorage.GitHubToken,
		"copilot_token":        tokenStorage.CopilotToken,
		"type":                 "copilot",
		"timestamp":            time.Now().UnixMilli(),
	}

	fmt.Printf("\nGitHub Copilot authentication successful for user: %s\n", tokenStorage.Username)

	return &coreauth.Auth{
		ID:       fileName,
		Provider: a.Provider(),
		FileName: fileName,
		Label:    tokenStorage.Username,
		Storage:  tokenStorage,
		Metadata: metadata,
		Attributes: map[string]string{
			"account_type": tokenStorage.AccountType,
		},
	}, nil
}

// RefreshGitHubCopilotToken validates and returns the current token status.
// GitHub OAuth tokens don't need traditional refresh - we just validate they still work.
func RefreshGitHubCopilotToken(ctx context.Context, cfg *config.Config, storage *copilot.CopilotTokenStorage) error {
	if storage == nil || storage.GitHubToken == "" {
		return fmt.Errorf("no token available")
	}

	authSvc := copilot.NewCopilotAuth(cfg)

	// Validate the token can still get a Copilot API token
	_, err := authSvc.GetCopilotToken(ctx, storage.GitHubToken)
	if err != nil {
		return fmt.Errorf("token validation failed: %w", err)
	}

	return nil
}
