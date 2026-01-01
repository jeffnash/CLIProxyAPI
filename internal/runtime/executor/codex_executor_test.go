package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestResolveCodexAlias(t *testing.T) {
	tests := []struct {
		name          string
		modelName     string
		wantBaseModel string
		wantEffort    string
		wantOk        bool
	}{
		// GPT-5 base aliases
		{
			name:          "gpt-5-minimal",
			modelName:     "gpt-5-minimal",
			wantBaseModel: "gpt-5",
			wantEffort:    "minimal",
			wantOk:        true,
		},
		{
			name:          "gpt-5-low",
			modelName:     "gpt-5-low",
			wantBaseModel: "gpt-5",
			wantEffort:    "low",
			wantOk:        true,
		},
		{
			name:          "gpt-5-medium",
			modelName:     "gpt-5-medium",
			wantBaseModel: "gpt-5",
			wantEffort:    "medium",
			wantOk:        true,
		},
		{
			name:          "gpt-5-high",
			modelName:     "gpt-5-high",
			wantBaseModel: "gpt-5",
			wantEffort:    "high",
			wantOk:        true,
		},
		// GPT-5-codex aliases
		{
			name:          "gpt-5-codex-low",
			modelName:     "gpt-5-codex-low",
			wantBaseModel: "gpt-5-codex",
			wantEffort:    "low",
			wantOk:        true,
		},
		{
			name:          "gpt-5-codex-high",
			modelName:     "gpt-5-codex-high",
			wantBaseModel: "gpt-5-codex",
			wantEffort:    "high",
			wantOk:        true,
		},
		// GPT-5.1 aliases
		{
			name:          "gpt-5.1-none",
			modelName:     "gpt-5.1-none",
			wantBaseModel: "gpt-5.1",
			wantEffort:    "none",
			wantOk:        true,
		},
		{
			name:          "gpt-5.1-high",
			modelName:     "gpt-5.1-high",
			wantBaseModel: "gpt-5.1",
			wantEffort:    "high",
			wantOk:        true,
		},
		// GPT-5.1-codex-max aliases
		{
			name:          "gpt-5.1-codex-max-xhigh",
			modelName:     "gpt-5.1-codex-max-xhigh",
			wantBaseModel: "gpt-5.1-codex-max",
			wantEffort:    "xhigh",
			wantOk:        true,
		},
		// GPT-5.2 aliases
		{
			name:          "gpt-5.2-xhigh",
			modelName:     "gpt-5.2-xhigh",
			wantBaseModel: "gpt-5.2",
			wantEffort:    "xhigh",
			wantOk:        true,
		},
		{
			name:          "gpt-5.2-codex-xhigh",
			modelName:     "gpt-5.2-codex-xhigh",
			wantBaseModel: "gpt-5.2-codex",
			wantEffort:    "xhigh",
			wantOk:        true,
		},
		// Non-alias models should return false
		{
			name:          "base gpt-5 (not an alias)",
			modelName:     "gpt-5",
			wantBaseModel: "",
			wantEffort:    "",
			wantOk:        false,
		},
		{
			name:          "base gpt-5-codex (not an alias)",
			modelName:     "gpt-5-codex",
			wantBaseModel: "",
			wantEffort:    "",
			wantOk:        false,
		},
		{
			name:          "claude model (not codex)",
			modelName:     "claude-sonnet-4",
			wantBaseModel: "",
			wantEffort:    "",
			wantOk:        false,
		},
		{
			name:          "empty string",
			modelName:     "",
			wantBaseModel: "",
			wantEffort:    "",
			wantOk:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBaseModel, gotEffort, gotOk := resolveCodexAlias(tt.modelName)
			if gotBaseModel != tt.wantBaseModel {
				t.Errorf("resolveCodexAlias(%q) baseModel = %q, want %q", tt.modelName, gotBaseModel, tt.wantBaseModel)
			}
			if gotEffort != tt.wantEffort {
				t.Errorf("resolveCodexAlias(%q) effort = %q, want %q", tt.modelName, gotEffort, tt.wantEffort)
			}
			if gotOk != tt.wantOk {
				t.Errorf("resolveCodexAlias(%q) ok = %v, want %v", tt.modelName, gotOk, tt.wantOk)
			}
		})
	}
}

func TestSetReasoningEffortByAlias(t *testing.T) {
	tests := []struct {
		name       string
		payload    []byte
		baseModel  string
		effort     string
		wantModel  string
		wantEffort string
	}{
		{
			name:       "set model and effort",
			payload:    []byte(`{}`),
			baseModel:  "gpt-5",
			effort:     "high",
			wantModel:  "gpt-5",
			wantEffort: "high",
		},
		{
			name:       "overwrite existing model",
			payload:    []byte(`{"model": "gpt-5-high"}`),
			baseModel:  "gpt-5",
			effort:     "high",
			wantModel:  "gpt-5",
			wantEffort: "high",
		},
		{
			name:       "effort is lowercased",
			payload:    []byte(`{}`),
			baseModel:  "gpt-5.1-codex-max",
			effort:     "XHIGH",
			wantModel:  "gpt-5.1-codex-max",
			wantEffort: "xhigh",
		},
		{
			name:       "effort is trimmed",
			payload:    []byte(`{}`),
			baseModel:  "gpt-5",
			effort:     "  medium  ",
			wantModel:  "gpt-5",
			wantEffort: "medium",
		},
		{
			name:       "empty effort is not set",
			payload:    []byte(`{}`),
			baseModel:  "gpt-5",
			effort:     "",
			wantModel:  "gpt-5",
			wantEffort: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := setReasoningEffortByAlias(tt.payload, tt.baseModel, tt.effort)
			gotModel := gjson.GetBytes(result, "model").String()
			gotEffort := gjson.GetBytes(result, "reasoning.effort").String()

			if gotModel != tt.wantModel {
				t.Errorf("setReasoningEffortByAlias() model = %q, want %q", gotModel, tt.wantModel)
			}
			if gotEffort != tt.wantEffort {
				t.Errorf("setReasoningEffortByAlias() reasoning.effort = %q, want %q", gotEffort, tt.wantEffort)
			}
		})
	}
}

func TestTokenizerForCodexModel(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		wantError bool
	}{
		{
			name:      "gpt-5 model",
			model:     "gpt-5",
			wantError: false,
		},
		{
			name:      "gpt-5-codex model",
			model:     "gpt-5-codex",
			wantError: false,
		},
		{
			name:      "gpt-5.1 model",
			model:     "gpt-5.1",
			wantError: false,
		},
		{
			name:      "gpt-5.2-codex model",
			model:     "gpt-5.2-codex",
			wantError: false,
		},
		{
			name:      "gpt-4o model",
			model:     "gpt-4o",
			wantError: false,
		},
		{
			name:      "gpt-4.1 model",
			model:     "gpt-4.1",
			wantError: false,
		},
		{
			name:      "gpt-3.5-turbo model",
			model:     "gpt-3.5-turbo",
			wantError: false,
		},
		{
			name:      "empty model defaults to cl100k_base",
			model:     "",
			wantError: false,
		},
		{
			name:      "unknown model defaults to cl100k_base",
			model:     "unknown-model",
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			enc, err := tokenizerForCodexModel(tt.model)
			if tt.wantError {
				if err == nil {
					t.Errorf("tokenizerForCodexModel(%q) expected error, got nil", tt.model)
				}
			} else {
				if err != nil {
					t.Errorf("tokenizerForCodexModel(%q) unexpected error: %v", tt.model, err)
				}
				if enc == nil {
					t.Errorf("tokenizerForCodexModel(%q) returned nil encoder", tt.model)
				}
			}
		})
	}
}
