package executor

import "testing"

func TestResolveCopilotAlias_GPT54(t *testing.T) {
	tests := []struct {
		model      string
		wantModel  string
		wantEffort string
	}{
		{model: "gpt-5.4-low", wantModel: "gpt-5.4", wantEffort: "low"},
		{model: "gpt-5.4-medium", wantModel: "gpt-5.4", wantEffort: "medium"},
		{model: "gpt-5.4-high", wantModel: "gpt-5.4", wantEffort: "high"},
		{model: "gpt-5.4-xhigh", wantModel: "gpt-5.4", wantEffort: "xhigh"},
	}

	for _, tt := range tests {
		gotModel, gotEffort, ok := resolveCopilotAlias(tt.model)
		if !ok {
			t.Fatalf("resolveCopilotAlias(%q) returned ok=false", tt.model)
		}
		if gotModel != tt.wantModel || gotEffort != tt.wantEffort {
			t.Fatalf("resolveCopilotAlias(%q) = (%q, %q), want (%q, %q)", tt.model, gotModel, gotEffort, tt.wantModel, tt.wantEffort)
		}
	}
}
