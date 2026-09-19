package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type codeBuddyAuthRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f codeBuddyAuthRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func codeBuddyAuthJSON(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func overrideCodeBuddyAuthHooks(t *testing.T, transport http.RoundTripper) *int {
	t.Helper()
	previousFactory := codeBuddyHTTPClientFactory
	previousBrowser := codeBuddyOpenBrowser
	previousAvailable := codeBuddyBrowserAvailable
	previousWait := codeBuddyWaitForPoll
	calls := 0
	codeBuddyHTTPClientFactory = func(_ *config.Config) *http.Client {
		return &http.Client{Transport: transport}
	}
	codeBuddyOpenBrowser = func(_ string) error {
		calls++
		return nil
	}
	codeBuddyBrowserAvailable = func() bool { return true }
	codeBuddyWaitForPoll = func(ctx context.Context, _ time.Duration) error {
		return ctx.Err()
	}
	t.Cleanup(func() {
		codeBuddyHTTPClientFactory = previousFactory
		codeBuddyOpenBrowser = previousBrowser
		codeBuddyBrowserAvailable = previousAvailable
		codeBuddyWaitForPoll = previousWait
	})
	return &calls
}

func codeBuddySeedTestBundle(t *testing.T) string {
	t.Helper()
	tools := true
	creds := codebuddy.Credentials{
		Realm: codebuddy.RealmGlobal, UID: "seed-uid", Nickname: "seed",
		AccessToken: "SYNTHETIC_ACCESS", RefreshToken: "SYNTHETIC_REFRESH",
		Domain: "www.codebuddy.ai", Expired: time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC),
	}
	catalog := codebuddy.Catalog{
		Models:  []codebuddy.Model{{ID: "hy4-preview", MaxInputTokens: 100, MaxOutputTokens: 10, SupportsTools: &tools}},
		Sources: []string{"v3"},
	}
	raw, err := json.Marshal(codebuddy.Metadata(creds, catalog, time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)))
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return string(raw)
}

func TestSeedCodeBuddyAuthFromEnv(t *testing.T) {
	bundle := codeBuddySeedTestBundle(t)
	t.Run("writes missing record", func(t *testing.T) {
		t.Setenv(CodeBuddyAuthJSONEnv, bundle)
		dir := t.TempDir()
		seeded, err := SeedCodeBuddyAuthFromEnv(dir)
		if err != nil || !seeded {
			t.Fatalf("seeded=%v err=%v", seeded, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 1 {
			t.Fatalf("entries=%v err=%v", entries, err)
		}
		if !strings.HasPrefix(entries[0].Name(), "codebuddy-global-") {
			t.Fatalf("filename=%q", entries[0].Name())
		}
		info, _ := entries[0].Info()
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("perm=%o", info.Mode().Perm())
		}
		raw, _ := os.ReadFile(filepath.Join(dir, entries[0].Name()))
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded["uid"] != "seed-uid" {
			t.Fatalf("seeded content invalid: %s", raw)
		}
	})
	t.Run("existing file wins", func(t *testing.T) {
		t.Setenv(CodeBuddyAuthJSONEnv, bundle)
		dir := t.TempDir()
		seeded, err := SeedCodeBuddyAuthFromEnv(dir)
		if err != nil || !seeded {
			t.Fatalf("first seed: %v %v", seeded, err)
		}
		entries, _ := os.ReadDir(dir)
		target := filepath.Join(dir, entries[0].Name())
		if err := os.WriteFile(target, []byte(`{"type":"codebuddy","note":"rotated"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		seeded, err = SeedCodeBuddyAuthFromEnv(dir)
		if err != nil || seeded {
			t.Fatalf("reseed clobbered: %v %v", seeded, err)
		}
		raw, _ := os.ReadFile(target)
		if !strings.Contains(string(raw), "rotated") {
			t.Fatalf("existing file changed: %s", raw)
		}
	})
	t.Run("empty env is a no-op", func(t *testing.T) {
		t.Setenv(CodeBuddyAuthJSONEnv, "")
		seeded, err := SeedCodeBuddyAuthFromEnv(t.TempDir())
		if err != nil || seeded {
			t.Fatalf("seeded=%v err=%v", seeded, err)
		}
	})
	t.Run("rejects garbage", func(t *testing.T) {
		for _, bad := range []string{
			"not-json",
			`{"type":"codex"}`,
			`{"type":"codebuddy"}`,
			`{"type":"codebuddy","realm":"global","uid":"u","access_token":"a"}`,
		} {
			t.Setenv(CodeBuddyAuthJSONEnv, bad)
			if _, err := SeedCodeBuddyAuthFromEnv(t.TempDir()); err == nil {
				t.Fatalf("accepted %q", bad)
			}
		}
	})
}

func TestCodeBuddyAuthenticatorBasics(t *testing.T) {
	authenticator := NewCodeBuddyAuthenticator()
	if authenticator.Provider() != "codebuddy" {
		t.Fatalf("Provider() = %q", authenticator.Provider())
	}
	lead := authenticator.RefreshLead()
	if lead == nil || *lead != 5*time.Minute {
		t.Fatalf("RefreshLead() = %v", lead)
	}
}

func TestCodeBuddyAuthRecord(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	creds := codebuddy.Credentials{Realm: codebuddy.RealmGlobal, AccessToken: "A", UID: "u", Nickname: "Nick"}
	record := NewCodeBuddyAuthRecord(creds, codebuddy.Catalog{Models: []codebuddy.Model{{ID: "hy4-preview"}}}, now)
	if record.Provider != "codebuddy" || record.FileName == "" || record.ID != record.FileName {
		t.Fatalf("record = %+v", record)
	}
	if record.Label != "Nick" || record.Storage != nil {
		t.Fatalf("record = %+v", record)
	}
	if record.Metadata["realm"] != "global" || record.Metadata["uid"] != "u" {
		t.Fatalf("metadata = %v", record.Metadata)
	}
	anonymous := creds
	anonymous.Nickname = ""
	if got := NewCodeBuddyAuthRecord(anonymous, codebuddy.Catalog{}, now).Label; got != "CodeBuddy global" {
		t.Fatalf("label = %q", got)
	}
}

func TestCodeBuddyLoginInvalidRealm(t *testing.T) {
	transportCalls := 0
	overrideCodeBuddyAuthHooks(t, codeBuddyAuthRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		transportCalls++
		return codeBuddyAuthJSON(200, `{}`), nil
	}))
	_, err := NewCodeBuddyAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{NoBrowser: true, Metadata: map[string]string{"realm": "eu"}})
	if err == nil {
		t.Fatal("invalid realm accepted")
	}
	if transportCalls != 0 {
		t.Fatal("network call before validation")
	}
}

func TestCodeBuddyLoginFailedDiscovery(t *testing.T) {
	polls := 0
	overrideCodeBuddyAuthHooks(t, codeBuddyAuthRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.cn/auth"}}`), nil
		case "/v2/plugin/auth/token":
			polls++
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"accessToken":"A","refreshToken":"R","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
		case "/v2/plugin/login/account":
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"uid":"u","nickname":"N"}}`), nil
		default:
			return codeBuddyAuthJSON(500, `{"code":500,"msg":"boom"}`), nil
		}
	}))
	record, err := NewCodeBuddyAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{NoBrowser: true})
	if err == nil || record != nil {
		t.Fatalf("record=%v err=%v", record, err)
	}
	if !strings.Contains(err.Error(), "discovery") {
		t.Fatalf("err = %v", err)
	}
	if polls != 1 {
		t.Fatalf("polls = %d", polls)
	}
}

func TestCodeBuddyLoginSuccess(t *testing.T) {
	browserCalls := overrideCodeBuddyAuthHooks(t, codeBuddyAuthRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.cn/auth"}}`), nil
		case "/v2/plugin/auth/token":
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"accessToken":"A","refreshToken":"R","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
		case "/v2/plugin/login/account":
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"uid":"u","nickname":"N"}}`), nil
		case "/v3/config":
			return codeBuddyAuthJSON(200, `{"code":0,"data":{"models":[{"id":"hy4-preview"}]}}`), nil
		default:
			return codeBuddyAuthJSON(200, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`), nil
		}
	}))
	record, err := NewCodeBuddyAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{Metadata: map[string]string{"realm": "cn"}})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if record.Provider != "codebuddy" || *browserCalls != 1 {
		t.Fatalf("record=%+v browser=%d", record, *browserCalls)
	}
	catalog, err := codebuddy.CatalogFromMetadata(record.Metadata)
	if err != nil || len(catalog.Models) != 1 {
		t.Fatalf("catalog=%+v err=%v", catalog, err)
	}
}

func TestCodeBuddyLoginNoBrowser(t *testing.T) {
	browserCalls := overrideCodeBuddyAuthHooks(t, codeBuddyAuthRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.cn/auth"}}`), nil
		case "/v2/plugin/auth/token":
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"accessToken":"A","refreshToken":"R","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
		case "/v2/plugin/login/account":
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"uid":"u"}}`), nil
		default:
			return codeBuddyAuthJSON(200, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`), nil
		}
	}))
	if _, err := NewCodeBuddyAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{NoBrowser: true}); err != nil {
		t.Fatalf("Login: %v", err)
	}
	if *browserCalls != 0 {
		t.Fatal("browser opened despite no-browser")
	}
}

func TestCodeBuddyLoginCancellation(t *testing.T) {
	overrideCodeBuddyAuthHooks(t, codeBuddyAuthRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v2/plugin/auth/state" {
			return codeBuddyAuthJSON(200, `{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.cn/auth"}}`), nil
		}
		return codeBuddyAuthJSON(200, `{"code":1,"msg":"login ing","data":null}`), nil
	}))
	previousWait := codeBuddyWaitForPoll
	codeBuddyWaitForPoll = func(ctx context.Context, _ time.Duration) error {
		return context.Canceled
	}
	defer func() { codeBuddyWaitForPoll = previousWait }()
	if _, err := NewCodeBuddyAuthenticator().Login(context.Background(), &config.Config{}, &LoginOptions{NoBrowser: true}); err == nil {
		t.Fatal("cancellation ignored")
	}
}
