package auth_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func codeBuddyPersistSetup(t *testing.T, transport http.RoundTripper) (*coreauth.Manager, *sdkAuth.FileTokenStore, string, context.Context) {
	t.Helper()
	dir := t.TempDir()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	mgr := coreauth.NewManager(store, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.RegisterExecutor(executor.NewCodeBuddyExecutor(&internalconfig.Config{}))
	return mgr, store, dir, context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
}

func codeBuddyPersistAuth(id string) *coreauth.Auth {
	auth := codeBuddyConductorAuth(id, codebuddy.RealmCN, "persist-user", "STALE", "hy4-preview")
	auth.FileName = id
	return auth
}

func codeBuddyPersistRegister(t *testing.T, mgr *coreauth.Manager, auth *coreauth.Auth) {
	t.Helper()
	if _, err := mgr.Register(context.Background(), auth); err != nil {
		t.Fatalf("register: %v", err)
	}
	creds, err := codebuddy.CredentialsFromMetadata(auth.Metadata)
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(auth.ID, "codebuddy", []*registry.ModelInfo{{ID: codebuddy.PublicModelID(creds.Realm, "hy4-preview"), UpstreamID: "hy4-preview", Type: "codebuddy"}})
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
}

func readCodeBuddyPersistFile(t *testing.T, dir, id string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, id))
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	return string(raw)
}

func TestConductorCodeBuddyRefreshPersistsAndReloads(t *testing.T) {
	chats := 0
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/token/refresh") {
			return codeBuddyConductorJSON(200, `{"code":0,"msg":"ok","data":{"accessToken":"ROTATED","refreshToken":"ROTATED-R","expiresIn":3600}}`, nil), nil
		}
		chats++
		if chats == 1 {
			return codeBuddyConductorJSON(401, `{"code":0,"msg":"unauthorized"}`, nil), nil
		}
		return codeBuddyConductorSSE(codeBuddyConductorSuccessSSE), nil
	}}
	mgr, _, dir, ctx := codeBuddyPersistSetup(t, transport)
	auth := codeBuddyPersistAuth("codebuddy-persist-a.json")
	codeBuddyPersistRegister(t, mgr, auth)
	if body := readCodeBuddyPersistFile(t, dir, auth.ID); !strings.Contains(body, "STALE") {
		t.Fatalf("initial record not persisted: %s", body)
	}
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	if _, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if body := readCodeBuddyPersistFile(t, dir, auth.ID); !strings.Contains(body, "ROTATED") {
		t.Fatalf("rotated token not persisted: %s", body)
	}
	// A fresh manager over the same directory reconstructs the canonical record.
	mgr2 := coreauth.NewManager(mustCodeBuddyPersistStore(t, dir), nil, nil)
	if err := mgr2.Load(context.Background()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	loaded, ok := mgr2.GetByID(auth.ID)
	if !ok {
		t.Fatal("reloaded auth missing")
	}
	creds, err := codebuddy.CredentialsFromMetadata(loaded.Metadata)
	if err != nil || creds.Realm != codebuddy.RealmCN || creds.AccessToken != "ROTATED" {
		t.Fatalf("reloaded credentials = %+v %v", creds, err)
	}
	catalog, err := codebuddy.CatalogFromMetadata(loaded.Metadata)
	if err != nil || len(catalog.Models) != 1 || catalog.Models[0].ID != "hy4-preview" {
		t.Fatalf("reloaded catalog = %+v %v", catalog, err)
	}
	if loaded.Provider != "codebuddy" {
		t.Fatalf("reloaded provider = %q", loaded.Provider)
	}
}

func mustCodeBuddyPersistStore(t *testing.T, dir string) *sdkAuth.FileTokenStore {
	t.Helper()
	store := sdkAuth.NewFileTokenStore()
	store.SetBaseDir(dir)
	return store
}

func TestConductorCodeBuddyConcurrentEditSurvivesRefreshMerge(t *testing.T) {
	refreshObserved := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseOnce sync.Once
	chats := 0
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/token/refresh") {
			select {
			case <-refreshObserved:
			default:
				close(refreshObserved)
			}
			<-releaseRefresh
			return codeBuddyConductorJSON(200, `{"code":0,"msg":"ok","data":{"accessToken":"ROTATED","refreshToken":"ROTATED-R","expiresIn":3600}}`, nil), nil
		}
		chats++
		if chats == 1 {
			return codeBuddyConductorJSON(401, `{"code":0,"msg":"unauthorized"}`, nil), nil
		}
		return codeBuddyConductorSSE(codeBuddyConductorSuccessSSE), nil
	}}
	mgr, _, dir, ctx := codeBuddyPersistSetup(t, transport)
	auth := codeBuddyPersistAuth("codebuddy-persist-merge.json")
	codeBuddyPersistRegister(t, mgr, auth)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	execDone := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
		execDone <- err
	}()
	select {
	case <-refreshObserved:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh never started")
	}
	// Concurrent user edit while the refresh HTTP call is in flight.
	current, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing before edit")
	}
	current.Disabled = true
	current.Status = coreauth.StatusDisabled
	current.ProxyURL = "http://proxy.invalid:8080"
	if current.Metadata == nil {
		current.Metadata = map[string]any{}
	}
	current.Metadata["proxy_url"] = "http://proxy.invalid:8080"
	if _, err := mgr.Update(context.Background(), current); err != nil {
		t.Fatalf("user edit: %v", err)
	}
	releaseOnce.Do(func() { close(releaseRefresh) })
	select {
	case err := <-execDone:
		// The user disabled the account mid-flight, so the post-refresh chat
		// attempt must refuse to serve on the merged disabled auth.
		if err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("execute err = %v, want disabled refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execute never finished")
	}
	merged, ok := mgr.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing after merge")
	}
	if !merged.Disabled || merged.ProxyURL != "http://proxy.invalid:8080" {
		t.Fatalf("user edit lost in merge: disabled=%v proxy=%q", merged.Disabled, merged.ProxyURL)
	}
	if merged.Metadata["access_token"] != "ROTATED" {
		t.Fatalf("rotated token lost in merge: %v", merged.Metadata["access_token"])
	}
	body := readCodeBuddyPersistFile(t, dir, auth.ID)
	if !strings.Contains(body, "ROTATED") || !strings.Contains(body, "proxy.invalid") {
		t.Fatalf("merged record not persisted: %s", body)
	}
	// Re-enabling (and clearing the synthetic proxy) serves subsequent chats
	// with the rotated token.
	merged.Disabled = false
	merged.Status = coreauth.StatusActive
	merged.ProxyURL = ""
	delete(merged.Metadata, "proxy_url")
	if _, err := mgr.Update(context.Background(), merged); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	req2, opts2 := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	if _, err := mgr.Execute(ctx, []string{"codebuddy"}, req2, opts2); err != nil {
		t.Fatalf("post-merge execute: %v", err)
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	rotated := false
	for _, call := range transport.calls {
		if strings.HasSuffix(call.path, "/v2/chat/completions") && strings.Contains(call.token, "ROTATED") {
			rotated = true
		}
	}
	if !rotated {
		t.Fatalf("no chat used the rotated token: %+v", transport.calls)
	}
}

func TestConductorCodeBuddyDeleteDuringRefreshDoesNotResurrect(t *testing.T) {
	refreshObserved := make(chan struct{})
	releaseRefresh := make(chan struct{})
	var releaseOnce sync.Once
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/token/refresh") {
			select {
			case <-refreshObserved:
			default:
				close(refreshObserved)
			}
			<-releaseRefresh
			return codeBuddyConductorJSON(200, `{"code":0,"msg":"ok","data":{"accessToken":"ROTATED","refreshToken":"ROTATED-R","expiresIn":3600}}`, nil), nil
		}
		return codeBuddyConductorJSON(401, `{"code":0,"msg":"unauthorized"}`, nil), nil
	}}
	mgr, store, _, ctx := codeBuddyPersistSetup(t, transport)
	auth := codeBuddyPersistAuth("codebuddy-persist-delete.json")
	codeBuddyPersistRegister(t, mgr, auth)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	execDone := make(chan error, 1)
	go func() {
		_, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
		execDone <- err
	}()
	select {
	case <-refreshObserved:
	case <-time.After(10 * time.Second):
		t.Fatal("refresh never started")
	}
	mgr.Remove(context.Background(), auth.ID)
	if err := store.Delete(context.Background(), auth.ID); err != nil {
		t.Fatalf("store delete: %v", err)
	}
	releaseOnce.Do(func() { close(releaseRefresh) })
	select {
	case err := <-execDone:
		if err == nil {
			t.Fatal("expected failure after account deletion")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execute never finished")
	}
	if _, ok := mgr.GetByID(auth.ID); ok {
		t.Fatal("deleted auth resurrected in manager")
	}
	remaining, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("store list: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("deleted auth resurrected on disk: %d records", len(remaining))
	}
}
