package handlers

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestGetRequestDetails_PreservesSuffix(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()

	modelRegistry.RegisterClient("test-request-details-gemini", "gemini", []*registry.ModelInfo{
		{ID: "gemini-2.5-pro", Created: now + 30},
		{ID: "gemini-2.5-flash", Created: now + 25},
	})
	modelRegistry.RegisterClient("test-request-details-openai", "openai", []*registry.ModelInfo{
		{ID: "gpt-5.2", Created: now + 20},
	})
	modelRegistry.RegisterClient("test-request-details-claude", "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-5", Created: now + 5},
	})

	// Ensure cleanup of all test registrations.
	clientIDs := []string{
		"test-request-details-gemini",
		"test-request-details-openai",
		"test-request-details-claude",
	}
	for _, clientID := range clientIDs {
		id := clientID
		t.Cleanup(func() {
			modelRegistry.UnregisterClient(id)
		})
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	tests := []struct {
		name          string
		inputModel    string
		wantProviders []string
		wantModel     string
		wantErr       bool
	}{
		{
			name:          "numeric suffix preserved",
			inputModel:    "gemini-2.5-pro(8192)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-pro(8192)",
			wantErr:       false,
		},
		{
			name:          "level suffix preserved",
			inputModel:    "gpt-5.2(high)",
			wantProviders: []string{"openai"},
			wantModel:     "gpt-5.2(high)",
			wantErr:       false,
		},
		{
			name:          "no suffix unchanged",
			inputModel:    "claude-sonnet-4-5",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5",
			wantErr:       false,
		},
		{
			name:          "unknown model with suffix",
			inputModel:    "unknown-model(8192)",
			wantProviders: nil,
			wantModel:     "",
			wantErr:       true,
		},
		{
			name:          "auto suffix resolved",
			inputModel:    "auto(high)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-pro(high)",
			wantErr:       false,
		},
		{
			name:          "special suffix none preserved",
			inputModel:    "gemini-2.5-flash(none)",
			wantProviders: []string{"gemini"},
			wantModel:     "gemini-2.5-flash(none)",
			wantErr:       false,
		},
		{
			name:          "special suffix auto preserved",
			inputModel:    "claude-sonnet-4-5(auto)",
			wantProviders: []string{"claude"},
			wantModel:     "claude-sonnet-4-5(auto)",
			wantErr:       false,
		},
	}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				providers, model, _, errMsg := handler.getRequestDetails(tt.inputModel)
				if (errMsg != nil) != tt.wantErr {
					t.Fatalf("getRequestDetails() error = %v, wantErr %v", errMsg, tt.wantErr)
				}
				if errMsg != nil {
				return
			}
			if !reflect.DeepEqual(providers, tt.wantProviders) {
				t.Fatalf("getRequestDetails() providers = %v, want %v", providers, tt.wantProviders)
			}
			if model != tt.wantModel {
				t.Fatalf("getRequestDetails() model = %v, want %v", model, tt.wantModel)
			}
		})
	}
}

func TestGetRequestDetails_TemperatureSuffix(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	now := time.Now().Unix()

	modelRegistry.RegisterClient("test-temp-gemini", "gemini", []*registry.ModelInfo{
		{ID: "gemini-2.5-pro", Created: now + 30},
	})
	modelRegistry.RegisterClient("test-temp-openai", "openai", []*registry.ModelInfo{
		{ID: "gpt-5.2", Created: now + 20},
		{ID: "gpt-4o", Created: now + 15},
	})
	modelRegistry.RegisterClient("test-temp-claude", "claude", []*registry.ModelInfo{
		{ID: "claude-sonnet-4-5", Created: now + 5},
	})

	clientIDs := []string{
		"test-temp-gemini",
		"test-temp-openai",
		"test-temp-claude",
	}
	for _, clientID := range clientIDs {
		id := clientID
		t.Cleanup(func() {
			modelRegistry.UnregisterClient(id)
		})
	}

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	tests := []struct {
		name          string
		inputModel    string
		wantModel     string
		wantTemp      float64
		wantHasTemp   bool
		wantProviders []string
		wantErr       bool
	}{
		{
			name:          "temp suffix stripped from claude model",
			inputModel:    "claude-sonnet-4-5-temp-0.7",
			wantModel:     "claude-sonnet-4-5",
			wantTemp:      0.7,
			wantHasTemp:   true,
			wantProviders: []string{"claude"},
		},
		{
			name:          "temp suffix stripped from gemini model",
			inputModel:    "gemini-2.5-pro-temp-1.2",
			wantModel:     "gemini-2.5-pro",
			wantTemp:      1.2,
			wantHasTemp:   true,
			wantProviders: []string{"gemini"},
		},
		{
			name:          "temp suffix with thinking suffix preserved",
			inputModel:    "claude-sonnet-4-5-temp-0.7(16384)",
			wantModel:     "claude-sonnet-4-5(16384)",
			wantTemp:      0.7,
			wantHasTemp:   true,
			wantProviders: []string{"claude"},
		},
		{
			name:          "temp suffix with thinking level preserved",
			inputModel:    "gemini-2.5-pro-temp-0.5(high)",
			wantModel:     "gemini-2.5-pro(high)",
			wantTemp:      0.5,
			wantHasTemp:   true,
			wantProviders: []string{"gemini"},
		},
		{
			name:          "gpt-5 model with temp - parsed but GPT5 check is at executor level",
			inputModel:    "gpt-5.2-temp-0.5",
			wantModel:     "gpt-5.2",
			wantTemp:      0.5,
			wantHasTemp:   true,
			wantProviders: []string{"openai"},
		},
		{
			name:          "no temp suffix - no metadata",
			inputModel:    "claude-sonnet-4-5",
			wantModel:     "claude-sonnet-4-5",
			wantHasTemp:   false,
			wantProviders: []string{"claude"},
		},
		{
			name:          "only thinking suffix - no temp metadata",
			inputModel:    "gemini-2.5-pro(8192)",
			wantModel:     "gemini-2.5-pro(8192)",
			wantHasTemp:   false,
			wantProviders: []string{"gemini"},
		},
		{
			name:          "gpt-4o with temp suffix",
			inputModel:    "gpt-4o-temp-0.3",
			wantModel:     "gpt-4o",
			wantTemp:      0.3,
			wantHasTemp:   true,
			wantProviders: []string{"openai"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers, model, metadata, errMsg := handler.getRequestDetails(tt.inputModel)
			if (errMsg != nil) != tt.wantErr {
				t.Fatalf("getRequestDetails(%q) error = %v, wantErr %v", tt.inputModel, errMsg, tt.wantErr)
			}
			if errMsg != nil {
				return
			}
			if !reflect.DeepEqual(providers, tt.wantProviders) {
				t.Errorf("providers = %v, want %v", providers, tt.wantProviders)
			}
			if model != tt.wantModel {
				t.Errorf("model = %q, want %q", model, tt.wantModel)
			}

			// Check temperature metadata
			if tt.wantHasTemp {
				if metadata == nil {
					t.Fatal("expected metadata to be non-nil when temperature suffix present")
				}
				raw, ok := metadata[cliproxyexecutor.TemperatureSuffixMetadataKey]
				if !ok {
					t.Fatal("expected temperature_suffix key in metadata")
				}
				temp, ok := raw.(float64)
				if !ok {
					t.Fatalf("expected temperature to be float64, got %T", raw)
				}
				if math.Abs(temp-tt.wantTemp) > 1e-9 {
					t.Errorf("temperature = %v, want %v", temp, tt.wantTemp)
				}
			} else {
				if metadata != nil {
					if _, ok := metadata[cliproxyexecutor.TemperatureSuffixMetadataKey]; ok {
						t.Error("expected no temperature_suffix in metadata")
					}
				}
			}
		})
	}
}
