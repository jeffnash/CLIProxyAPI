package auth_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type codeBuddyConductorCall struct {
	host  string
	path  string
	token string
}

type codeBuddyConductorTransport struct {
	mu     sync.Mutex
	calls  []codeBuddyConductorCall
	handle func(call codeBuddyConductorCall, req *http.Request) (*http.Response, error)
}

func (t *codeBuddyConductorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	call := codeBuddyConductorCall{host: req.URL.Host, path: req.URL.Path, token: req.Header.Get("Authorization")}
	t.mu.Lock()
	t.calls = append(t.calls, call)
	t.mu.Unlock()
	return t.handle(call, req)
}

func (t *codeBuddyConductorTransport) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.calls)
}

func (t *codeBuddyConductorTransport) countHost(host string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, call := range t.calls {
		if call.host == host {
			n++
		}
	}
	return n
}

func codeBuddyConductorSSE(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func codeBuddyConductorJSON(status int, body string, headers map[string]string) *http.Response {
	header := http.Header{"Content-Type": []string{"application/json"}}
	for key, value := range headers {
		header.Set(key, value)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

const codeBuddyConductorSuccessSSE = "data: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: {\"model\":\"hy4-preview\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func codeBuddyConductorAuth(id string, realm codebuddy.Realm, uid, token string, models ...string) *coreauth.Auth {
	tools := true
	reasoning := true
	entries := make([]codebuddy.Model, 0, len(models))
	for _, model := range models {
		entries = append(entries, codebuddy.Model{ID: model, SupportsTools: &tools, SupportsReasoning: &reasoning, Efforts: []string{"high"}})
	}
	creds := codebuddy.Credentials{
		Realm: realm, AccessToken: token, RefreshToken: token + "-refresh",
		UID: uid, MachineID: "mid-" + id, SessionID: "sid-" + id,
		Expired: time.Now().Add(time.Hour),
	}
	catalog := codebuddy.Catalog{Models: entries, Sources: []string{"/v3/config"}}
	return &coreauth.Auth{ID: id, Provider: "codebuddy", Status: coreauth.StatusActive, Metadata: codebuddy.Metadata(creds, catalog, time.Now())}
}

func codeBuddyConductorSetup(t *testing.T, transport http.RoundTripper, auths ...*coreauth.Auth) (*coreauth.Manager, context.Context) {
	t.Helper()
	mgr := coreauth.NewManager(nil, nil, nil)
	mgr.SetConfig(&internalconfig.Config{})
	mgr.RegisterExecutor(executor.NewCodeBuddyExecutor(&internalconfig.Config{}))
	reg := registry.GetGlobalRegistry()
	for _, auth := range auths {
		if _, err := mgr.Register(context.Background(), auth); err != nil {
			t.Fatalf("register %s: %v", auth.ID, err)
		}
		catalog, err := codebuddy.CatalogFromMetadata(auth.Metadata)
		if err != nil {
			t.Fatalf("catalog %s: %v", auth.ID, err)
		}
		creds, err := codebuddy.CredentialsFromMetadata(auth.Metadata)
		if err != nil {
			t.Fatalf("credentials %s: %v", auth.ID, err)
		}
		infos := make([]*registry.ModelInfo, 0, len(catalog.Models))
		for _, model := range catalog.Models {
			infos = append(infos, &registry.ModelInfo{ID: codebuddy.PublicModelID(creds.Realm, model.ID), UpstreamID: model.ID, Type: "codebuddy"})
		}
		reg.RegisterClient(auth.ID, "codebuddy", infos)
	}
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		ids = append(ids, auth.ID)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			reg.UnregisterClient(id)
		}
	})
	return mgr, context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
}

func codeBuddyConductorRequest(model string) (cliproxyexecutor.Request, cliproxyexecutor.Options) {
	payload := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	return cliproxyexecutor.Request{Model: model, Payload: []byte(payload)},
		cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(payload)}
}

func TestConductorCodeBuddyRealmRouting(t *testing.T) {
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		return codeBuddyConductorSSE(codeBuddyConductorSuccessSSE), nil
	}}
	cn := codeBuddyConductorAuth("codebuddy-cond-cn", codebuddy.RealmCN, "same-user", "TOKEN-CN", "hy4-preview")
	global := codeBuddyConductorAuth("codebuddy-cond-global", codebuddy.RealmGlobal, "same-user", "TOKEN-GLOBAL", "hy4-preview")
	mgr, ctx := codeBuddyConductorSetup(t, transport, cn, global)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	if _, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts); err != nil {
		t.Fatalf("cn execute: %v", err)
	}
	if transport.countHost("copilot.tencent.com") == 0 || transport.countHost("www.codebuddy.ai") != 0 {
		t.Fatalf("cn request touched global: %+v", transport.calls)
	}
	before := transport.count()
	req, opts = codeBuddyConductorRequest("codebuddy-global-hy4-preview")
	if _, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts); err != nil {
		t.Fatalf("global execute: %v", err)
	}
	if transport.count()-before == 0 || transport.countHost("www.codebuddy.ai") == 0 {
		t.Fatalf("global request misrouted: %+v", transport.calls)
	}
}

func TestConductorCodeBuddyRequestScopedNoRotation(t *testing.T) {
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		return codeBuddyConductorJSON(400, `{"code":11115,"msg":"prompt is too long"}`, nil), nil
	}}
	a := codeBuddyConductorAuth("codebuddy-cond-a", codebuddy.RealmCN, "user-a", "TOKEN-A", "hy4-preview")
	b := codeBuddyConductorAuth("codebuddy-cond-b", codebuddy.RealmCN, "user-b", "TOKEN-B", "hy4-preview")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a, b)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	_, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err == nil {
		t.Fatal("expected error")
	}
	if transport.count() != 1 {
		t.Fatalf("request fault rotated: %d calls", transport.count())
	}
	var scoped interface{ IsRequestScoped() bool }
	if !errors.As(err, &scoped) || !scoped.IsRequestScoped() {
		t.Fatalf("error not request-scoped: %v", err)
	}
}

func TestConductorCodeBuddyRetryAfterPreserved(t *testing.T) {
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		return codeBuddyConductorJSON(429, `{"code":0,"msg":"slow down"}`, map[string]string{"Retry-After": "3"}), nil
	}}
	a := codeBuddyConductorAuth("codebuddy-cond-429", codebuddy.RealmCN, "user-429", "TOKEN-429", "hy4-preview")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	_, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err == nil {
		t.Fatal("expected error")
	}
	var withDelay interface{ RetryAfter() *time.Duration }
	if !errors.As(err, &withDelay) || withDelay.RetryAfter() == nil || *withDelay.RetryAfter() != 3*time.Second {
		t.Fatalf("retry-after lost: %v", err)
	}
}

func TestConductorCodeBuddyModelNotFoundScope(t *testing.T) {
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		if strings.Contains(string(raw), "hy4-preview-f") {
			return codeBuddyConductorSSE(strings.ReplaceAll(codeBuddyConductorSuccessSSE, "hy4-preview", "hy4-preview-f")), nil
		}
		return codeBuddyConductorJSON(400, `{"code":11102,"msg":"service info not found for hy4-preview"}`, nil), nil
	}}
	a := codeBuddyConductorAuth("codebuddy-cond-mnf", codebuddy.RealmCN, "user-mnf", "TOKEN-MNF", "hy4-preview", "hy4-preview-f")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	_, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err == nil || !strings.Contains(err.Error(), "model_not_found") {
		t.Fatalf("err = %v", err)
	}
	req, opts = codeBuddyConductorRequest("codebuddy-cn-hy4-preview-f")
	resp, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err != nil {
		t.Fatalf("sibling model affected: %v", err)
	}
	if !strings.Contains(string(resp.Payload), "Hi") {
		t.Fatalf("payload = %s", resp.Payload)
	}
}

func TestConductorCodeBuddyFailover429(t *testing.T) {
	var mu sync.Mutex
	var wires [][]byte
	chats := 0
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		mu.Lock()
		wires = append(wires, raw)
		chats++
		first := chats == 1
		mu.Unlock()
		if first {
			return codeBuddyConductorJSON(429, `{"code":11140,"msg":"rate-limiting requests"}`, map[string]string{"Retry-After": "1"}), nil
		}
		return codeBuddyConductorSSE(codeBuddyConductorSuccessSSE), nil
	}}
	a := codeBuddyConductorAuth("codebuddy-cond-fail-a", codebuddy.RealmCN, "user-fa", "TOKEN-FA", "hy4-preview")
	b := codeBuddyConductorAuth("codebuddy-cond-fail-b", codebuddy.RealmCN, "user-fb", "TOKEN-FB", "hy4-preview")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a, b)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	resp, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(string(resp.Payload), "Hi") {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if transport.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2", transport.count())
	}
	transport.mu.Lock()
	firstToken, secondToken := transport.calls[0].token, transport.calls[1].token
	transport.mu.Unlock()
	if firstToken == secondToken {
		t.Fatalf("no rotation: both calls used %q", firstToken)
	}
	for i, wire := range wires {
		if !strings.Contains(string(wire), `"hy4-preview"`) {
			t.Fatalf("wire %d changed model: %s", i, wire)
		}
	}
}

func TestConductorCodeBuddyFailoverAccountQuota(t *testing.T) {
	chats := 0
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		chats++
		if chats == 1 {
			return codeBuddyConductorJSON(429, `{"code":0,"msg":"account quota exceeded"}`, nil), nil
		}
		return codeBuddyConductorSSE(codeBuddyConductorSuccessSSE), nil
	}}
	a := codeBuddyConductorAuth("codebuddy-cond-quota-a", codebuddy.RealmCN, "user-qa", "TOKEN-QA", "hy4-preview")
	b := codeBuddyConductorAuth("codebuddy-cond-quota-b", codebuddy.RealmCN, "user-qb", "TOKEN-QB", "hy4-preview")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a, b)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	resp, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(string(resp.Payload), "Hi") {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if transport.count() != 2 {
		t.Fatalf("upstream calls = %d, want failover to second account", transport.count())
	}
}

func TestConductorCodeBuddyUnentitledNeverSelected(t *testing.T) {
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		return codeBuddyConductorSSE(codeBuddyConductorSuccessSSE), nil
	}}
	a := codeBuddyConductorAuth("codebuddy-cond-ent-a", codebuddy.RealmCN, "user-ea", "TOKEN-EA", "hy4-preview")
	b := codeBuddyConductorAuth("codebuddy-cond-ent-b", codebuddy.RealmCN, "user-eb", "TOKEN-EB", "hy4-preview-f")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a, b)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	if _, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if transport.count() == 0 {
		t.Fatal("no upstream call")
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	for _, call := range transport.calls {
		if !strings.Contains(call.token, "TOKEN-EA") {
			t.Fatalf("unentitled account served request: %+v", call)
		}
	}
}

func TestConductorCodeBuddyAliasRouting(t *testing.T) {
	var wire []byte
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(req.Body)
		wire = raw
		return codeBuddyConductorSSE(codeBuddyConductorSuccessSSE), nil
	}}
	a := codeBuddyConductorAuth("codebuddy-cond-alias", codebuddy.RealmGlobal, "user-alias", "TOKEN-ALIAS", "hy4-preview")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a)
	mgr.SetOAuthModelAlias(map[string][]internalconfig.OAuthModelAlias{
		"codebuddy": {{Name: "codebuddy-global-hy4-preview", Alias: "hy4", Fork: true}},
	})
	req, opts := codeBuddyConductorRequest("hy4")
	resp, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err != nil {
		t.Fatalf("alias execute: %v", err)
	}
	if !strings.Contains(string(wire), `"hy4-preview"`) {
		t.Fatalf("alias did not resolve to entitled upstream model: %s", wire)
	}
	if !strings.Contains(string(resp.Payload), "Hi") {
		t.Fatalf("payload = %s", resp.Payload)
	}
}

func TestConductorCodeBuddyInvalidRefreshFailsVisible(t *testing.T) {
	transport := &codeBuddyConductorTransport{handle: func(_ codeBuddyConductorCall, req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/token/refresh") {
			return codeBuddyConductorJSON(401, `{"code":12153,"msg":"Offline user session not found"}`, nil), nil
		}
		return codeBuddyConductorJSON(401, `{"code":0,"msg":"unauthorized"}`, nil), nil
	}}
	a := codeBuddyConductorAuth("codebuddy-cond-badrefresh", codebuddy.RealmCN, "user-br", "STALE", "hy4-preview")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	_, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err == nil {
		t.Fatal("expected visible failure")
	}
	// One chat attempt plus one refresh attempt: no success loop, no storm.
	if transport.count() != 2 {
		t.Fatalf("upstream calls = %d, want exactly chat+refresh", transport.count())
	}
	stored, ok := mgr.GetByID(a.ID)
	if !ok {
		t.Fatal("auth missing after failed refresh")
	}
	if stored.Metadata["access_token"] != "STALE" {
		t.Fatalf("stale token overwritten without rotation: %v", stored.Metadata["access_token"])
	}
}

func TestConductorCodeBuddyRefreshAndContinue(t *testing.T) {
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
	a := codeBuddyConductorAuth("codebuddy-cond-refresh", codebuddy.RealmCN, "user-refresh", "STALE", "hy4-preview")
	mgr, ctx := codeBuddyConductorSetup(t, transport, a)
	req, opts := codeBuddyConductorRequest("codebuddy-cn-hy4-preview")
	resp, err := mgr.Execute(ctx, []string{"codebuddy"}, req, opts)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(string(resp.Payload), "Hi") {
		t.Fatalf("payload = %s", resp.Payload)
	}
	if chats != 2 {
		t.Fatalf("chats = %d", chats)
	}
	stored, ok := mgr.GetByID(a.ID)
	if !ok {
		t.Fatal("auth missing after refresh")
	}
	if stored.Metadata["access_token"] != "ROTATED" {
		t.Fatalf("token not rotated: %v", stored.Metadata["access_token"])
	}
	if stored.Metadata["machine_id"] != "mid-codebuddy-cond-refresh" {
		t.Fatalf("device identity lost: %v", stored.Metadata)
	}
	if _, err := codebuddy.CatalogFromMetadata(stored.Metadata); err != nil {
		t.Fatalf("catalog lost: %v", err)
	}
}
