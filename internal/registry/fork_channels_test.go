package registry

import "testing"

// TestForkOnlyModelChannelsPresent guards fork-only model sources that upstream
// does not carry. An upstream merge (or the catalog refresh script, which
// overwrites models.json) can silently drop these channels; the static
// accessors would then return no models while executors still exist.
func TestForkOnlyModelChannelsPresent(t *testing.T) {
	channels := map[string]func() []*ModelInfo{
		"qwen":    GetQwenModels,
		"iflow":   GetIFlowModels,
		"cursor":  GetCursorModels,
		"chutes":  GetChutesModels,
		"copilot": GetCopilotModels,
		"kiro":    GetKiroModels,
	}

	for name, get := range channels {
		models := get()
		if len(models) == 0 {
			t.Errorf("%s: no models returned; fork-only channel appears dropped", name)
			continue
		}
		for _, model := range models {
			if model == nil || model.ID == "" {
				t.Errorf("%s: model entry with empty ID: %+v", name, model)
			}
		}
	}
}
