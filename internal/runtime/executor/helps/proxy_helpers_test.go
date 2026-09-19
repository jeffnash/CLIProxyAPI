package helps

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
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

	wrapper := NewProxyAwareHTTPClient(ctx, nil, nil, 5*time.Second, "test")
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

	client := NewProxyAwareHTTPClient(ctx, nil, nil, 0, "test")
	if client.Timeout != 0 {
		t.Fatalf("expected client Timeout=0, got %v", client.Timeout)
	}
}

func TestNewProxyAwareHTTPClient_DoesNotCacheTimeout_WithProxy(t *testing.T) {
	resetProxyHTTPClientCacheForTest()
	ctx := context.Background()

	auth := &cliproxyauth.Auth{ProxyURL: "http://example.com:8080"}
	wrapper := NewProxyAwareHTTPClient(ctx, nil, auth, 7*time.Second, "test")
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

	client := NewProxyAwareHTTPClient(ctx, nil, auth, 0, "test")
	if client.Timeout != 0 {
		t.Fatalf("expected client Timeout=0, got %v", client.Timeout)
	}
}

func TestNewProxyAwareHTTPClientDirectBypassesGlobalProxy(t *testing.T) {
	t.Parallel()

	client := NewProxyAwareHTTPClient(
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

func TestNewDevinHTTPClient_ReusesTransportFromContext(t *testing.T) {
	baseTransport := &http.Transport{}
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", baseTransport)

	c1 := NewDevinHTTPClient(ctx, nil, nil, 0)
	c2 := NewDevinHTTPClient(ctx, nil, nil, 0)

	if c1.Transport != c2.Transport {
		t.Errorf("expected c1.Transport == c2.Transport across requests, got different pointers %p vs %p", c1.Transport, c2.Transport)
	}

	tr, ok := c1.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c1.Transport)
	}
	if !tr.DisableCompression {
		t.Error("expected DisableCompression = true")
	}
}

func TestNewDevinHTTPClient_NonStandardRoundTripperDisablesGzip(t *testing.T) {
	var seenEncoding string
	customRT := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seenEncoding = req.Header.Get("Accept-Encoding")
		return &http.Response{StatusCode: 200}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", customRT)

	c := NewDevinHTTPClient(ctx, nil, nil, 0)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid", nil)
	_, _ = c.Transport.RoundTrip(req)

	if seenEncoding != "identity" {
		t.Errorf("expected Accept-Encoding: identity, got %q", seenEncoding)
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
