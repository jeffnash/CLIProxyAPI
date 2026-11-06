package codebuddy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func resetDoHState(t *testing.T, url string) {
	t.Helper()
	oldURL := dohResolveURL
	dohResolveURL = url
	dohCacheMu.Lock()
	oldCache := dohCache
	dohCache = map[string]dohCacheEntry{}
	dohCacheMu.Unlock()
	t.Cleanup(func() {
		dohResolveURL = oldURL
		dohCacheMu.Lock()
		dohCache = oldCache
		dohCacheMu.Unlock()
	})
}

func TestHostNeedsDoH(t *testing.T) {
	cases := map[string]bool{
		"www.codebuddy.ai":       true,
		"codebuddy.ai":           true,
		"api.codebuddy.ai":       true,
		"WWW.CodeBuddy.AI":       true,
		"www.codebuddy.cn":       false,
		"www.workbuddy.ai":       false,
		"copilot.tencent.com":    false,
		"codebuddy.ai.evil.com":  false,
		"notcodebuddy.ai":        false,
		"www.codebuddy.ai.evil":  false,
		"codebuddy.cn":           false,
		"sub.codebuddy.ai.local": false,
	}
	for host, want := range cases {
		if got := hostNeedsDoH(host); got != want {
			t.Errorf("hostNeedsDoH(%q) = %v, want %v", host, got, want)
		}
	}
}

func dohTestServer(t *testing.T, body string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("name"); got != "www.codebuddy.ai" {
			t.Errorf("DoH query name = %q, want www.codebuddy.ai", got)
		}
		if got := r.URL.Query().Get("type"); got != "A" {
			t.Errorf("DoH query type = %q, want A", got)
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveViaDoHParsesARecords(t *testing.T) {
	srv := dohTestServer(t, `{"Answer":[
		{"name":"www.codebuddy.ai.","type":5,"TTL":60,"data":"www.codebuddy.ai.eo.dnse2.com."},
		{"name":"www.codebuddy.ai.eo.dnse2.com.","type":1,"TTL":60,"data":"43.169.32.91"},
		{"name":"www.codebuddy.ai.eo.dnse2.com.","type":1,"TTL":60,"data":"43.169.32.92"}
	]}`, http.StatusOK)
	resetDoHState(t, srv.URL)

	ips, err := resolveViaDoH(context.Background(), "www.codebuddy.ai")
	if err != nil {
		t.Fatalf("resolveViaDoH: %v", err)
	}
	if len(ips) != 2 || ips[0] != "43.169.32.91" || ips[1] != "43.169.32.92" {
		t.Fatalf("ips = %v, want [43.169.32.91 43.169.32.92]", ips)
	}
}

func TestResolveViaDoHStaleCacheOnFailure(t *testing.T) {
	srv := dohTestServer(t, ``, http.StatusInternalServerError)
	resetDoHState(t, srv.URL)
	dohCacheMu.Lock()
	dohCache["www.codebuddy.ai"] = dohCacheEntry{ips: []string{"43.169.32.91"}, at: time.Now().Add(-time.Hour)}
	dohCacheMu.Unlock()

	ips, err := resolveViaDoH(context.Background(), "www.codebuddy.ai")
	if err != nil {
		t.Fatalf("resolveViaDoH with stale cache: %v", err)
	}
	if len(ips) != 1 || ips[0] != "43.169.32.91" {
		t.Fatalf("ips = %v, want stale [43.169.32.91]", ips)
	}
}

func TestDoHDialContextDialsResolvedIP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, errAccept := listener.Accept()
		if errAccept == nil {
			_ = conn.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())

	srv := dohTestServer(t, `{"Answer":[{"type":1,"data":"127.0.0.1"}]}`, http.StatusOK)
	resetDoHState(t, srv.URL)

	var dialed []string
	base := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	conn, err := dohDialContext(base)(context.Background(), "tcp", net.JoinHostPort("www.codebuddy.ai", port))
	if err != nil {
		t.Fatalf("dial via DoH: %v", err)
	}
	_ = conn.Close()
	if len(dialed) != 1 || dialed[0] != net.JoinHostPort("127.0.0.1", port) {
		t.Fatalf("base dialed %v, want [127.0.0.1:%s]", dialed, port)
	}
}

func TestDoHDialContextPassesThroughOtherHosts(t *testing.T) {
	resetDoHState(t, "http://127.0.0.1:1/unused")
	var dialed []string
	base := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("stop here")
	}
	_, err := dohDialContext(base)(context.Background(), "tcp", "copilot.tencent.com:443")
	if err == nil || !strings.Contains(err.Error(), "stop here") {
		t.Fatalf("err = %v, want the base dialer's error", err)
	}
	if len(dialed) != 1 || dialed[0] != "copilot.tencent.com:443" {
		t.Fatalf("base dialed %v, want the address unchanged", dialed)
	}
}

func TestDoHDialContextFallsBackWhenDoHFails(t *testing.T) {
	srv := dohTestServer(t, ``, http.StatusBadGateway)
	resetDoHState(t, srv.URL)
	var dialed []string
	base := func(_ context.Context, _, addr string) (net.Conn, error) {
		dialed = append(dialed, addr)
		return nil, errors.New("stop here")
	}
	_, _ = dohDialContext(base)(context.Background(), "tcp", "www.codebuddy.ai:443")
	if len(dialed) != 1 || dialed[0] != "www.codebuddy.ai:443" {
		t.Fatalf("base dialed %v, want fallback to the original address", dialed)
	}
}

func TestDoHWrapTransport(t *testing.T) {
	base := &http.Transport{}
	first := dohWrapTransport(base)
	second := dohWrapTransport(base)
	if first != second {
		t.Fatalf("wrap not memoized: %p != %p", first, second)
	}
	if first == http.RoundTripper(base) {
		t.Fatal("wrap returned the source transport; want a clone")
	}
	if first.(*http.Transport).DialContext == nil {
		t.Fatal("wrapped transport has no DialContext")
	}

	// An operator-configured proxy (one that differs from the environment's)
	// is respected: the transport is returned unchanged.
	explicitProxy, _ := url.Parse("http://127.0.0.1:9999")
	withProxy := &http.Transport{Proxy: http.ProxyURL(explicitProxy)}
	if got := dohWrapTransport(withProxy); got != http.RoundTripper(withProxy) {
		t.Fatal("transport with an operator-configured proxy must be returned unchanged")
	}

	// The inherited environment proxy is dropped on the wrapped clone: a
	// proxied connection would hand the proxy the broken hostname lookup.
	inherited := &http.Transport{Proxy: http.ProxyFromEnvironment}
	wrappedInherited := dohWrapTransport(inherited).(*http.Transport)
	if wrappedInherited == inherited {
		t.Fatal("inherited-proxy transport was not wrapped")
	}
	if wrappedInherited.Proxy != nil {
		t.Fatal("wrapped transport must dial directly (Proxy = nil)")
	}

	sentinel := errors.New("custom round tripper")
	custom := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, sentinel })
	got := dohWrapTransport(custom)
	if _, err := got.RoundTrip(&http.Request{}); !errors.Is(err, sentinel) {
		t.Fatalf("non-*http.Transport round tripper changed: RoundTrip err = %v, want the sentinel", err)
	}
}
