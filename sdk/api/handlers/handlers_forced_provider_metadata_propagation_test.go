package handlers

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
)

type metadataAssertingExecutor struct {
	t *testing.T
}

func (e *metadataAssertingExecutor) Identifier() string { return "copilot" }

func (e *metadataAssertingExecutor) Execute(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.t.Helper()
	if opts.Metadata == nil {
		e.t.Fatalf("opts.Metadata=nil, want forced_provider=true")
	}
	v, ok := opts.Metadata["forced_provider"].(bool)
	if !ok || !v {
		e.t.Fatalf("opts.Metadata[forced_provider]=%v (type %T), want true", opts.Metadata["forced_provider"], opts.Metadata["forced_provider"])
	}
	return coreexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *metadataAssertingExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	ch := make(chan coreexecutor.StreamChunk)
	close(ch)
	return &coreexecutor.StreamResult{Chunks: ch}, nil
}

func (e *metadataAssertingExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *metadataAssertingExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, &coreauth.Error{Code: "not_implemented", Message: "CountTokens not implemented", HTTPStatus: http.StatusNotImplemented}
}

func (e *metadataAssertingExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, &coreauth.Error{Code: "not_implemented", Message: "HttpRequest not implemented", HTTPStatus: http.StatusNotImplemented}
}

func TestExecuteWithAuthManager_ForcedProviderMetadataPropagates(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(&metadataAssertingExecutor{t: t})

	// Register a minimal Copilot auth (forced_provider should bypass model registration checks).
	auth := &coreauth.Auth{
		ID:       "copilot-auth-1",
		Provider: "copilot",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "test@example.com"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register(): %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "gpt-5"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	if _, _, errMsg := handler.ExecuteWithAuthManager(context.Background(), "openai", "copilot-gpt-5", []byte(`{"model":"copilot-gpt-5"}`), ""); errMsg != nil {
		t.Fatalf("ExecuteWithAuthManager(): %v", errMsg)
	}
}
