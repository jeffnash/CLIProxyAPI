package tui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOAuthProvidersIncludeCodeBuddy(t *testing.T) {
	want := map[string]string{
		"CodeBuddy CN":     "codebuddy-auth-url?realm=cn",
		"CodeBuddy Global": "codebuddy-auth-url?realm=global",
		"WorkBuddy Global": "codebuddy-auth-url?realm=workbuddy-global",
	}
	for name, path := range want {
		found := false
		for _, provider := range oauthProviders {
			if provider.name == name {
				found = true
				if provider.apiPath != path {
					t.Fatalf("%s: apiPath = %q", name, provider.apiPath)
				}
				if !provider.deviceFlow {
					t.Fatalf("%s: deviceFlow = false", name)
				}
			}
		}
		if !found {
			t.Fatalf("%s missing", name)
		}
	}
}

func TestStartOAuthPreservesRealm(t *testing.T) {
	var gotPath, gotRealm, gotWebUI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotRealm = r.URL.Query().Get("realm")
		gotWebUI = r.URL.Query().Get("is_webui")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://example.com/auth","state":"s1","flow":"device","expires_in":900}`))
	}))
	defer server.Close()
	model := newOAuthTabModel(&Client{baseURL: server.URL, http: server.Client()})
	for _, provider := range oauthProviders {
		if !strings.Contains(provider.apiPath, "codebuddy") {
			continue
		}
		gotPath, gotRealm, gotWebUI = "", "", ""
		cmd := model.startOAuth(provider, 1)
		msg, ok := cmd().(oauthStartMsg)
		if !ok {
			t.Fatalf("%s: message = %T", provider.name, cmd())
		}
		if msg.err != nil {
			t.Fatalf("%s: %v", provider.name, msg.err)
		}
		if gotPath != "/v0/management/codebuddy-auth-url" {
			t.Fatalf("%s: path = %q", provider.name, gotPath)
		}
		if gotRealm == "" || gotWebUI != "true" {
			t.Fatalf("%s: realm=%q webui=%q", provider.name, gotRealm, gotWebUI)
		}
		if !msg.deviceFlow || msg.expiresIn != 900 {
			t.Fatalf("%s: device=%v expires=%d", provider.name, msg.deviceFlow, msg.expiresIn)
		}
	}
}

func TestStartOAuthExistingProviderUnchanged(t *testing.T) {
	var rawQuery string
	var queryCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		queryCount = len(r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://example.com/auth","state":"s1","flow":"device"}`))
	}))
	defer server.Close()
	model := newOAuthTabModel(&Client{baseURL: server.URL, http: server.Client()})
	cmd := model.startOAuth(oauthProvider{name: "Kimi", apiPath: "kimi-auth-url", deviceFlow: true}, 1)
	if msg := cmd().(oauthStartMsg); msg.err != nil {
		t.Fatalf("start: %v", msg.err)
	}
	if rawQuery != "is_webui=true" || queryCount != 1 {
		t.Fatalf("query = %q", rawQuery)
	}
}

func TestStartOAuthFailureNamesProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()
	model := newOAuthTabModel(&Client{baseURL: server.URL, http: server.Client()})
	msg := model.startOAuth(oauthProvider{name: "CodeBuddy CN", apiPath: "codebuddy-auth-url?realm=cn", deviceFlow: true}, 1)().(oauthStartMsg)
	if msg.err == nil || !strings.Contains(msg.err.Error(), "CodeBuddy CN") {
		t.Fatalf("err = %v", msg.err)
	}
}
