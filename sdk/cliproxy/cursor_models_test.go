package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// RC3: cursor-api-key[].models are registered for /v1/models under their client-facing ALIASES (the alias
// is the model ID a client requests). Empty models -> nil (the caller falls back to registry.GetCursorModels).
func TestBuildCursorConfigModels(t *testing.T) {
	entry := &config.CursorKey{Models: []config.CursorModel{
		{Name: "composer-2.5", Alias: "big"},
		{Name: "composer-2.5-fast", Alias: "fast"},
	}}
	models := buildCursorConfigModels(entry)
	if len(models) != 2 {
		t.Fatalf("expected 2 registered models, got %d", len(models))
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
		if m.OwnedBy != "cursor" {
			t.Fatalf("model %s ownedBy=%q, want cursor", m.ID, m.OwnedBy)
		}
	}
	if !ids["big"] || !ids["fast"] {
		t.Fatalf("expected client-facing aliases registered as model IDs, got %v", ids)
	}
	if buildCursorConfigModels(&config.CursorKey{}) != nil {
		t.Fatalf("no configured models must return nil (fall back to registry defaults)")
	}
	if buildCursorConfigModels(nil) != nil {
		t.Fatalf("nil entry must return nil")
	}
}
