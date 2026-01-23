package handlers

import (
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

func TestGetRequestDetails_ChutesPrefixRouting(t *testing.T) {
	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, coreauth.NewManager(nil, nil, nil))

	providers, model, metadata, err := handler.getRequestDetails("chutes-gpt-4.1")
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

