package executor

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
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
		cacheKey += "|no_proxy=" + strings.ToLower(noProxyRaw)
	}
	cached := httpClientCache[cacheKey]
	httpClientCacheMutex.RUnlock()
	if cached == nil {
		t.Fatalf("expected cached base client for proxy key %q", cacheKey)
	}
	if cached.Timeout != 0 {
		t.Fatalf("expected cached Timeout=0, got %v", cached.Timeout)
	}

	client := newProxyAwareHTTPClient(ctx, nil, auth, 0, "test")
	if client.Timeout != 0 {
		t.Fatalf("expected client Timeout=0, got %v", client.Timeout)
	}
}

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := newProxyAwareHTTPClient(
		context.Background(),
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
		0,
		"test",
	)

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.Proxy != nil {
		t.Fatal("expected direct transport to disable proxy function")
	}
}
