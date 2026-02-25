package executor

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func resetProxyHTTPClientCacheForTest() {
	httpClientCacheMutex.Lock()
	defer httpClientCacheMutex.Unlock()
	httpClientCache = make(map[string]*http.Client)
	proxyInfoOnce = sync.Map{}
}

func TestNewProxyAwareHTTPClient_DoesNotCacheTimeout_NoProxy(t *testing.T) {
	resetProxyHTTPClientCacheForTest()
	ctx := context.Background()

	wrapper := newProxyAwareHTTPClient(ctx, nil, nil, 5*time.Second, "test")
	if wrapper.Timeout != 5*time.Second {
		t.Fatalf("expected wrapper Timeout=5s, got %v", wrapper.Timeout)
	}

	httpClientCacheMutex.RLock()
	cached := httpClientCache[""]
	httpClientCacheMutex.RUnlock()
	if cached == nil {
		t.Fatalf("expected cached base client for empty proxy key")
	}
	if cached.Timeout != 0 {
		t.Fatalf("expected cached Timeout=0, got %v", cached.Timeout)
	}

	client := newProxyAwareHTTPClient(ctx, nil, nil, 0, "test")
	if client.Timeout != 0 {
		t.Fatalf("expected client Timeout=0, got %v", client.Timeout)
	}
}

func TestNewProxyAwareHTTPClient_DoesNotCacheTimeout_WithProxy(t *testing.T) {
	resetProxyHTTPClientCacheForTest()
	ctx := context.Background()

	auth := &cliproxyauth.Auth{ProxyURL: "http://example.com:8080"}
	wrapper := newProxyAwareHTTPClient(ctx, nil, auth, 7*time.Second, "test")
	if wrapper.Timeout != 7*time.Second {
		t.Fatalf("expected wrapper Timeout=7s, got %v", wrapper.Timeout)
	}

	httpClientCacheMutex.RLock()
	cacheKey := "http://example.com:8080"
	if noProxyRaw := noProxyEnvRaw(); noProxyRaw != "" {
		cacheKey = cacheKey + "|no_proxy=" + strings.ToLower(noProxyRaw)
	}
	cached := httpClientCache[cacheKey]
	httpClientCacheMutex.RUnlock()
	if cached == nil {
		t.Fatalf("expected cached base client for proxy key %q", cacheKey)
	}
	if cached.Timeout != 0 {
		t.Fatalf("expected cached Timeout=0, got %v", cached.Timeout)
	}
}
