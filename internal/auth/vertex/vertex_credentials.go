// Package vertex provides token storage for Google Vertex AI Gemini via service account credentials.
// It serialises service account JSON into an auth file that is consumed by the runtime executor.
package vertex

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
)

// VertexCredentialStorage stores the service account JSON for Vertex AI access.
// The content is persisted verbatim under the "service_account" key, together with
// helper fields for project, location and email to improve logging and discovery.
type VertexCredentialStorage struct {
	// ServiceAccount holds the parsed service account JSON content.
	ServiceAccount map[string]any `json:"service_account"`

	// ProjectID is derived from the service account JSON (project_id).
	ProjectID string `json:"project_id"`

	// Email is the client_email from the service account JSON.
	Email string `json:"email"`

	// Location optionally sets a default region (e.g., us-central1) for Vertex endpoints.
	Location string `json:"location,omitempty"`

	// Type is the provider identifier stored alongside credentials. Always "vertex".
	Type string `json:"type"`
}

// SaveTokenToFile writes the credential payload to the given file path in JSON format.
// It ensures the parent directory exists and logs the operation for transparency.
// Uses atomic write to prevent race conditions with file watchers.
func (s *VertexCredentialStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	if s == nil {
		return fmt.Errorf("vertex credential: storage is nil")
	}
	if s.ServiceAccount == nil {
		return fmt.Errorf("vertex credential: service account content is empty")
	}
	// Ensure we tag the file with the provider type.
	s.Type = "vertex"

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("vertex credential: marshal failed: %w", err)
	}

	// Append newline for consistency with encoder behavior
	data = append(data, '\n')

	// Use atomic write to prevent race conditions with file watcher
	if err = util.AtomicWriteFile(authFilePath, data, 0o600); err != nil {
		return fmt.Errorf("vertex credential: write file failed: %w", err)
	}
	return nil
}
