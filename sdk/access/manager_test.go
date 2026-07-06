package access

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestManagerAuthenticateWithoutProvidersFailsClosed(t *testing.T) {
	manager := NewManager()
	_, err := manager.Authenticate(context.Background(), httptest.NewRequest("GET", "/", nil))
	if !IsAuthErrorCode(err, AuthErrorCodeNoCredentials) {
		t.Fatalf("Authenticate() error = %#v, want no credentials", err)
	}
}
