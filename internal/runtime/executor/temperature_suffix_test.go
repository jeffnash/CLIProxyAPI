package executor

import (
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestIsGPT5Model(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"gpt-5.1", true},
		{"gpt-5.2", true},
		{"gpt-5.2-codex", true},
		{"gpt-5.3-codex", true},
		{"GPT-5", true},
		{"GPT-5.2", true},
		{"gpt-5(high)", true},
		{"gpt-5.1-codex-max(low)", true},
		{"gpt-4o", false},
		{"gpt-4o-mini", false},
		{"claude-sonnet-4-5", false},
		{"gemini-2.5-pro", false},
		{"o3-pro", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			got := isGPT5Model(tt.model)
			if got != tt.want {
				t.Errorf("isGPT5Model(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestApplyTemperatureSuffix_NoMetadata(t *testing.T) {
	body := []byte(`{"model":"test","messages":[]}`)
	opts := cliproxyexecutor.Options{}
	result := applyTemperatureSuffix(body, "test", opts, "openai")
	if string(result) != string(body) {
		t.Errorf("expected body unchanged, got %s", string(result))
	}
}

func TestApplyTemperatureSuffix_NoTempKey(t *testing.T) {
	body := []byte(`{"model":"test","messages":[]}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{"other_key": "value"},
	}
	result := applyTemperatureSuffix(body, "test", opts, "openai")
	if string(result) != string(body) {
		t.Errorf("expected body unchanged, got %s", string(result))
	}
}

func TestApplyTemperatureSuffix_OpenAI(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[]}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.7),
		},
	}
	result := applyTemperatureSuffix(body, "claude-sonnet-4-5", opts, "openai")

	temp := gjson.GetBytes(result, "temperature")
	if !temp.Exists() {
		t.Fatal("expected temperature field to exist")
	}
	if temp.Float() != 0.7 {
		t.Errorf("temperature = %v, want 0.7", temp.Float())
	}
}

func TestApplyTemperatureSuffix_Claude(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","messages":[]}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.3),
		},
	}
	result := applyTemperatureSuffix(body, "claude-sonnet-4-5", opts, "claude")

	temp := gjson.GetBytes(result, "temperature")
	if !temp.Exists() {
		t.Fatal("expected temperature field to exist")
	}
	if temp.Float() != 0.3 {
		t.Errorf("temperature = %v, want 0.3", temp.Float())
	}
}

func TestApplyTemperatureSuffix_Gemini(t *testing.T) {
	body := []byte(`{"contents":[],"generationConfig":{}}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(1.2),
		},
	}
	result := applyTemperatureSuffix(body, "gemini-2.5-pro", opts, "gemini")

	temp := gjson.GetBytes(result, "generationConfig.temperature")
	if !temp.Exists() {
		t.Fatal("expected generationConfig.temperature field to exist")
	}
	if temp.Float() != 1.2 {
		t.Errorf("temperature = %v, want 1.2", temp.Float())
	}

	// Top-level temperature should NOT exist for Gemini
	topTemp := gjson.GetBytes(result, "temperature")
	if topTemp.Exists() {
		t.Error("expected no top-level temperature for Gemini provider")
	}
}

func TestApplyTemperatureSuffix_GeminiCLI(t *testing.T) {
	body := []byte(`{"request":{"contents":[],"generationConfig":{}}}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.9),
		},
	}
	result := applyTemperatureSuffix(body, "gemini-2.5-pro", opts, "gemini-cli")

	temp := gjson.GetBytes(result, "request.generationConfig.temperature")
	if !temp.Exists() {
		t.Fatal("expected request.generationConfig.temperature field to exist")
	}
	if temp.Float() != 0.9 {
		t.Errorf("temperature = %v, want 0.9", temp.Float())
	}
}

func TestApplyTemperatureSuffix_Antigravity(t *testing.T) {
	body := []byte(`{"request":{"contents":[],"generationConfig":{}}}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.5),
		},
	}
	result := applyTemperatureSuffix(body, "gemini-2.5-pro", opts, "antigravity")

	temp := gjson.GetBytes(result, "request.generationConfig.temperature")
	if !temp.Exists() {
		t.Fatal("expected request.generationConfig.temperature field to exist")
	}
	if temp.Float() != 0.5 {
		t.Errorf("temperature = %v, want 0.5", temp.Float())
	}
}

func TestApplyTemperatureSuffix_GPT5_Skipped(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"gpt-5", "gpt-5"},
		{"gpt-5-mini", "gpt-5-mini"},
		{"gpt-5.1", "gpt-5.1"},
		{"gpt-5.2", "gpt-5.2"},
		{"gpt-5.2-codex", "gpt-5.2-codex"},
		{"gpt-5.3-codex", "gpt-5.3-codex"},
		{"GPT-5 case insensitive", "GPT-5"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tt.model + `","messages":[]}`)
			opts := cliproxyexecutor.Options{
				Metadata: map[string]any{
					cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.7),
				},
			}
			result := applyTemperatureSuffix(body, tt.model, opts, "openai")

			temp := gjson.GetBytes(result, "temperature")
			if temp.Exists() {
				t.Errorf("expected temperature NOT to be set for gpt-5 model %q, but got %v", tt.model, temp.Float())
			}
		})
	}
}

func TestApplyTemperatureSuffix_NonGPT5_Applied(t *testing.T) {
	tests := []struct {
		name  string
		model string
	}{
		{"gpt-4o", "gpt-4o"},
		{"gpt-4o-mini", "gpt-4o-mini"},
		{"claude-sonnet-4-5", "claude-sonnet-4-5"},
		{"gemini-2.5-pro", "gemini-2.5-pro"},
		{"o3-pro", "o3-pro"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tt.model + `","messages":[]}`)
			opts := cliproxyexecutor.Options{
				Metadata: map[string]any{
					cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.7),
				},
			}
			result := applyTemperatureSuffix(body, tt.model, opts, "openai")

			temp := gjson.GetBytes(result, "temperature")
			if !temp.Exists() {
				t.Errorf("expected temperature to be set for model %q", tt.model)
			}
			if temp.Float() != 0.7 {
				t.Errorf("temperature = %v, want 0.7", temp.Float())
			}
		})
	}
}

func TestApplyTemperatureSuffix_OverridesExisting(t *testing.T) {
	body := []byte(`{"model":"test","messages":[],"temperature":0.9}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.3),
		},
	}
	result := applyTemperatureSuffix(body, "test", opts, "openai")

	temp := gjson.GetBytes(result, "temperature")
	if !temp.Exists() {
		t.Fatal("expected temperature field to exist")
	}
	if temp.Float() != 0.3 {
		t.Errorf("temperature = %v, want 0.3 (should override existing 0.9)", temp.Float())
	}
}

func TestApplyTemperatureSuffix_ZeroTemperature(t *testing.T) {
	body := []byte(`{"model":"test","messages":[]}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.0),
		},
	}
	result := applyTemperatureSuffix(body, "test", opts, "openai")

	temp := gjson.GetBytes(result, "temperature")
	if !temp.Exists() {
		t.Fatal("expected temperature field to exist even for zero value")
	}
	if temp.Float() != 0.0 {
		t.Errorf("temperature = %v, want 0.0", temp.Float())
	}
}

func TestApplyTemperatureSuffix_WrongMetadataType(t *testing.T) {
	body := []byte(`{"model":"test","messages":[]}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: "not-a-float",
		},
	}
	result := applyTemperatureSuffix(body, "test", opts, "openai")

	// Should return body unchanged if metadata value is wrong type
	temp := gjson.GetBytes(result, "temperature")
	if temp.Exists() {
		t.Error("expected no temperature set for non-float64 metadata value")
	}
}

func TestApplyTemperatureSuffix_Codex(t *testing.T) {
	body := []byte(`{"model":"o3-pro","input":"test"}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.8),
		},
	}
	result := applyTemperatureSuffix(body, "o3-pro", opts, "codex")

	temp := gjson.GetBytes(result, "temperature")
	if !temp.Exists() {
		t.Fatal("expected temperature field for codex provider")
	}
	if temp.Float() != 0.8 {
		t.Errorf("temperature = %v, want 0.8", temp.Float())
	}
}

func TestApplyTemperatureSuffix_Copilot(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.5),
		},
	}
	// Copilot uses OpenAI format
	result := applyTemperatureSuffix(body, "gpt-4o", opts, "openai")

	temp := gjson.GetBytes(result, "temperature")
	if !temp.Exists() {
		t.Fatal("expected temperature field for copilot/openai provider")
	}
	if temp.Float() != 0.5 {
		t.Errorf("temperature = %v, want 0.5", temp.Float())
	}
}

func TestApplyTemperatureSuffix_Iflow(t *testing.T) {
	body := []byte(`{"model":"deepseek-r1","messages":[]}`)
	opts := cliproxyexecutor.Options{
		Metadata: map[string]any{
			cliproxyexecutor.TemperatureSuffixMetadataKey: float64(0.6),
		},
	}
	// iFlow uses OpenAI format
	result := applyTemperatureSuffix(body, "deepseek-r1", opts, "openai")

	temp := gjson.GetBytes(result, "temperature")
	if !temp.Exists() {
		t.Fatal("expected temperature field for iflow provider")
	}
	if temp.Float() != 0.6 {
		t.Errorf("temperature = %v, want 0.6", temp.Float())
	}
}
