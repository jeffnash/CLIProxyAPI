package claude

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
)

func TestClaudeSSE_ErrorEventHasNonEmptyData(t *testing.T) {
	h := &ClaudeCodeAPIHandler{}

	errMsg := &interfaces.ErrorMessage{Error: errors.New("boom")}
	resp := h.toClaudeError(errMsg)
	errorBytes, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	wire := "event: error\ndata: " + string(errorBytes) + "\n\n"
	lines := strings.Split(wire, "\n")

	foundData := false
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			foundData = true
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" {
				t.Fatalf("unexpected empty data line: %q", wire)
			}
		}
	}
	if !foundData {
		t.Fatalf("expected a data line: %q", wire)
	}
}
