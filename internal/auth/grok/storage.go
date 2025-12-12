package grok

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
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
func (g *GrokTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	g.Type = "grok"
	g.SSOToken = NormalizeSSOToken(g.SSOToken)

	if err := os.MkdirAll(filepath.Dir(authFilePath), 0o700); err != nil {
		return fmt.Errorf("failed to create auth directory: %w", err)
	}

	f, err := os.Create(authFilePath)
	if err != nil {
		return fmt.Errorf("failed to create auth file: %w", err)
	}
	defer f.Close()

	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")

	if err := encoder.Encode(g); err != nil {
		return fmt.Errorf("failed to encode grok token: %w", err)
	}

	return nil
}
