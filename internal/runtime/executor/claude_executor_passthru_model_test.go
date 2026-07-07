package executor

import (
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// TestPassthruUpstreamModel guards the non-streaming Execute/CountTokens paths
// against regressing to forwarding the client-facing model name: a passthru
// route's upstream_model attribute must override baseModel so the upstream
// provider receives the model it actually knows (e.g. claude-fable-5), not the
// local alias (e.g. claude-fable-5-a6).
func TestPassthruUpstreamModel(t *testing.T) {
	const base = "claude-fable-5-a6"
	tests := []struct {
		name string
		auth *cliproxyauth.Auth
		want string
	}{
		{"nil auth falls back to base", nil, base},
		{"nil attributes falls back to base", &cliproxyauth.Auth{}, base},
		{"no upstream_model falls back to base", &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "k"}}, base},
		{"upstream_model override wins", &cliproxyauth.Auth{Attributes: map[string]string{"upstream_model": "claude-fable-5"}}, "claude-fable-5"},
		{"upstream_model suffix is stripped", &cliproxyauth.Auth{Attributes: map[string]string{"upstream_model": "claude-fable-5(1024)"}}, "claude-fable-5"},
		{"blank upstream_model falls back to base", &cliproxyauth.Auth{Attributes: map[string]string{"upstream_model": "   "}}, base},
		{"suffix-only upstream_model falls back to base", &cliproxyauth.Auth{Attributes: map[string]string{"upstream_model": "(0)"}}, base},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := passthruUpstreamModel(tt.auth, base); got != tt.want {
				t.Fatalf("passthruUpstreamModel(%+v, %q) = %q, want %q", tt.auth, base, got, tt.want)
			}
		})
	}
}
