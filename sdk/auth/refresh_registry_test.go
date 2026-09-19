package auth

import (
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestProviderRefreshLeads(t *testing.T) {
	tests := []struct {
		name          string
		authenticator Authenticator
		want          time.Duration
		wantNil       bool
	}{
		{name: "codex", authenticator: NewCodexAuthenticator(), want: 24 * time.Hour},
		{name: "claude", authenticator: NewClaudeAuthenticator(), want: 4 * time.Hour},
		{name: "antigravity", authenticator: NewAntigravityAuthenticator(), want: 30 * time.Minute},
		{name: "kimi", authenticator: NewKimiAuthenticator(), want: 5 * time.Minute},
		{name: "xai", authenticator: NewXAIAuthenticator(), want: 5 * time.Minute},
		{name: "codebuddy", authenticator: NewCodeBuddyAuthenticator(), want: 5 * time.Minute},
		{name: "devin", authenticator: NewDevinAuthenticator(), wantNil: true},
		{name: "meta", authenticator: NewMetaAuthenticator(), wantNil: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.authenticator.Provider(); got != test.name {
				t.Fatalf("Provider() = %q, want %q", got, test.name)
			}
			lead := test.authenticator.RefreshLead()
			if test.wantNil {
				if lead != nil {
					t.Fatalf("RefreshLead() = %v, want nil", lead)
				}
				return
			}
			if lead == nil || *lead != test.want {
				t.Fatalf("RefreshLead() = %v, want %v", lead, test.want)
			}
		})
	}
}

func TestProviderRefreshLeadRegistry(t *testing.T) {
	if lead := cliproxyauth.ProviderRefreshLead("codebuddy", nil); lead == nil || *lead != 5*time.Minute {
		t.Fatalf("codebuddy lead = %v", lead)
	}
	registerRefreshLead("codebuddy-nil-factory", nil)
	if lead := cliproxyauth.ProviderRefreshLead("codebuddy-nil-factory", nil); lead != nil {
		t.Fatalf("nil factory lead = %v", lead)
	}
	registerRefreshLead("codebuddy-nil-auth", func() Authenticator { return nil })
	if lead := cliproxyauth.ProviderRefreshLead("codebuddy-nil-auth", nil); lead != nil {
		t.Fatalf("nil authenticator lead = %v", lead)
	}
}
