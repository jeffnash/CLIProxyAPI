package codebuddy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

var testFixedTime = time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func fakeJSONResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(transport http.RoundTripper) *Client {
	client := NewClient(&http.Client{Transport: transport})
	client.now = func() time.Time { return testFixedTime }
	return client
}

func readRequestBody(t *testing.T, req *http.Request) string {
	t.Helper()
	if req.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return string(raw)
}

func TestLoginProtocol(t *testing.T) {
	var requests []*http.Request
	var bodies []string
	pending := true
	client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req)
		bodies = append(bodies, readRequestBody(t, req))
		switch req.URL.Path {
		case "/v2/plugin/auth/state":
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"state":"ST-1","authUrl":"https://www.codebuddy.cn/authorization-example"}}`), nil
		case "/v2/plugin/auth/token":
			if pending {
				return fakeJSONResponse(200, `{"code":1,"msg":"login ing","data":null}`), nil
			}
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"accessToken":"SYNTHETIC_ACCESS","refreshToken":"SYNTHETIC_REFRESH","expiresIn":3600,"domain":"www.codebuddy.cn"}}`), nil
		case "/v2/plugin/login/account":
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"uid":"fixture-user","enterpriseId":"fixture-enterprise","nickname":"Fixture"}}`), nil
		default:
			t.Fatalf("unexpected path %s", req.URL.Path)
			return nil, nil
		}
	}))
	ctx := context.Background()

	session, err := client.StartLogin(ctx, RealmCN)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if session.State != "ST-1" || session.Realm != RealmCN {
		t.Fatalf("session = %+v", session)
	}
	if !session.ExpiresAt.Equal(testFixedTime.Add(LoginLifetime)) {
		t.Fatalf("session expiry = %v", session.ExpiresAt)
	}
	startReq := requests[0]
	if startReq.Method != http.MethodPost || startReq.URL.Host != "copilot.tencent.com" || startReq.URL.Query().Get("platform") != "CLI" {
		t.Fatalf("start request = %s %s", startReq.Method, startReq.URL.String())
	}
	if bodies[0] != "{}" {
		t.Fatalf("start body = %q", bodies[0])
	}
	if got := startReq.Header.Get("Origin"); got != "https://www.codebuddy.cn" {
		t.Fatalf("Origin = %q", got)
	}
	if startReq.Header.Get("Authorization") != "" || startReq.Header.Get("X-Refresh-Token") != "" {
		t.Fatal("start request must not carry credentials")
	}

	_, ready, err := client.PollLogin(ctx, session)
	if err != nil {
		t.Fatalf("pending poll: %v", err)
	}
	if ready {
		t.Fatal("pending poll reported ready")
	}
	if len(requests) != 2 {
		t.Fatalf("pending poll made %d requests, want 2 (no account fetch)", len(requests))
	}

	pending = false
	credentials, ready, err := client.PollLogin(ctx, session)
	if err != nil {
		t.Fatalf("ready poll: %v", err)
	}
	if !ready {
		t.Fatal("ready poll reported pending")
	}
	if credentials.Realm != RealmCN || credentials.UID != "fixture-user" || credentials.AccessToken != "SYNTHETIC_ACCESS" || credentials.RefreshToken != "SYNTHETIC_REFRESH" {
		t.Fatalf("credentials = %+v", credentials)
	}
	if !credentials.Expired.Equal(testFixedTime.Add(time.Hour)) {
		t.Fatalf("expiry = %v", credentials.Expired)
	}
	if len(credentials.MachineID) != 32 || len(credentials.SessionID) != 32 {
		t.Fatalf("device ids = %q %q", credentials.MachineID, credentials.SessionID)
	}
	if len(requests) != 4 {
		t.Fatalf("total requests = %d, want 4", len(requests))
	}
	for _, req := range requests {
		if req.URL.Host != "copilot.tencent.com" {
			t.Fatalf("request host = %s, want one realm", req.URL.Host)
		}
	}
	accountReq := requests[3]
	if accountReq.Header.Get("Authorization") != "Bearer SYNTHETIC_ACCESS" {
		t.Fatalf("account auth = %q", accountReq.Header.Get("Authorization"))
	}
	if accountReq.URL.Query().Get("state") != "ST-1" {
		t.Fatalf("account state = %q", accountReq.URL.Query().Get("state"))
	}
}

func TestLoginProtocolRealmHosts(t *testing.T) {
	cases := []struct {
		realm Realm
		host  string
	}{
		{RealmCN, "copilot.tencent.com"},
		{RealmGlobal, "www.codebuddy.ai"},
		{RealmWorkBuddyGlobal, "www.workbuddy.ai"},
	}
	for _, tc := range cases {
		var host string
		client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			host = req.URL.Host
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.ai/authorization-example"}}`), nil
		}))
		if _, err := client.StartLogin(context.Background(), tc.realm); err != nil {
			t.Fatalf("%s: %v", tc.realm, err)
		}
		if host != tc.host {
			t.Fatalf("%s: host = %s, want %s", tc.realm, host, tc.host)
		}
	}
}

func TestStartLoginRejectsBadAuthURL(t *testing.T) {
	for _, authURL := range []string{
		"http://www.codebuddy.cn/auth",
		"https://user@www.codebuddy.cn/auth",
		"https://evil.test/auth",
		"https://codebuddy.cn.evil.test/auth",
		"https://copilot.tencent.com/auth",
		"not-a-url",
		"",
	} {
		client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"state":"S","authUrl":"`+authURL+`"}}`), nil
		}))
		if _, err := client.StartLogin(context.Background(), RealmCN); err == nil {
			t.Fatalf("authUrl %q accepted", authURL)
		}
	}
}

func TestPollLoginRejectsCrossRealmDomain(t *testing.T) {
	client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v2/plugin/auth/token" {
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"accessToken":"A","refreshToken":"R","expiresIn":60,"domain":"www.codebuddy.ai"}}`), nil
		}
		t.Fatalf("unexpected path %s (domain must not redirect calls)", req.URL.Path)
		return nil, nil
	}))
	session := LoginSession{State: "S", Realm: RealmCN, ExpiresAt: testFixedTime.Add(time.Minute)}
	if _, _, err := client.PollLogin(context.Background(), session); err == nil {
		t.Fatal("cross-realm token domain accepted")
	}
}

func TestPollLoginTransportErrorRedactsState(t *testing.T) {
	client := testClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic transport failure")
	}))
	session := LoginSession{State: "SENSITIVE_LOGIN_STATE", Realm: RealmGlobal, ExpiresAt: testFixedTime.Add(time.Hour)}
	_, _, err := client.PollLogin(context.Background(), session)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), "SENSITIVE_LOGIN_STATE") {
		t.Fatalf("login state leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "www.codebuddy.ai/v2/plugin/auth/token") {
		t.Fatalf("host/path lost from error: %v", err)
	}
}

func TestPollLoginAccountLookupRedactsState(t *testing.T) {
	client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v2/plugin/auth/token" {
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"accessToken":"A","refreshToken":"R","expiresIn":60,"domain":"www.codebuddy.ai"}}`), nil
		}
		return nil, errors.New("synthetic account lookup failure")
	}))
	session := LoginSession{State: "SENSITIVE_LOOKUP_STATE", Realm: RealmGlobal, ExpiresAt: testFixedTime.Add(time.Hour)}
	_, _, err := client.PollLogin(context.Background(), session)
	if err == nil {
		t.Fatal("expected lookup error")
	}
	if strings.Contains(err.Error(), "SENSITIVE_LOOKUP_STATE") {
		t.Fatalf("login state leaked into error: %v", err)
	}
}

func TestRefreshProtocol(t *testing.T) {
	base := Credentials{
		Realm: RealmGlobal, AccessToken: "OLD_ACCESS", RefreshToken: "OLD_REFRESH",
		UID: "u1", EnterpriseID: "", Nickname: "N", Domain: "www.codebuddy.ai",
		MachineID: "m", SessionID: "s", Expired: testFixedTime,
	}
	t.Run("rotates tokens", func(t *testing.T) {
		var req *http.Request
		client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			req = r
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"accessToken":"NEW_ACCESS","refreshToken":"NEW_REFRESH","expiresIn":7200,"domain":"www.codebuddy.ai"}}`), nil
		}))
		got, err := client.Refresh(context.Background(), base)
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if got.AccessToken != "NEW_ACCESS" || got.RefreshToken != "NEW_REFRESH" {
			t.Fatalf("tokens = %q %q", got.AccessToken, got.RefreshToken)
		}
		if !got.Expired.Equal(testFixedTime.Add(2 * time.Hour)) {
			t.Fatalf("expiry = %v", got.Expired)
		}
		if got.UID != "u1" || got.MachineID != "m" || got.SessionID != "s" || got.Nickname != "N" {
			t.Fatalf("identity not preserved: %+v", got)
		}
		if req.URL.Host != "www.codebuddy.ai" || req.URL.Path != "/v2/plugin/auth/token/refresh" {
			t.Fatalf("refresh URL = %s", req.URL.String())
		}
		if req.Header.Get("X-Refresh-Token") != "OLD_REFRESH" || req.Header.Get("X-Auth-Refresh-Source") != "plugin" {
			t.Fatalf("refresh headers = %v", req.Header)
		}
		if req.Header.Get("Authorization") != "" {
			t.Fatal("refresh must not carry bearer authorization")
		}
	})
	t.Run("preserves omitted fields", func(t *testing.T) {
		client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"accessToken":"NEW_ACCESS"}}`), nil
		}))
		got, err := client.Refresh(context.Background(), base)
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if got.RefreshToken != "OLD_REFRESH" || got.Domain != "www.codebuddy.ai" || !got.Expired.Equal(testFixedTime) {
			t.Fatalf("omitted fields not preserved: %+v", got)
		}
	})
	t.Run("rejects missing access token", func(t *testing.T) {
		client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"refreshToken":"R"}}`), nil
		}))
		if _, err := client.Refresh(context.Background(), base); err == nil {
			t.Fatal("missing access token accepted")
		}
	})
	t.Run("rejects malformed expiry", func(t *testing.T) {
		client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"accessToken":"A","expiresIn":-5}}`), nil
		}))
		if _, err := client.Refresh(context.Background(), base); err == nil {
			t.Fatal("negative expiresIn accepted")
		}
	})
	t.Run("rejects missing refresh token without network", func(t *testing.T) {
		called := false
		client := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return fakeJSONResponse(200, `{}`), nil
		}))
		without := base
		without.RefreshToken = ""
		if _, err := client.Refresh(context.Background(), without); err == nil {
			t.Fatal("missing refresh token accepted")
		}
		if called {
			t.Fatal("refresh attempted without a refresh token")
		}
	})
}

func TestCrossHostRedirectRejected(t *testing.T) {
	attackerCalls := 0
	client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "attacker.test" {
			attackerCalls++
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{}}`), nil
		}
		resp := fakeJSONResponse(302, "")
		resp.Header.Set("Location", "https://attacker.test/steal")
		return resp, nil
	}))
	creds := Credentials{Realm: RealmCN, AccessToken: "A", RefreshToken: "R", UID: "u", Expired: testFixedTime}
	if _, err := client.Refresh(context.Background(), creds); err == nil {
		t.Fatal("redirect accepted")
	} else if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("error = %v", err)
	}
	if attackerCalls != 0 {
		t.Fatalf("attacker invocations = %d", attackerCalls)
	}
	if _, err := client.StartLogin(context.Background(), RealmCN); err == nil {
		t.Fatal("start-login redirect accepted")
	}
	if attackerCalls != 0 {
		t.Fatalf("attacker invocations = %d", attackerCalls)
	}
}

func TestAuthEnvelopeValidation(t *testing.T) {
	start := func(body string, status int) error {
		client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return fakeJSONResponse(status, body), nil
		}))
		_, err := client.StartLogin(context.Background(), RealmCN)
		return err
	}
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"nonzero code":  {`{"code":11140,"msg":"request illegal","data":null}`, 200},
		"unauthorized":  {`{"code":0,"msg":"ok","data":{}}`, 401},
		"forbidden":     {`{"code":0,"msg":"ok","data":{}}`, 403},
		"invalid json":  {`{not json`, 200},
		"missing code":  {`{"msg":"ok","data":{}}`, 200},
		"missing data":  {`{"code":0,"msg":"ok"}`, 200},
		"missing state": {`{"code":0,"msg":"ok","data":{"authUrl":"https://www.codebuddy.cn/x"}}`, 200},
		"server error":  {`{"code":0,"msg":"ok","data":{}}`, 500},
		"oversized":     {`{"code":0,"msg":"` + strings.Repeat("x", int(AuthEnvelopeLimit)) + `","data":{}}`, 200},
	} {
		if err := start(tc.body, tc.status); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestAuthErrorsRedactTokens(t *testing.T) {
	client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return fakeJSONResponse(200, `{"code":11140,"msg":"request illegal for SYNTHETIC_ACCESS / SYNTHETIC_REFRESH","data":null}`), nil
	}))
	creds := Credentials{Realm: RealmCN, AccessToken: "SYNTHETIC_ACCESS", RefreshToken: "SYNTHETIC_REFRESH", UID: "u", Expired: testFixedTime}
	_, err := client.Refresh(context.Background(), creds)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SYNTHETIC_ACCESS") || strings.Contains(err.Error(), "SYNTHETIC_REFRESH") {
		t.Fatalf("token leaked in error: %v", err)
	}
}

func TestAuthCancellation(t *testing.T) {
	t.Run("expired session makes no request", func(t *testing.T) {
		called := false
		client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			called = true
			return fakeJSONResponse(200, `{}`), nil
		}))
		session := LoginSession{State: "S", Realm: RealmCN, ExpiresAt: testFixedTime.Add(-time.Second)}
		if _, _, err := client.PollLogin(context.Background(), session); err == nil {
			t.Fatal("expired session accepted")
		}
		if called {
			t.Fatal("expired session triggered a request")
		}
	})
	t.Run("canceled context aborts poll", func(t *testing.T) {
		started := make(chan struct{})
		client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(started)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			session := LoginSession{State: "S", Realm: RealmCN, ExpiresAt: testFixedTime.Add(time.Minute)}
			_, _, err := client.PollLogin(ctx, session)
			done <- err
		}()
		<-started
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("canceled poll succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("canceled poll did not return")
		}
	})
}

func TestChatHeaders(t *testing.T) {
	capture := func(creds Credentials, caller http.Header) http.Header {
		var captured http.Header
		client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Header.Clone()
			return fakeJSONResponse(200, `data: [DONE]`), nil
		}))
		resp, err := client.OpenChat(context.Background(), creds, []byte(`{"model":"hy4-preview"}`), caller)
		if err != nil {
			t.Fatalf("OpenChat: %v", err)
		}
		_ = resp.Body.Close()
		return captured
	}
	base := Credentials{
		Realm: RealmCN, AccessToken: "AT", RefreshToken: "RT", UID: "u1",
		EnterpriseID: "e1", Domain: "www.codebuddy.cn", DeviceToken: "DT",
		MachineID: "mid", SessionID: "sid", Expired: testFixedTime,
	}
	header := capture(base, nil)
	for key, want := range map[string]string{
		"Authorization":   "Bearer AT",
		"X-User-Id":       "u1",
		"X-Enterprise-Id": "e1",
		"X-Domain":        "www.codebuddy.cn",
		"X-Machine-Id":    "mid",
		"X-Session-Id":    "sid",
		"X-Device-Token":  "DT",
		"Accept":          "text/event-stream",
	} {
		if got := header.Get(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if header.Get("X-Refresh-Token") != "" {
		t.Fatal("chat carries refresh token")
	}
	if header.Get("X-Request-Id") == "" || header.Get("X-Conversation-Message-Id") == "" || header.Get("X-Trace-Id") == "" {
		t.Fatal("chat missing trace headers")
	}
	if header.Get("X-Request-Id") != header.Get("X-Conversation-Message-Id") {
		t.Fatal("request and message ids differ")
	}

	global := base
	global.Realm = RealmGlobal
	global.EnterpriseID = ""
	header = capture(global, nil)
	if header.Get("X-No-Enterprise-Id") != "1" || header.Get("X-Domain") != "www.codebuddy.ai" {
		t.Fatalf("global headers = %v", header)
	}
	if header.Get("X-Enterprise-Id") != "" {
		t.Fatal("global chat carries enterprise id")
	}

	workbuddy := base
	workbuddy.Realm = RealmWorkBuddyGlobal
	header = capture(workbuddy, nil)
	if header.Get("X-Domain") != "www.workbuddy.ai" {
		t.Fatalf("workbuddy domain = %q", header.Get("X-Domain"))
	}

	empty := base
	empty.EnterpriseID = ""
	empty.Domain = ""
	header = capture(empty, nil)
	if header.Get("X-No-Enterprise-Id") != "1" || header.Get("X-No-Department-Info") != "1" {
		t.Fatalf("empty identity headers = %v", header)
	}

	caller := http.Header{
		"Authorization":   []string{"Bearer EVIL"},
		"X-User-Id":       []string{"evil"},
		"X-Refresh-Token": []string{"EVIL"},
		"Cookie":          []string{"a=b"},
		"X-Forwarded-For": []string{"1.2.3.4"},
		"X-Real-Ip":       []string{"1.2.3.4"},
		"X-Domain":        []string{"evil.test"},
		"User-Agent":      []string{"CustomAgent/1.0"},
		"X-Trace-Id":      []string{"trace-123"},
	}
	header = capture(base, caller)
	if header.Get("Authorization") != "Bearer AT" || header.Get("X-User-Id") != "u1" || header.Get("X-Domain") != "www.codebuddy.cn" {
		t.Fatalf("caller overrode identity: %v", header)
	}
	if header.Get("Cookie") != "" || header.Get("X-Forwarded-For") != "" || header.Get("X-Real-Ip") != "" {
		t.Fatalf("caller forwarding headers leaked: %v", header)
	}
	if header.Get("User-Agent") != "CustomAgent/1.0" {
		t.Fatalf("user-agent override lost: %q", header.Get("User-Agent"))
	}
	if header.Get("X-Trace-Id") != "trace-123" {
		t.Fatalf("trace id = %q", header.Get("X-Trace-Id"))
	}

	badTrace := http.Header{"X-Trace-Id": []string{"bad\r\ninjection"}}
	header = capture(base, badTrace)
	if header.Get("X-Trace-Id") == "bad\r\ninjection" || header.Get("X-Trace-Id") == "" {
		t.Fatalf("invalid trace forwarded: %q", header.Get("X-Trace-Id"))
	}
}

func TestCommonHeaderLocales(t *testing.T) {
	for _, tc := range []struct {
		realm Realm
		lang  string
		ua    string
	}{
		{RealmCN, "zh-CN", cnUserAgent},
		{RealmGlobal, "en-US", codebuddyUserAgent},
		{RealmWorkBuddyGlobal, "en-US", workbuddyUserAgent},
	} {
		var captured http.Header
		client := testClient(roundTripFunc(func(req *http.Request) (*http.Response, error) {
			captured = req.Header.Clone()
			return fakeJSONResponse(200, `{"code":0,"msg":"ok","data":{"state":"S","authUrl":"https://www.codebuddy.ai/x"}}`), nil
		}))
		if _, err := client.StartLogin(context.Background(), tc.realm); err != nil {
			t.Fatalf("%s: %v", tc.realm, err)
		}
		if captured.Get("Accept-Language") != tc.lang || captured.Get("User-Agent") != tc.ua {
			t.Fatalf("%s: lang=%q ua=%q", tc.realm, captured.Get("Accept-Language"), captured.Get("User-Agent"))
		}
		if captured.Get("X-CodeBuddy-Request") != "1" || captured.Get("X-Requested-With") != "XMLHttpRequest" {
			t.Fatalf("%s: missing common headers", tc.realm)
		}
	}
}
