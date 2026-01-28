package grok

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// GrokTokenStorage stores Grok authentication JWTs and status metadata.
type GrokTokenStorage struct {
	SSOToken              string `json:"sso_token"`
	CFClearance           string `json:"cf_clearance"`
	TokenType             string `json:"token_type"`
	Status                string `json:"status"`
	FailedCount           int    `json:"failed_count"`
	RemainingQueries      int    `json:"remaining_queries"`
	HeavyRemainingQueries int    `json:"heavy_remaining_queries"`
	Note                  string `json:"note"`
	Type                  string `json:"type"`
}

// SaveTokenToFile persists Grok token data to the provided path.
// Uses atomic write to prevent race conditions with file watchers.
func (g *GrokTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	g.Type = "grok"
	g.SSOToken = NormalizeSSOToken(g.SSOToken)

	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal grok token: %w", err)
	}

	// Append newline for consistency with encoder behavior
	data = append(data, '\n')

	// Use atomic write to prevent race conditions with file watcher
	if err = util.AtomicWriteFile(authFilePath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write grok token file: %w", err)
	}
	return nil
}
