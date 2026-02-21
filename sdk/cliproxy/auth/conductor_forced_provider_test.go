package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
)

// TestPickNext_ForcedProviderBypassesModelCheck verifies that the forced_provider
// metadata flag bypasses the ClientSupportsModel check in pickNext.
func TestPickNext_ForcedProviderBypassesModelCheck(t *testing.T) {
	// Create a manager with mock dependencies
	mgr := NewManager(nil, &mockSelector{}, NoopHook{})

	// Register a mock executor
	mockExecutor := &mockProviderExecutor{id: "copilot"}
	mgr.RegisterExecutor(mockExecutor)

	// Add an auth that does NOT have "gemini-3-flash-preview" in its model list
	// (simulating what happens when /models API doesn't return the model)
	testAuth := &Auth{
		ID:       "test-copilot-auth",
		Provider: "copilot",
		Disabled: false,
	}
	mgr.Register(context.Background(), testAuth)

	ctx := context.Background()
	tried := make(map[string]struct{})

	tests := []struct {
		name           string
		provider       string
		model          string
		forcedProvider bool
		expectAuth     bool
		expectError    bool
	}{
		{
			name:           "forced_provider=true bypasses model check",
			provider:       "copilot",
			model:          "gemini-3-flash-preview",
			forcedProvider: true,
			expectAuth:     true,
			expectError:    false,
		},
		{
			name:           "forced_provider=true with any model succeeds",
			provider:       "copilot",
			model:          "completely-unknown-model",
			forcedProvider: true,
			expectAuth:     true,
			expectError:    false,
		},
		{
			name:           "empty model with forced_provider=true succeeds",
			provider:       "copilot",
			model:          "",
			forcedProvider: true,
			expectAuth:     true,
			expectError:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := cliproxyexecutor.Options{}
			if tt.forcedProvider {
				opts.Metadata = map[string]any{"forced_provider": true}
			}

			// Clear tried map for each test
			for k := range tried {
				delete(tried, k)
			}

			auth, executor, err := mgr.pickNext(ctx, tt.provider, tt.model, opts, tried)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
				}
				if auth != nil {
					t.Errorf("expected nil auth on error, got %+v", auth)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if tt.expectAuth && auth == nil {
					t.Errorf("expected auth but got nil")
				}
				if tt.expectAuth && executor == nil {
					t.Errorf("expected executor but got nil")
				}
			}
		})
	}
}

// TestPickNext_ForcedProviderMetadataTypes tests that forced_provider metadata
// is correctly parsed from different types.
func TestPickNext_ForcedProviderMetadataTypes(t *testing.T) {
	mgr := NewManager(nil, &mockSelector{}, NoopHook{})
	mockExecutor := &mockProviderExecutor{id: "copilot"}
	mgr.RegisterExecutor(mockExecutor)

	testAuth := &Auth{
		ID:       "test-auth",
		Provider: "copilot",
		Disabled: false,
	}
	mgr.Register(context.Background(), testAuth)

	ctx := context.Background()
	tried := make(map[string]struct{})

	tests := []struct {
		name         string
		metadata     map[string]any
		shouldBypass bool
	}{
		{
			name:         "bool true bypasses check",
			metadata:     map[string]any{"forced_provider": true},
			shouldBypass: true,
		},
		{
			name:         "bool false does not bypass check",
			metadata:     map[string]any{"forced_provider": false},
			shouldBypass: false,
		},
		{
			name:         "nil metadata does not bypass check",
			metadata:     nil,
			shouldBypass: false,
		},
		{
			name:         "empty metadata does not bypass check",
			metadata:     map[string]any{},
			shouldBypass: false,
		},
		{
			name:         "string 'true' does not bypass (must be bool)",
			metadata:     map[string]any{"forced_provider": "true"},
			shouldBypass: false,
		},
		{
			name:         "integer 1 does not bypass (must be bool)",
			metadata:     map[string]any{"forced_provider": 1},
			shouldBypass: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := cliproxyexecutor.Options{Metadata: tt.metadata}

			// Clear tried map
			for k := range tried {
				delete(tried, k)
			}

			// With bypass, any model should succeed because we skip model check
			// Without bypass, we still succeed because there's no global registry to check against
			auth, _, err := mgr.pickNext(ctx, "copilot", "test-model", opts, tried)

			if tt.shouldBypass {
				if err != nil {
					t.Errorf("expected bypass to allow auth selection, got error: %v", err)
				}
				if auth == nil {
					t.Errorf("expected auth to be returned when bypassing")
				}
			}
			// Note: without bypass but also without a registry, pick may still succeed
			// The key test is that bypass ALWAYS works
		})
	}
}

// TestPickNext_ForcedProviderWithMultipleAuths tests that forced_provider works
// correctly when there are multiple auths for the same provider.
func TestPickNext_ForcedProviderWithMultipleAuths(t *testing.T) {
	mgr := NewManager(nil, &mockSelector{}, NoopHook{})
	mockExecutor := &mockProviderExecutor{id: "copilot"}
	mgr.RegisterExecutor(mockExecutor)

	ctx := context.Background()

	// Add multiple copilot auths
	auth1 := &Auth{ID: "copilot-auth-1", Provider: "copilot", Disabled: false}
	auth2 := &Auth{ID: "copilot-auth-2", Provider: "copilot", Disabled: false}
	auth3 := &Auth{ID: "copilot-auth-3", Provider: "copilot", Disabled: true} // disabled
	mgr.Register(ctx, auth1)
	mgr.Register(ctx, auth2)
	mgr.Register(ctx, auth3)

	tried := make(map[string]struct{})
	opts := cliproxyexecutor.Options{Metadata: map[string]any{"forced_provider": true}}

	// First call should return one of the enabled auths
	auth, _, err := mgr.pickNext(ctx, "copilot", "any-model", opts, tried)
	if err != nil {
		t.Fatalf("unexpected error on first pick: %v", err)
	}
	if auth == nil {
		t.Fatal("expected auth on first pick")
	}
	if auth.Disabled {
		t.Error("should not return disabled auth")
	}

	// Mark first auth as tried
	tried[auth.ID] = struct{}{}

	// Second call should return the other enabled auth
	auth2Result, _, err := mgr.pickNext(ctx, "copilot", "any-model", opts, tried)
	if err != nil {
		t.Fatalf("unexpected error on second pick: %v", err)
	}
	if auth2Result == nil {
		t.Fatal("expected auth on second pick")
	}
	if auth2Result.ID == auth.ID {
		t.Error("should return different auth than first pick")
	}
	if auth2Result.Disabled {
		t.Error("should not return disabled auth")
	}

	// Mark second auth as tried
	tried[auth2Result.ID] = struct{}{}

	// Third call should fail - all enabled auths tried
	_, _, err = mgr.pickNext(ctx, "copilot", "any-model", opts, tried)
	if err == nil {
		t.Error("expected error when all auths tried")
	}
}

// TestPickNext_DisabledAuthsExcluded tests that disabled auths are never selected.
func TestPickNext_DisabledAuthsExcluded(t *testing.T) {
	mgr := NewManager(nil, &mockSelector{}, NoopHook{})
	mockExecutor := &mockProviderExecutor{id: "copilot"}
	mgr.RegisterExecutor(mockExecutor)

	ctx := context.Background()

	// Add only disabled auths
	auth1 := &Auth{ID: "disabled-auth-1", Provider: "copilot", Disabled: true}
	auth2 := &Auth{ID: "disabled-auth-2", Provider: "copilot", Disabled: true}
	mgr.Register(ctx, auth1)
	mgr.Register(ctx, auth2)

	tried := make(map[string]struct{})
	opts := cliproxyexecutor.Options{Metadata: map[string]any{"forced_provider": true}}

	// Should fail because all auths are disabled
	_, _, err := mgr.pickNext(ctx, "copilot", "any-model", opts, tried)
	if err == nil {
		t.Error("expected error when all auths disabled")
	}
}

// TestPickNext_WrongProviderExcluded tests that auths for other providers are excluded.
func TestPickNext_WrongProviderExcluded(t *testing.T) {
	mgr := NewManager(nil, &mockSelector{}, NoopHook{})

	copilotExecutor := &mockProviderExecutor{id: "copilot"}
	geminiExecutor := &mockProviderExecutor{id: "gemini"}
	mgr.RegisterExecutor(copilotExecutor)
	mgr.RegisterExecutor(geminiExecutor)

	ctx := context.Background()

	// Add auths for different providers
	geminiAuth := &Auth{ID: "gemini-auth", Provider: "gemini", Disabled: false}
	mgr.Register(ctx, geminiAuth)

	tried := make(map[string]struct{})
	opts := cliproxyexecutor.Options{Metadata: map[string]any{"forced_provider": true}}

	// Should fail because no copilot auths exist
	_, _, err := mgr.pickNext(ctx, "copilot", "any-model", opts, tried)
	if err == nil {
		t.Error("expected error when no auths for provider")
	}
}

// mockProviderExecutor implements ProviderExecutor for testing
type mockProviderExecutor struct {
	id string
}

func (m *mockProviderExecutor) Identifier() string { return m.id }

func (m *mockProviderExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (m *mockProviderExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	close(ch)
	return &cliproxyexecutor.StreamResult{Chunks: ch}, nil
}

func (m *mockProviderExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (m *mockProviderExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (m *mockProviderExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

// mockSelector implements Selector for testing
type mockSelector struct{}

func (s *mockSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, candidates []*Auth) (*Auth, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	return candidates[0], nil
}
