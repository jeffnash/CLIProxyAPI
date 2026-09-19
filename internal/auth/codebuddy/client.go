package codebuddy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Memory-safety ceilings for provider responses. These bound retained bytes,
// not timeouts or context windows. Streaming paths never accumulate the whole
// response; the aggregate ceiling applies only to non-stream assembly.
const (
	// AuthEnvelopeLimit bounds auth/error JSON envelopes.
	AuthEnvelopeLimit int64 = 1 << 20
	// CatalogLimit bounds model catalog payloads.
	CatalogLimit int64 = 4 << 20
	// SSEEventLimit bounds a single SSE event across all its lines.
	SSEEventLimit int64 = 8 << 20
	// AggregateLimit bounds retained content while aggregating a stream.
	AggregateLimit int64 = 64 << 20
)

// AuthTimeout is the per-request deadline for credential acquisition and
// refresh. Catalog and chat requests use the caller context with a
// zero-timeout client instead.
const AuthTimeout = 30 * time.Second

// LoginLifetime bounds an interactive browser authorization session.
const LoginLifetime = 15 * time.Minute

// Observed client version constants. These are compatibility values observed
// in official clients, not assertions that CLIProxyAPI is the official app.
const (
	cnUserAgent        = "WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1"
	workbuddyUserAgent = "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1"
	codebuddyUserAgent = "CodeBuddy/2.143.0 CLI/2.143.0"
)

// Client performs CodeBuddy account HTTP operations against realm endpoints.
type Client struct {
	httpClient *http.Client
	now        func() time.Time
}

// NewClient builds a provider client over the supplied HTTP client. The client
// value is cloned so the no-redirect policy never mutates the caller's client;
// the transport pointer is kept for connection reuse. Production callers pass
// a proxy-aware zero-timeout client.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		panic("codebuddy: NewClient requires a non-nil HTTP client")
	}
	clone := *httpClient
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{httpClient: &clone, now: time.Now}
}

func (c *Client) currentTime() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// baseURL returns the API base for a realm.
func baseURL(realm Realm) string {
	switch realm {
	case RealmGlobal:
		return "https://www.codebuddy.ai"
	case RealmWorkBuddyGlobal:
		return "https://www.workbuddy.ai"
	default:
		return "https://copilot.tencent.com"
	}
}

// originFor returns the Origin/Referer base for a realm.
func originFor(realm Realm) string {
	switch realm {
	case RealmGlobal:
		return "https://www.codebuddy.ai"
	case RealmWorkBuddyGlobal:
		return "https://www.workbuddy.ai"
	default:
		return "https://www.codebuddy.cn"
	}
}

// ChatURL returns the inference endpoint for a realm.
func ChatURL(realm Realm) string {
	switch realm {
	case RealmCN, RealmGlobal, RealmWorkBuddyGlobal:
		return baseURL(realm) + "/v2/chat/completions"
	default:
		return ""
	}
}

func userAgentFor(realm Realm) string {
	switch realm {
	case RealmGlobal:
		return codebuddyUserAgent
	case RealmWorkBuddyGlobal:
		return workbuddyUserAgent
	default:
		return cnUserAgent
	}
}

func acceptLanguageFor(realm Realm) string {
	if realm == RealmCN {
		return "zh-CN"
	}
	return "en-US"
}

// validRealm reports whether the realm is a known profile.
func validRealm(realm Realm) bool {
	switch realm {
	case RealmCN, RealmGlobal, RealmWorkBuddyGlobal:
		return true
	default:
		return false
	}
}

// commonHeaders sets the headers shared by login, account, catalog, refresh
// and chat requests.
func commonHeaders(req *http.Request, realm Realm) {
	header := req.Header
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json")
	header.Set("X-Requested-With", "XMLHttpRequest")
	header.Set("X-CodeBuddy-Request", "1")
	origin := originFor(realm)
	header.Set("Origin", origin)
	header.Set("Referer", origin+"/")
	header.Set("Accept-Language", acceptLanguageFor(realm))
	header.Set("User-Agent", userAgentFor(realm))
}

// envelopeResult is a decoded Tencent {code,msg,data} business envelope.
type envelopeResult struct {
	code int
	msg  string
	data json.RawMessage
}

// authError is a terminal credential-layer failure. It never carries raw
// upstream bodies or secret values.
type authError struct {
	op     string
	status int
	code   int
	msg    string
}

func (e *authError) Error() string {
	if e == nil {
		return "codebuddy: unknown auth error"
	}
	if e.code != 0 {
		return fmt.Sprintf("codebuddy: %s failed (http %d, code %d): %s", e.op, e.status, e.code, e.msg)
	}
	if e.status != 0 {
		return fmt.Sprintf("codebuddy: %s failed (http %d): %s", e.op, e.status, e.msg)
	}
	return fmt.Sprintf("codebuddy: %s failed: %s", e.op, e.msg)
}

// redactSecrets replaces known secret values in a message so upstream echoes
// of credentials cannot leak into errors or logs.
func redactSecrets(msg string, secrets ...string) string {
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, secret, "[REDACTED]")
	}
	return msg
}

// codeBuddyLoggedURL matches URLs embedded in transport errors so their
// query strings can be stripped before the error reaches logs or API errors.
var codeBuddyLoggedURL = regexp.MustCompile(`https?://[^\s"']+`)

// sanitizeRequestErrorURL strips query strings from URLs embedded in an
// error message. net/http wraps transport failures with the complete request
// URL, and credential-bearing CodeBuddy requests carry the login state in
// the query; the host and path stay for debuggability.
func sanitizeRequestErrorURL(msg string) string {
	return codeBuddyLoggedURL.ReplaceAllStringFunc(msg, func(raw string) string {
		if index := strings.IndexByte(raw, '?'); index >= 0 {
			return raw[:index] + "?[REDACTED]"
		}
		return raw
	})
}

// readLimitedBody reads resp.Body with an overflow check. It rejects bodies
// larger than limit instead of parsing a truncated prefix.
func readLimitedBody(resp *http.Response, limit int64) ([]byte, error) {
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("codebuddy: missing response body")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("codebuddy: response body exceeds %d bytes", limit)
	}
	return raw, nil
}

// decodeEnvelope decodes a Tencent business envelope. A missing code is
// invalid, never implicit success.
func decodeEnvelope(raw []byte) (envelopeResult, error) {
	var parsed struct {
		Code *int            `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		return envelopeResult{}, fmt.Errorf("codebuddy: invalid response envelope: %w", err)
	}
	if parsed.Code == nil {
		return envelopeResult{}, fmt.Errorf("codebuddy: invalid response envelope: missing code")
	}
	return envelopeResult{code: *parsed.Code, msg: parsed.Msg, data: parsed.Data}, nil
}

// doAuthRequest executes one credential-bearing API call with a per-request
// auth deadline. Redirects are never followed: a 3xx response is a protocol
// failure, so a redirect to an attacker host performs zero attacker requests.
func (c *Client) doAuthRequest(ctx context.Context, op, method, fullURL string, realm Realm, extra func(*http.Request), body []byte, secrets ...string) (envelopeResult, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	reqCtx, cancel := context.WithTimeout(ctx, AuthTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, fullURL, reader)
	if err != nil {
		return envelopeResult{}, 0, &authError{op: op, msg: "build request: " + redactSecrets(sanitizeRequestErrorURL(err.Error()), secrets...)}
	}
	commonHeaders(req, realm)
	if extra != nil {
		extra(req)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if reqCtx.Err() != nil && ctx.Err() == nil {
			return envelopeResult{}, 0, &authError{op: op, msg: "request timed out acquiring credentials"}
		}
		return envelopeResult{}, 0, &authError{op: op, msg: "request failed: " + redactSecrets(sanitizeRequestErrorURL(err.Error()), secrets...)}
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	status := resp.StatusCode
	if status >= 300 && status < 400 {
		return envelopeResult{}, status, &authError{op: op, status: status, msg: "redirect rejected for credential-bearing request"}
	}
	raw, err := readLimitedBody(resp, AuthEnvelopeLimit)
	if err != nil {
		return envelopeResult{}, status, &authError{op: op, status: status, msg: err.Error()}
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return envelopeResult{}, status, &authError{op: op, status: status, msg: "unauthorized; re-login is required"}
	}
	if status < 200 || status >= 300 {
		return envelopeResult{}, status, &authError{op: op, status: status, msg: "unexpected status"}
	}
	result, err := decodeEnvelope(raw)
	if err != nil {
		return envelopeResult{}, status, &authError{op: op, status: status, msg: err.Error()}
	}
	return result, status, nil
}

// StartLogin opens a browser authorization session for the realm.
func (c *Client) StartLogin(ctx context.Context, realm Realm) (LoginSession, error) {
	if !validRealm(realm) {
		return LoginSession{}, &authError{op: "start login", msg: fmt.Sprintf("invalid realm %q", string(realm))}
	}
	endpoint, err := url.Parse(baseURL(realm) + "/v2/plugin/auth/state")
	if err != nil {
		return LoginSession{}, &authError{op: "start login", msg: "invalid endpoint"}
	}
	query := endpoint.Query()
	query.Set("platform", "CLI")
	endpoint.RawQuery = query.Encode()
	result, _, err := c.doAuthRequest(ctx, "start login", http.MethodPost, endpoint.String(), realm, nil, []byte("{}"))
	if err != nil {
		return LoginSession{}, err
	}
	if result.code != 0 {
		return LoginSession{}, &authError{op: "start login", code: result.code, msg: redactSecrets(result.msg)}
	}
	var payload struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(result.data, &payload); err != nil {
		return LoginSession{}, &authError{op: "start login", msg: "invalid authorization payload"}
	}
	if strings.TrimSpace(payload.State) == "" || strings.TrimSpace(payload.AuthURL) == "" {
		return LoginSession{}, &authError{op: "start login", msg: "authorization response missing state or URL"}
	}
	if err := validateAuthURL(payload.AuthURL); err != nil {
		return LoginSession{}, &authError{op: "start login", msg: err.Error()}
	}
	return LoginSession{
		State:     payload.State,
		AuthURL:   payload.AuthURL,
		Realm:     realm,
		ExpiresAt: c.currentTime().Add(LoginLifetime),
	}, nil
}

// validateAuthURL ensures an authorization link is an HTTPS URL without
// userinfo on a CodeBuddy/WorkBuddy host. Authorization links are browser
// destinations, never API destinations.
func validateAuthURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid authorization URL")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return fmt.Errorf("invalid authorization URL scheme")
	}
	if parsed.User != nil {
		return fmt.Errorf("invalid authorization URL userinfo")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return fmt.Errorf("invalid authorization URL host")
	}
	for _, domain := range []string{"codebuddy.cn", "codebuddy.ai", "workbuddy.ai"} {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return nil
		}
	}
	return fmt.Errorf("invalid authorization URL host")
}

// isPendingLoginMessage reports whether a nonzero business message is the
// recognized authorization-pending signal. Arbitrary nonzero codes are
// terminal errors, never pending.
func isPendingLoginMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "login ing")
}

// PollLogin checks a pending authorization session. It returns ready=false
// with nil error only for the recognized pending response. The stored session
// realm selects the endpoint; the token response domain cannot redirect calls.
func (c *Client) PollLogin(ctx context.Context, session LoginSession) (Credentials, bool, error) {
	if strings.TrimSpace(session.State) == "" {
		return Credentials{}, false, &authError{op: "poll login", msg: "missing login state"}
	}
	if !validRealm(session.Realm) {
		return Credentials{}, false, &authError{op: "poll login", msg: fmt.Sprintf("invalid realm %q", string(session.Realm))}
	}
	now := c.currentTime()
	if !session.ExpiresAt.IsZero() && !now.Before(session.ExpiresAt) {
		return Credentials{}, false, &authError{op: "poll login", msg: "login session expired"}
	}
	endpoint, err := url.Parse(baseURL(session.Realm) + "/v2/plugin/auth/token")
	if err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: "invalid endpoint"}
	}
	query := endpoint.Query()
	query.Set("state", session.State)
	endpoint.RawQuery = query.Encode()
	result, _, err := c.doAuthRequest(ctx, "poll login", http.MethodGet, endpoint.String(), session.Realm, nil, nil, session.State)
	if err != nil {
		return Credentials{}, false, err
	}
	if result.code != 0 {
		if isPendingLoginMessage(result.msg) {
			return Credentials{}, false, nil
		}
		return Credentials{}, false, &authError{op: "poll login", code: result.code, msg: redactSecrets(result.msg)}
	}
	var token struct {
		AccessToken  string      `json:"accessToken"`
		RefreshToken string      `json:"refreshToken"`
		ExpiresIn    json.Number `json:"expiresIn"`
		Domain       string      `json:"domain"`
	}
	decoder := json.NewDecoder(bytes.NewReader(result.data))
	decoder.UseNumber()
	if err := decoder.Decode(&token); err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: "invalid token payload"}
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return Credentials{}, false, &authError{op: "poll login", msg: "token response missing access token"}
	}
	realm, err := ParseRealm(string(session.Realm), token.Domain)
	if err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: err.Error()}
	}
	expired, err := expiresInToTime(token.ExpiresIn, token.RefreshToken, now)
	if err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: err.Error()}
	}
	accountEndpoint, err := url.Parse(baseURL(session.Realm) + "/v2/plugin/login/account")
	if err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: "invalid endpoint"}
	}
	accountQuery := accountEndpoint.Query()
	accountQuery.Set("state", session.State)
	accountEndpoint.RawQuery = accountQuery.Encode()
	accessToken := token.AccessToken
	account, _, err := c.doAuthRequest(ctx, "poll login", http.MethodGet, accountEndpoint.String(), session.Realm, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}, nil, token.AccessToken, token.RefreshToken, session.State)
	if err != nil {
		return Credentials{}, false, err
	}
	if account.code != 0 {
		return Credentials{}, false, &authError{op: "poll login", code: account.code, msg: redactSecrets(account.msg, token.AccessToken, token.RefreshToken)}
	}
	var identity struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if err := json.Unmarshal(account.data, &identity); err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: "invalid account payload"}
	}
	if strings.TrimSpace(identity.UID) == "" {
		return Credentials{}, false, &authError{op: "poll login", msg: "account response missing UID"}
	}
	machineID, err := randomHexID()
	if err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: "generate device identity: " + err.Error()}
	}
	sessionID, err := randomHexID()
	if err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: "generate device identity: " + err.Error()}
	}
	credentials := Credentials{
		Realm:        realm,
		AccessToken:  token.AccessToken,
		RefreshToken: token.RefreshToken,
		UID:          identity.UID,
		EnterpriseID: identity.EnterpriseID,
		Nickname:     identity.Nickname,
		Domain:       token.Domain,
		MachineID:    machineID,
		SessionID:    sessionID,
		Expired:      expired,
	}
	if err := credentials.Validate(); err != nil {
		return Credentials{}, false, &authError{op: "poll login", msg: err.Error()}
	}
	return credentials, true, nil
}

// expiresInToTime converts a fresh login/refresh expiresIn value (positive
// integer seconds) to an absolute time using the injected clock. A missing or
// zero value leaves expiry unknown, which requires a refresh token so the
// credential can be refreshed before use.
func expiresInToTime(value json.Number, refreshToken string, now time.Time) (time.Time, error) {
	trimmed := strings.TrimSpace(value.String())
	if trimmed == "" || trimmed == "0" {
		if strings.TrimSpace(refreshToken) == "" {
			return time.Time{}, fmt.Errorf("no usable expiration and no refresh token; re-login is required")
		}
		return time.Time{}, nil
	}
	seconds, err := parsePositiveInt64(trimmed)
	if err != nil || seconds <= 0 {
		return time.Time{}, fmt.Errorf("invalid expires_in value")
	}
	if seconds > int64(1<<62)/int64(time.Second) {
		return time.Time{}, fmt.Errorf("expires_in value overflows")
	}
	return now.Add(time.Duration(seconds) * time.Second), nil
}

// parsePositiveInt64 parses a JSON integer without accepting fractions,
// exponents or surrounding whitespace.
func parsePositiveInt64(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty integer")
	}
	var value int64
	for i := 0; i < len(raw); i++ {
		digit := raw[i]
		if digit == '+' && i == 0 && len(raw) > 1 {
			continue
		}
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("invalid integer %q", raw)
		}
		value = value*10 + int64(digit-'0')
		if value < 0 {
			return 0, fmt.Errorf("integer %q overflows", raw)
		}
	}
	return value, nil
}

// Refresh rotates tokens for stored credentials. Fields the refresh response
// omits keep their previous values; only a nonempty access token completes.
func (c *Client) Refresh(ctx context.Context, credentials Credentials) (Credentials, error) {
	if err := credentials.Validate(); err != nil {
		return Credentials{}, &authError{op: "refresh", msg: err.Error()}
	}
	if strings.TrimSpace(credentials.RefreshToken) == "" {
		return Credentials{}, &authError{op: "refresh", msg: "missing refresh token; re-login is required"}
	}
	if !validRealm(credentials.Realm) {
		return Credentials{}, &authError{op: "refresh", msg: fmt.Sprintf("invalid realm %q", string(credentials.Realm))}
	}
	refreshToken := credentials.RefreshToken
	enterpriseID := credentials.EnterpriseID
	result, _, err := c.doAuthRequest(ctx, "refresh", http.MethodPost, baseURL(credentials.Realm)+"/v2/plugin/auth/token/refresh", credentials.Realm, func(req *http.Request) {
		req.Header.Set("X-Refresh-Token", refreshToken)
		req.Header.Set("X-Auth-Refresh-Source", "plugin")
		if enterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", enterpriseID)
		}
	}, nil, credentials.AccessToken, credentials.RefreshToken)
	if err != nil {
		return Credentials{}, err
	}
	if result.code != 0 {
		return Credentials{}, &authError{op: "refresh", code: result.code, msg: redactSecrets(result.msg, credentials.AccessToken, credentials.RefreshToken)}
	}
	var token struct {
		AccessToken  string      `json:"accessToken"`
		RefreshToken string      `json:"refreshToken"`
		ExpiresIn    json.Number `json:"expiresIn"`
		Domain       string      `json:"domain"`
	}
	decoder := json.NewDecoder(bytes.NewReader(result.data))
	decoder.UseNumber()
	if err := decoder.Decode(&token); err != nil {
		return Credentials{}, &authError{op: "refresh", msg: "invalid refresh payload"}
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return Credentials{}, &authError{op: "refresh", msg: "refresh response missing access token"}
	}
	updated := credentials
	updated.AccessToken = token.AccessToken
	if strings.TrimSpace(token.RefreshToken) != "" {
		updated.RefreshToken = token.RefreshToken
	}
	if strings.TrimSpace(token.Domain) != "" {
		if _, err := ParseRealm(string(credentials.Realm), token.Domain); err != nil {
			return Credentials{}, &authError{op: "refresh", msg: err.Error()}
		}
		updated.Domain = token.Domain
	}
	if strings.TrimSpace(token.ExpiresIn.String()) != "" {
		expired, err := expiresInToTime(token.ExpiresIn, updated.RefreshToken, c.currentTime())
		if err != nil {
			return Credentials{}, &authError{op: "refresh", msg: err.Error()}
		}
		if !expired.IsZero() {
			updated.Expired = expired
		}
	}
	if err := updated.Validate(); err != nil {
		return Credentials{}, &authError{op: "refresh", msg: err.Error()}
	}
	return updated, nil
}

// OpenChat sends one chat completion request. It uses the caller context with
// the client's zero-timeout transport: established inference connections stay
// cancellation-driven without an inference timeout. Caller headers contribute
// only an optional User-Agent override and an optional validated trace ID;
// caller Authorization, cookies, forwarding and Tencent identity headers are
// never forwarded.
func (c *Client) OpenChat(ctx context.Context, credentials Credentials, body []byte, headers http.Header) (*http.Response, error) {
	if err := credentials.Validate(); err != nil {
		return nil, &authError{op: "chat", msg: err.Error()}
	}
	endpoint := ChatURL(credentials.Realm)
	if endpoint == "" {
		return nil, &authError{op: "chat", msg: fmt.Sprintf("invalid realm %q", string(credentials.Realm))}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestID, err := randomHexID()
	if err != nil {
		return nil, &authError{op: "chat", msg: "generate request identity: " + err.Error()}
	}
	traceID := validatedTraceID(headers.Get("X-Trace-ID"))
	if traceID == "" {
		traceID = requestID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, &authError{op: "chat", msg: "build request: " + err.Error()}
	}
	applyChatHeaders(req, credentials, requestID, traceID)
	if override := strings.TrimSpace(headers.Get("User-Agent")); override != "" && !strings.ContainsAny(override, "\r\n") {
		req.Header.Set("User-Agent", override)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &authError{op: "chat", msg: "request failed: " + redactSecrets(err.Error(), credentials.AccessToken, credentials.RefreshToken, credentials.DeviceToken)}
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		_ = resp.Body.Close()
		return nil, &authError{op: "chat", status: resp.StatusCode, msg: "redirect rejected for credential-bearing request"}
	}
	return resp, nil
}

// applyChatHeaders sets the full chat header set for validated credentials.
func applyChatHeaders(req *http.Request, credentials Credentials, requestID, traceID string) {
	commonHeaders(req, credentials.Realm)
	header := req.Header
	header.Set("Accept", "text/event-stream")
	header.Set("Authorization", "Bearer "+credentials.AccessToken)
	header.Set("X-User-Id", credentials.UID)
	switch credentials.Realm {
	case RealmCN:
		if credentials.EnterpriseID != "" {
			header.Set("X-Enterprise-Id", credentials.EnterpriseID)
		} else {
			header.Set("X-No-Enterprise-Id", "1")
		}
		if credentials.Domain != "" {
			header.Set("X-Domain", credentials.Domain)
		} else {
			header.Set("X-No-Department-Info", "1")
		}
	case RealmGlobal:
		header.Set("X-No-Enterprise-Id", "1")
		header.Set("X-Domain", "www.codebuddy.ai")
	case RealmWorkBuddyGlobal:
		header.Set("X-No-Enterprise-Id", "1")
		header.Set("X-Domain", "www.workbuddy.ai")
	}
	if credentials.MachineID != "" {
		header.Set("X-Machine-ID", credentials.MachineID)
	}
	if credentials.SessionID != "" {
		header.Set("X-Session-ID", credentials.SessionID)
	}
	if credentials.DeviceToken != "" {
		header.Set("X-Device-Token", credentials.DeviceToken)
	}
	header.Set("X-Request-ID", requestID)
	header.Set("X-Conversation-Message-ID", requestID)
	header.Set("X-Trace-ID", traceID)
}

// RealmHost returns the exact API hostname for a realm, or "" when unknown.
func RealmHost(realm Realm) string {
	switch realm {
	case RealmCN, RealmGlobal, RealmWorkBuddyGlobal:
		endpoint, err := url.Parse(baseURL(realm))
		if err != nil {
			return ""
		}
		return endpoint.Host
	default:
		return ""
	}
}

// InjectHeadersForRequest sets the correct headers on an already
// boundary-validated provider request: full chat headers for the chat path,
// common authenticated headers for catalog paths. Callers must have stripped
// untrusted caller headers first.
func InjectHeadersForRequest(req *http.Request, credentials Credentials) error {
	if req == nil {
		return fmt.Errorf("codebuddy: request is nil")
	}
	if err := credentials.Validate(); err != nil {
		return err
	}
	switch req.URL.Path {
	case "/v2/chat/completions":
		requestID, err := randomHexID()
		if err != nil {
			return fmt.Errorf("codebuddy: generate request identity: %w", err)
		}
		applyChatHeaders(req, credentials, requestID, requestID)
		return nil
	case "/v3/config", "/console/enterprises/personal/models", "/v2/enterprises/personal/models":
		commonHeaders(req, credentials.Realm)
		req.Header.Set("Authorization", "Bearer "+credentials.AccessToken)
		return nil
	default:
		return fmt.Errorf("codebuddy: path %q is not an approved provider path", req.URL.Path)
	}
}

// validatedTraceID accepts an existing CLIProxyAPI request trace ID for the
// upstream trace header. Untrusted conversation IDs are never forwarded.
func validatedTraceID(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" || len(trimmed) > 256 || strings.ContainsAny(trimmed, "\r\n") {
		return ""
	}
	return trimmed
}

// randomHexID generates a cryptographic 16-byte hex identifier.
func randomHexID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}
