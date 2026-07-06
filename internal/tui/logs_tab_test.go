package tui

import "testing"

func TestLogsTabIgnoresStaleGenerationMessages(t *testing.T) {
	model := newLogsTabModel(nil, nil)
	model, _ = model.Start()
	staleGeneration := model.generation
	model = model.Stop()

	updated, cmd := model.Update(logsTickMsg{generation: staleGeneration})
	if cmd != nil {
		t.Fatal("stale tick scheduled another command")
	}
	if updated.generation != model.generation {
		t.Fatalf("generation changed on stale tick: %d -> %d", model.generation, updated.generation)
	}

	updated, cmd = model.Update(logLineMsg{line: "stale", generation: staleGeneration})
	if cmd != nil {
		t.Fatal("stale log line scheduled another wait command")
	}
	if len(updated.lines) != 0 {
		t.Fatalf("stale log line was appended: %v", updated.lines)
	}
}
