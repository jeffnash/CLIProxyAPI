package auth

import "testing"

func TestXAIAuthenticatorProviderAndRefreshLead(t *testing.T) {
	authenticator := NewXAIAuthenticator()
	if authenticator.Provider() != "xai" {
		t.Fatalf("Provider() = %q, want xai", authenticator.Provider())
	}
	lead := authenticator.RefreshLead()
	if lead == nil || *lead <= 0 {
		t.Fatalf("RefreshLead() = %v, want positive duration", lead)
	}
}

// NOTE: the loopback manual-callback-token flow (parseXAIManualCallbackToken)
// was replaced by the OAuth device-code flow; its parser tests were removed
// with it. Device-flow coverage lives in the Login path tests.
