package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type codeBuddyMgmtRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f codeBuddyMgmtRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func codeBuddyMgmtJSON(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

type codeBuddySignalStore struct {
	*memoryAuthStore
	saved chan *coreauth.Auth
}

func (s *codeBuddySignalStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	path, err := s.memoryAuthStore.Save(ctx, auth)
	if err == nil {
		select {
		case s.saved <- auth:
		default:
		}
	}
	return path, err
}

func setupCodeBuddyMgmtTest(t *testing.T, transport http.RoundTripper) (*Handler, *codeBuddySignalStore) {
	t.Helper()
	previousFactory := codeBuddyManagementHTTPClient
	previousInterval := codeBuddyManagementPollInterval
	codeBuddyManagementHTTPClient = func(_ *config.Config) *http.Client {
		return &http.Client{Transport: transport}
	}
	codeBuddyManagementPollInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		codeBuddyManagementHTTPClient = previousFactory
		codeBuddyManagementPollInterval = previousInterval
	})
	store := &codeBuddySignalStore{memoryAuthStore: &memoryAuthStore{}, saved: make(chan *coreauth.Auth, 4)}
	handler := &Handler{cfg: &config.Config{}, tokenStore: store}
	return handler, store
}

func callCodeBuddyMgmtLogin(t *testing.T, handler *Handler, realm string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	target := "/v0/management/codebuddy-auth-url"
	if realm != "" {
		target += "?realm=" + realm
	}
	ctx.Request = httptest.NewRequest(http.MethodGet, target, nil)
	handler.RequestCodeBuddyToken(ctx)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v (%s)", err, recorder.Body.String())
	}
	return recorder.Code, response
}

func waitCodeBuddyMgmtSave(t *testing.T, store *codeBuddySignalStore) *coreauth.Auth {
	t.Helper()
	select {
	case auth := <-store.saved:
		return auth
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for save")
		return nil
	}
}

func waitCodeBuddyMgmtStatus(t *testing.T, state, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, status, _, _, completed, _ := GetOAuthSessionDetails(state)
		if completed && want == "completed" {
			return
		}
		if status == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state %s never reached %q", state, want)
}

func TestRequestCodeBuddyTokenSuccess(t *testing.T) {
	var transportCalls atomic.Int32
	handler, store := setupCodeBuddyMgmtTest(t, codeBuddyMgmtRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		transportCalls.Add(1)
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"state":"UPSTREAM-STATE","authUrl":"https://www.codebuddy.ai/auth"}}`), nil
		case "/v2/plugin/auth/token":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"accessToken":"A","refreshToken":"R","expiresIn":3600,"domain":"www.codebuddy.ai"}}`), nil
		case "/v2/plugin/login/account":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"uid":"mgmt-user","nickname":"Mgmt"}}`), nil
		case "/v3/config":
			return codeBuddyMgmtJSON(`{"code":0,"data":{"models":[{"id":"hy4-preview"}]}}`), nil
		default:
			return codeBuddyMgmtJSON(`{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`), nil
		}
	}))
	code, response := callCodeBuddyMgmtLogin(t, handler, "global")
	if code != 200 {
		t.Fatalf("code = %d (%v)", code, response)
	}
	if response["flow"] != "device" || response["realm"] != "global" {
		t.Fatalf("response = %v", response)
	}
	if response["expires_in"] != float64(900) {
		t.Fatalf("expires_in = %v", response["expires_in"])
	}
	localState, _ := response["state"].(string)
	if localState == "" || localState == "UPSTREAM-STATE" || !strings.HasPrefix(localState, "cb-") {
		t.Fatalf("state = %q", localState)
	}
	saved := waitCodeBuddyMgmtSave(t, store)
	if saved.Provider != "codebuddy" {
		t.Fatalf("provider = %q", saved.Provider)
	}
	catalog, err := codebuddy.CatalogFromMetadata(saved.Metadata)
	if err != nil || len(catalog.Models) != 1 {
		t.Fatalf("catalog = %+v %v", catalog, err)
	}
	waitCodeBuddyMgmtStatus(t, localState, "completed")
	if transportCalls.Load() == 0 {
		t.Fatal("no upstream calls")
	}
}

func TestRequestCodeBuddyTokenInvalidRealm(t *testing.T) {
	var transportCalls atomic.Int32
	handler, _ := setupCodeBuddyMgmtTest(t, codeBuddyMgmtRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		transportCalls.Add(1)
		return codeBuddyMgmtJSON(`{}`), nil
	}))
	for _, realm := range []string{"", "eu"} {
		if code, _ := callCodeBuddyMgmtLogin(t, handler, realm); code != http.StatusBadRequest {
			t.Fatalf("realm %q: code = %d", realm, code)
		}
	}
	if transportCalls.Load() != 0 {
		t.Fatal("Tencent contacted before validation")
	}
}

func TestRequestCodeBuddyTokenFailedCatalog(t *testing.T) {
	handler, store := setupCodeBuddyMgmtTest(t, codeBuddyMgmtRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.cn/auth"}}`), nil
		case "/v2/plugin/auth/token":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"accessToken":"A","refreshToken":"R","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
		case "/v2/plugin/login/account":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"uid":"u"}}`), nil
		default:
			return &http.Response{StatusCode: 500, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
	}))
	code, response := callCodeBuddyMgmtLogin(t, handler, "cn")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	localState, _ := response["state"].(string)
	waitCodeBuddyMgmtStatus(t, localState, "CodeBuddy model discovery failed")
	select {
	case saved := <-store.saved:
		t.Fatalf("saved despite failed catalog: %+v", saved)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestRequestCodeBuddyTokenCancelDuringPoll(t *testing.T) {
	polls := make(chan struct{}, 16)
	handler, store := setupCodeBuddyMgmtTest(t, codeBuddyMgmtRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.cn/auth"}}`), nil
		case "/v2/plugin/auth/token":
			polls <- struct{}{}
			return codeBuddyMgmtJSON(`{"code":1,"msg":"login ing","data":null}`), nil
		default:
			return codeBuddyMgmtJSON(`{}`), nil
		}
	}))
	code, response := callCodeBuddyMgmtLogin(t, handler, "cn")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	localState, _ := response["state"].(string)
	select {
	case <-polls:
	case <-time.After(10 * time.Second):
		t.Fatal("no poll observed")
	}
	if !CancelOAuthSession(localState) {
		t.Fatal("cancel failed")
	}
	waitCodeBuddyMgmtQuiescent(t, polls)
	select {
	case saved := <-store.saved:
		t.Fatalf("saved after cancel: %+v", saved)
	default:
	}
}

func waitCodeBuddyMgmtQuiescent(t *testing.T, polls chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	stableSince := time.Now()
	var lastCount int
	count := 0
	for {
		select {
		case <-polls:
			count++
			stableSince = time.Now()
		case <-time.After(20 * time.Millisecond):
			if count == lastCount && time.Since(stableSince) > 100*time.Millisecond {
				return
			}
			lastCount = count
		case <-time.After(time.Until(deadline)):
			t.Fatal("worker did not quiesce")
		}
	}
}

func TestRequestCodeBuddyTokenCancelDuringCatalog(t *testing.T) {
	catalogStarted := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	handler, store := setupCodeBuddyMgmtTest(t, codeBuddyMgmtRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.cn/auth"}}`), nil
		case "/v2/plugin/auth/token":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"accessToken":"A","refreshToken":"R","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
		case "/v2/plugin/login/account":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"uid":"u"}}`), nil
		default:
			select {
			case <-catalogStarted:
			default:
				close(catalogStarted)
			}
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-release:
				return codeBuddyMgmtJSON(`{"code":0,"data":{"models":[{"id":"hy4-preview"}],"agents":[{"name":"cli","models":["hy4-preview"]}]}}`), nil
			}
		}
	}))
	code, response := callCodeBuddyMgmtLogin(t, handler, "cn")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	localState, _ := response["state"].(string)
	select {
	case <-catalogStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("catalog not started")
	}
	if !CancelOAuthSession(localState) {
		t.Fatal("cancel failed")
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case saved := <-store.saved:
		t.Fatalf("saved after cancel: %+v", saved)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestRequestCodeBuddyTokenSessionIsolation(t *testing.T) {
	var starts atomic.Int32
	handler, store := setupCodeBuddyMgmtTest(t, codeBuddyMgmtRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			n := starts.Add(1)
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"state":"UP-` + string(rune('A'+n-1)) + `","authUrl":"https://www.codebuddy.cn/auth"}}`), nil
		case "/v2/plugin/auth/token":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"accessToken":"A-` + req.URL.Query().Get("state") + `","refreshToken":"R","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
		case "/v2/plugin/login/account":
			return codeBuddyMgmtJSON(`{"code":0,"msg":"ok","data":{"uid":"user-` + req.URL.Query().Get("state") + `"}}`), nil
		case "/v3/config":
			return codeBuddyMgmtJSON(`{"code":0,"data":{"models":[{"id":"hy4-preview"}]}}`), nil
		default:
			return codeBuddyMgmtJSON(`{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`), nil
		}
	}))
	_, first := callCodeBuddyMgmtLogin(t, handler, "cn")
	_, second := callCodeBuddyMgmtLogin(t, handler, "cn")
	firstState, _ := first["state"].(string)
	secondState, _ := second["state"].(string)
	if firstState == secondState {
		t.Fatal("local states collide")
	}
	if !CancelOAuthSession(secondState) {
		t.Fatal("cancel failed")
	}
	saved := waitCodeBuddyMgmtSave(t, store)
	if saved.Metadata["uid"] != "user-UP-A" {
		t.Fatalf("saved = %v", saved.Metadata)
	}
	waitCodeBuddyMgmtStatus(t, firstState, "completed")
	select {
	case extra := <-store.saved:
		t.Fatalf("second session saved: %+v", extra)
	case <-time.After(300 * time.Millisecond):
	}
}
