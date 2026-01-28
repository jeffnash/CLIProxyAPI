package copilot

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// CopilotTokenStorage stores authentication tokens for GitHub Copilot API.
// It maintains both the GitHub OAuth token and the Copilot-specific token,
// along with metadata for token refresh and user identification.
//
// Note on AccountType: This field is used to persist the account type to disk and
// to seed coreauth.Auth.Attributes["account_type"] during login. At runtime, executor
// logic should read from Attributes["account_type"], not from this storage field directly.
// See sdk/auth/copilot.go for the canonical source of truth on precedence and runtime contracts.
type CopilotTokenStorage struct {
	// GitHubToken is the OAuth access token from GitHub device code flow.
	GitHubToken string `json:"github_token"`

	// CopilotToken is the bearer token for Copilot API requests.
	// Note: marked as "-" to prevent persistence to disk.
	CopilotToken string `json:"-"`

	// CopilotTokenExpiry is the RFC3339 timestamp when the Copilot token expires.
	// Persisted to disk to enable proper refresh scheduling after reload.
	CopilotTokenExpiry string `json:"copilot_token_expiry,omitempty"`

	// RefreshIn is the number of seconds after which the token should be refreshed.
	// Note: marked as "-" to prevent persistence to disk.
	RefreshIn int `json:"-"`

	// AccountType is the Copilot subscription type (individual, business, enterprise).
	// This is persisted for storage but Attributes["account_type"] is authoritative at runtime.
	AccountType string `json:"account_type"`

	// Email is the GitHub account email address.
	Email string `json:"email"`

	// Username is the GitHub username.
	Username string `json:"username"`

	// LastRefresh is the RFC3339 timestamp of the last token refresh.
	// Persisted to disk to track refresh history after reload.
	LastRefresh string `json:"last_refresh,omitempty"`

	// ExpiresAt is the RFC3339 timestamp when the Copilot token expires.
	// Standard field used by the refresh scheduler for expiry-based refresh decisions.
	ExpiresAt string `json:"expires_at,omitempty"`

	// Type indicates the authentication provider type, always "copilot" for this storage.
	Type string `json:"type"`
}

// SaveTokenToFile serializes the Copilot token storage to a JSON file.
// This method creates the necessary directory structure and writes the token
// data in JSON format to the specified file path for persistent storage.
// Uses atomic write to prevent race conditions with file watchers.
//
// Parameters:
//   - authFilePath: The full path where the token file should be saved
//
// Returns:
//   - error: An error if the operation fails, nil otherwise
func (ts *CopilotTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = "copilot"

	data, err := json.MarshalIndent(ts, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal token: %w", err)
	}

	// Append newline for consistency with encoder behavior
	data = append(data, '\n')

	// Use atomic write to prevent race conditions with file watcher
	if err = util.AtomicWriteFile(authFilePath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write token file: %w", err)
	}
	return nil
}
