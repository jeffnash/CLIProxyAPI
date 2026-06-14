package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"golang.org/x/sync/singleflight"
)

func resetAntigravityRefreshGroupForTest() {
	antigravityRefreshGroup = singleflight.Group{}
}

func TestAntigravityRefresh_DeduplicatesConcurrentRefresh(t *testing.T) {
	resetAntigravityRefreshGroupForTest()
	t.Cleanup(resetAntigravityRefreshGroupForTest)
	resetAntigravityCreditsRetryState()
	t.Cleanup(resetAntigravityCreditsRetryState)

	var tokenCalls int32
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			atomic.AddInt32(&tokenCalls, 1)
			once.Do(func() { close(started) })
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"access_token":"new-access",
				"refresh_token":"new-refresh",
				"token_type":"Bearer",
				"expires_in":3600
			}`)
		case "/v1internal:loadCodeAssist":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"paidTier":{"id":"tier","availableCredits":[]}}`)
		default:
			t.Errorf("unexpected antigravity test request path: %s", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer server.Close()

	serverURL, errParse := url.Parse(server.URL)
	if errParse != nil {
		t.Fatalf("parse test server URL: %v", errParse)
	}
	testTransport := server.Client().Transport
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		cloned := req.Clone(req.Context())
		cloned.URL = cloneURLForAntigravityRefreshTest(req.URL, serverURL)
		cloned.Host = serverURL.Host
		return testTransport.RoundTrip(cloned)
	}))

	executor := &AntigravityExecutor{}
	authA := &cliproxyauth.Auth{
		ID:       "auth-a",
		Provider: "antigravity",
		Metadata: map[string]any{
			"refresh_token": "shared-refresh-token",
			"project_id":    "project-a",
		},
	}
	authB := &cliproxyauth.Auth{
		ID:       "auth-b",
		Provider: "antigravity",
		Metadata: map[string]any{
			"refresh_token": "shared-refresh-token",
			"project_id":    "project-b",
		},
	}

	type refreshResult struct {
		auth *cliproxyauth.Auth
		err  error
	}
	results := make(chan refreshResult, 2)
	runRefresh := func(auth *cliproxyauth.Auth, launched chan<- struct{}) {
		if launched != nil {
			close(launched)
		}
		updated, errRefresh := executor.Refresh(ctx, auth)
		results <- refreshResult{auth: updated, err: errRefresh}
	}

	go runRefresh(authA, nil)
	<-started

	secondLaunched := make(chan struct{})
	go runRefresh(authB, secondLaunched)
	<-secondLaunched
	time.Sleep(20 * time.Millisecond)
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("expected concurrent refresh to share a single upstream token call, got %d", got)
	}
	close(release)

	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("expected refresh to succeed, got %v", result.err)
		}
		updated := result.auth
		if updated == nil {
			t.Fatal("expected refreshed auth, got nil")
		}
		if got := metaStringValue(updated.Metadata, "access_token"); got != "new-access" {
			t.Fatalf("access_token = %q, want new-access", got)
		}
		if got := metaStringValue(updated.Metadata, "refresh_token"); got != "new-refresh" {
			t.Fatalf("refresh_token = %q, want new-refresh", got)
		}
		if projectID := strings.TrimSpace(updated.Metadata["project_id"].(string)); projectID == "" {
			t.Fatalf("expected project_id to stay on refreshed auth: %#v", updated.Metadata)
		}
	}
	if got := atomic.LoadInt32(&tokenCalls); got != 1 {
		t.Fatalf("expected both refresh callers to share a single upstream token call, got %d", got)
	}
}

func cloneURLForAntigravityRefreshTest(original *url.URL, serverURL *url.URL) *url.URL {
	cloned := *original
	cloned.Scheme = serverURL.Scheme
	cloned.Host = serverURL.Host
	return &cloned
}
