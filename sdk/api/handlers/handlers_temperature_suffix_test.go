package handlers

import (
	"math"
	"testing"
)

func TestParseTemperatureSuffix(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantModel string
		wantTemp  float64
		wantHas   bool
	}{
		{
			name:      "basic decimal temperature",
			input:     "claude-sonnet-4-5-temp-0.7",
			wantModel: "claude-sonnet-4-5",
			wantTemp:  0.7,
			wantHas:   true,
		},
		{
			name:      "integer temperature",
			input:     "gemini-2.5-pro-temp-1",
			wantModel: "gemini-2.5-pro",
			wantTemp:  1.0,
			wantHas:   true,
		},
		{
			name:      "zero temperature",
			input:     "claude-sonnet-4-5-temp-0",
			wantModel: "claude-sonnet-4-5",
			wantTemp:  0.0,
			wantHas:   true,
		},
		{
			name:      "high temperature",
			input:     "gemini-2.5-pro-temp-2.0",
			wantModel: "gemini-2.5-pro",
			wantTemp:  2.0,
			wantHas:   true,
		},
		{
			name:      "with thinking suffix - numeric",
			input:     "claude-sonnet-4-5-temp-0.7(16384)",
			wantModel: "claude-sonnet-4-5(16384)",
			wantTemp:  0.7,
			wantHas:   true,
		},
		{
			name:      "with thinking suffix - level",
			input:     "model-temp-0.5(high)",
			wantModel: "model(high)",
			wantTemp:  0.5,
			wantHas:   true,
		},
		{
			name:      "with thinking suffix - auto",
			input:     "gemini-2.5-pro-temp-1.2(auto)",
			wantModel: "gemini-2.5-pro(auto)",
			wantTemp:  1.2,
			wantHas:   true,
		},
		{
			name:      "no suffix",
			input:     "claude-sonnet-4-5",
			wantModel: "claude-sonnet-4-5",
			wantTemp:  0,
			wantHas:   false,
		},
		{
			name:      "only thinking suffix",
			input:     "gemini-2.5-pro(8192)",
			wantModel: "gemini-2.5-pro(8192)",
			wantTemp:  0,
			wantHas:   false,
		},
		{
			name:      "no value after -temp-",
			input:     "model-temp-",
			wantModel: "model-temp-",
			wantTemp:  0,
			wantHas:   false,
		},
		{
			name:      "non-numeric value",
			input:     "model-temp-abc",
			wantModel: "model-temp-abc",
			wantTemp:  0,
			wantHas:   false,
		},
		{
			name:      "temp at the start only - invalid",
			input:     "-temp-0.5",
			wantModel: "-temp-0.5",
			wantTemp:  0,
			wantHas:   false,
		},
		{
			name:      "gpt-5 model with temp suffix",
			input:     "gpt-5.2-temp-0.5",
			wantModel: "gpt-5.2",
			wantTemp:  0.5,
			wantHas:   true,
		},
		{
			name:      "gpt-5-mini with temp suffix",
			input:     "gpt-5-mini-temp-0.7",
			wantModel: "gpt-5-mini",
			wantTemp:  0.7,
			wantHas:   true,
		},
		{
			name:      "decimal without leading zero",
			input:     "model-temp-1.5",
			wantModel: "model",
			wantTemp:  1.5,
			wantHas:   true,
		},
		{
			name:      "copilot prefix already stripped",
			input:     "gpt-4o-temp-0.3",
			wantModel: "gpt-4o",
			wantTemp:  0.3,
			wantHas:   true,
		},
		{
			name:      "temp in middle of name - not matched",
			input:     "my-temp-0.5-model",
			wantModel: "my-temp-0.5-model",
			wantTemp:  0,
			wantHas:   false,
		},
		{
			name:      "empty string",
			input:     "",
			wantModel: "",
			wantTemp:  0,
			wantHas:   false,
		},
		{
			name:      "just temp suffix",
			input:     "a-temp-0.5",
			wantModel: "a",
			wantTemp:  0.5,
			wantHas:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotModel, gotTemp, gotHas := parseTemperatureSuffix(tt.input)
			if gotModel != tt.wantModel {
				t.Errorf("parseTemperatureSuffix(%q) model = %q, want %q", tt.input, gotModel, tt.wantModel)
			}
			if gotHas != tt.wantHas {
				t.Errorf("parseTemperatureSuffix(%q) hasTemp = %v, want %v", tt.input, gotHas, tt.wantHas)
			}
			if gotHas && math.Abs(gotTemp-tt.wantTemp) > 1e-9 {
				t.Errorf("parseTemperatureSuffix(%q) temp = %v, want %v", tt.input, gotTemp, tt.wantTemp)
			}
		})
	}
}

func TestParseTemperatureSuffix_RoundTrip(t *testing.T) {
	// Verify that stripping temp suffix and re-attaching thinking suffix works correctly.
	model, temp, has := parseTemperatureSuffix("claude-sonnet-4-5-temp-0.7(16384)")
	if !has {
		t.Fatal("expected hasTemp=true")
	}
	if model != "claude-sonnet-4-5(16384)" {
		t.Fatalf("model = %q, want %q", model, "claude-sonnet-4-5(16384)")
	}
	if math.Abs(temp-0.7) > 1e-9 {
		t.Fatalf("temp = %v, want 0.7", temp)
	}
}
