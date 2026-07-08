package handlers

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestGetRequestDetails_ChutesPrefixRouting(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	providers, model, metadata, err := handler.getRequestDetailsWithOptions("chutes-gpt-4.1", false)
	if err != nil {
		t.Fatalf("getRequestDetails(): %v", err)
	}
	if len(providers) != 1 || providers[0] != "chutes" {
		t.Fatalf("providers=%v, want [chutes]", providers)
	}
	if model != "gpt-4.1" {
		t.Fatalf("model=%q, want %q", model, "gpt-4.1")
	}
	if metadata == nil {
		t.Fatalf("metadata=nil, want forced_provider=true")
	}
	if v, ok := metadata["forced_provider"].(bool); !ok || !v {
		t.Fatalf("metadata[forced_provider]=%v (type %T), want true", metadata["forced_provider"], metadata["forced_provider"])
	}
}

func TestGetRequestDetails_ManagedProviderPrefixRouting(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		ManagedProviders: []sdkconfig.ManagedProviderConfig{{
			Name:   "example-provider",
			Prefix: "example-",
		}},
	}, coreauth.NewManager(nil, nil, nil))

	providers, model, metadata, err := handler.getRequestDetailsWithOptions("example-glm-5.2", false)
	if err != nil {
		t.Fatalf("getRequestDetails(): %v", err)
	}
	if len(providers) != 1 || providers[0] != "example-provider" {
		t.Fatalf("providers=%v, want [example-provider]", providers)
	}
	if model != "glm-5.2" {
		t.Fatalf("model=%q, want %q", model, "glm-5.2")
	}
	if metadata == nil {
		t.Fatalf("metadata=nil, want forced_provider=true")
	}
	if v, ok := metadata["forced_provider"].(bool); !ok || !v {
		t.Fatalf("metadata[forced_provider]=%v (type %T), want true", metadata["forced_provider"], metadata["forced_provider"])
	}
}

func TestGetRequestDetails_ManagedProviderProtocolPrefixRouting(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{
		ManagedProviders: []sdkconfig.ManagedProviderConfig{{
			Name:   "example-provider",
			Prefix: "example-",
		}},
	}, coreauth.NewManager(nil, nil, nil))

	for _, tc := range []struct {
		requested string
		transport string
	}{
		{requested: "anthropic-example-glm-5.2", transport: "anthropic"},
		{requested: "openai-example-qwen3.7-max", transport: "openai"},
		{requested: "openai-responses-example-qwen3.7-max", transport: "openai-responses"},
		{requested: "openai-completions-example-qwen3.7-max", transport: "openai-completions"},
	} {
		providers, model, metadata, err := handler.getRequestDetailsWithOptions(tc.requested, false)
		if err != nil {
			t.Fatalf("getRequestDetails(%q): %v", tc.requested, err)
		}
		if len(providers) != 1 || providers[0] != "example-provider" {
			t.Fatalf("providers=%v, want [example-provider]", providers)
		}
		wantModel := "glm-5.2"
		if tc.transport != "anthropic" {
			wantModel = "qwen3.7-max"
		}
		if model != wantModel {
			t.Fatalf("model=%q, want %q", model, wantModel)
		}
		if metadata == nil {
			t.Fatalf("metadata=nil, want forced transport metadata")
		}
		if v, ok := metadata["forced_provider"].(bool); !ok || !v {
			t.Fatalf("metadata[forced_provider]=%v (type %T), want true", metadata["forced_provider"], metadata["forced_provider"])
		}
		if got := metadata[coreexecutor.ManagedProviderTransportMetadataKey]; got != tc.transport {
			t.Fatalf("metadata[%s]=%v, want %q", coreexecutor.ManagedProviderTransportMetadataKey, got, tc.transport)
		}
	}
}
