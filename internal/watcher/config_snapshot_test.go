package watcher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestApplyConfigSnapshotPreservesSettingsAndReplacesCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if errWrite := os.WriteFile(path, nil, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	configFor := func(name string) *config.Config {
		return &config.Config{
			AuthDir:          dir,
			RemoteManagement: config.RemoteManagement{SecretKey: "snapshot-secret"},
			OpenAICompatibility: []config.OpenAICompatibility{{
				Name: name, BaseURL: "https://compat.example/v1",
				APIKeyEntries: []config.OpenAICompatibilityAPIKey{{APIKey: "test-key"}},
				Models:        []config.OpenAICompatibilityModel{{Name: "test-model", Alias: name}},
			}},
		}
	}
	var applied *config.Config
	w := &Watcher{
		configPath: path,
		authDir:    dir,
		config:     configFor("old"),
		authQueue:  make(chan AuthUpdate, 4),
		reloadCallback: func(cfg *config.Config) {
			applied = cfg
		},
	}
	w.refreshAuthState(false)
	if len(w.currentAuths) != 1 {
		t.Fatalf("initial credentials=%d, want 1", len(w.currentAuths))
	}
	var oldID string
	for id := range w.currentAuths {
		oldID = id
	}
	w.pendingUpdates = nil
	w.pendingOrder = nil

	// The file may be truncated by the next save; the captured snapshot is complete.
	w.ApplyConfig(configFor("new"))
	if applied == nil || applied.RemoteManagement.SecretKey != "snapshot-secret" || applied.OpenAICompatibility[0].Name != "new" {
		t.Fatal("snapshot settings were not applied")
	}
	if update := w.pendingUpdates[oldID]; update.Action != AuthUpdateActionDelete {
		t.Fatalf("removed credential update=%+v", update)
	}
	if len(w.currentAuths) != 1 {
		t.Fatalf("current credentials=%d, want 1", len(w.currentAuths))
	}
	for id := range w.currentAuths {
		if id == oldID || w.pendingUpdates[id].Action != AuthUpdateActionAdd {
			t.Fatal("replacement credential was not added")
		}
	}
}
