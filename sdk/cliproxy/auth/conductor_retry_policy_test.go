package auth

import (
	"context"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	ex "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type policyExecutor struct {
	retryRoundCallExecutor
	calls     []string
	failures  int
	err       error
	cancel    context.CancelFunc
	partial   bool
	bootstrap bool
}

func (e *policyExecutor) call(a *Auth) error {
	e.calls = append(e.calls, a.ProxyURL)
	if e.cancel != nil {
		e.cancel()
	}
	if len(e.calls) <= e.failures {
		return e.err
	}
	return nil
}
func (e *policyExecutor) Execute(_ context.Context, a *Auth, _ ex.Request, _ ex.Options) (ex.Response, error) {
	return ex.Response{Payload: []byte("OK")}, e.call(a)
}
func (e *policyExecutor) CountTokens(ctx context.Context, a *Auth, r ex.Request, o ex.Options) (ex.Response, error) {
	return e.Execute(ctx, a, r, o)
}
func (e *policyExecutor) ExecuteStream(_ context.Context, a *Auth, _ ex.Request, _ ex.Options) (*ex.StreamResult, error) {
	err := e.call(a)
	if err != nil && !e.bootstrap {
		return nil, err
	}
	ch := make(chan ex.StreamChunk, 2)
	if err != nil {
		ch <- ex.StreamChunk{Err: err}
	} else {
		ch <- ex.StreamChunk{Payload: []byte("data: OK\n\n")}
		if e.partial {
			ch <- ex.StreamChunk{Err: e.err}
		}
	}
	close(ch)
	return &ex.StreamResult{Chunks: ch}, nil
}
func newPolicyManager(t *testing.T, p config.RetryPolicy, e *policyExecutor) *Manager {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.SetConfig(&config.Config{RetryPolicies: []config.RetryPolicy{p}})
	m.SetRetryConfig(0, 0, 30)
	e.identifier = "policy-provider"
	m.RegisterExecutor(e)
	registerRetryRoundLocalAuths(t, m, "policy-provider", "policy-model", map[string]int{"policy-auth": 0})
	return m
}
func testPolicy() config.RetryPolicy {
	return config.RetryPolicy{Providers: []string{"policy-provider"}, Models: []string{"policy-model"}, MaxRetries: 2, DelaysSeconds: []float64{0}, Errors: []config.RetryPolicyError{{Status: 404, Contains: "Model not found"}}, RetryProxyErrors: true}
}

func TestRetryPolicyRealManagerPaths(t *testing.T) {
	for _, kind := range []string{"execute", "count", "stream", "bootstrap"} {
		t.Run(kind, func(t *testing.T) {
			e := &policyExecutor{failures: 2, err: &Error{HTTPStatus: 404, Message: "Model not found or access denied"}, bootstrap: kind == "bootstrap"}
			m := newPolicyManager(t, testPolicy(), e)
			req := ex.Request{Model: "policy-model"}
			var err error
			switch kind {
			case "execute":
				_, err = m.Execute(t.Context(), []string{"policy-provider"}, req, ex.Options{})
			case "count":
				_, err = m.ExecuteCount(t.Context(), []string{"policy-provider"}, req, ex.Options{})
			default:
				var r *ex.StreamResult
				r, err = m.ExecuteStream(t.Context(), []string{"policy-provider"}, req, ex.Options{})
				if r != nil {
					for range r.Chunks {
					}
				}
			}
			if err != nil || len(e.calls) != 3 {
				t.Fatalf("calls=%d err=%v", len(e.calls), err)
			}
			a, _ := m.GetByID("policy-auth")
			if a.Unavailable {
				t.Fatal("transient failure suspended credential")
			}
		})
	}
}
func TestRetryPolicyExhaustionAndProxyRotation(t *testing.T) {
	p := testPolicy()
	p.ProxyURLs = []string{"http://first:12323", "http://second:12323"}
	e := &policyExecutor{failures: 5, err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("Forbidden")}}
	m := newPolicyManager(t, p, e)
	_, err := m.Execute(t.Context(), []string{"policy-provider"}, ex.Request{Model: "policy-model"}, ex.Options{})
	if err == nil || !reflect.DeepEqual(e.calls, []string{p.ProxyURLs[0], p.ProxyURLs[1], p.ProxyURLs[0]}) {
		t.Fatalf("calls=%v err=%v", e.calls, err)
	}
	a, _ := m.GetByID("policy-auth")
	if a.ProxyURL != "" || a.Unavailable {
		t.Fatalf("shared credential changed: proxy=%q unavailable=%v", a.ProxyURL, a.Unavailable)
	}
}
func TestRetryPolicyDoesNotReplayPartialStream(t *testing.T) {
	e := &policyExecutor{partial: true, err: &Error{HTTPStatus: 404, Message: "Model not found"}}
	m := newPolicyManager(t, testPolicy(), e)
	r, err := m.ExecuteStream(t.Context(), []string{"policy-provider"}, ex.Request{Model: "policy-model"}, ex.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var data, failed bool
	for c := range r.Chunks {
		data = data || len(c.Payload) > 0
		failed = failed || c.Err != nil
	}
	if !data || !failed || len(e.calls) != 1 {
		t.Fatalf("data=%v failed=%v calls=%d", data, failed, len(e.calls))
	}
}
func TestRetryPolicyCancellation(t *testing.T) {
	p := testPolicy()
	p.DelaysSeconds = []float64{15}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := &policyExecutor{failures: 5, err: &Error{HTTPStatus: 404, Message: "Model not found"}, cancel: cancel}
	m := newPolicyManager(t, p, e)
	_, err := m.Execute(ctx, []string{"policy-provider"}, ex.Request{Model: "policy-model"}, ex.Options{})
	if !errors.Is(err, context.Canceled) || len(e.calls) != 1 {
		t.Fatalf("calls=%d err=%v", len(e.calls), err)
	}
	ctx = withRetryPolicyState(ctx)
	state := ctx.Value(retryPolicyContextKey{}).(*retryPolicyState)
	state.policy = &p
	if err := waitForPolicyRetry(ctx, time.Hour, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait: %v", err)
	}
}
func TestRetryPolicyDelayWindowAndMatching(t *testing.T) {
	p := testPolicy()
	p.DelaysSeconds = []float64{1, 2, 4, 8, 15}
	p.MaxRetries = 20
	p.WindowSeconds = 120
	m := newPolicyManager(t, p, &policyExecutor{})
	ctx := withRetryPolicyState(t.Context())
	a, _ := m.GetByID("policy-auth")
	m.retryPolicyAuth(ctx, a, "policy-model")
	for attempt, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 15 * time.Second, 15 * time.Second} {
		wait, yes, matched := m.policyRetryDecision(ctx, &Error{HTTPStatus: 404, Message: "Model not found"}, attempt, 30*time.Second)
		if !yes || !matched || wait < want || wait > want+cooldownWaitJitterCap {
			t.Fatalf("attempt=%d wait=%v yes=%v matched=%v", attempt, wait, yes, matched)
		}
	}
	state := ctx.Value(retryPolicyContextKey{}).(*retryPolicyState)
	state.started = time.Now().Add(-121 * time.Second)
	if _, excluded := m.policyRoundExclusions(ctx, 1, 0, "policy-model")["policy-auth"]; !excluded {
		t.Fatal("expired window permits another dispatch")
	}
	if _, yes, _ := m.policyRetryDecision(ctx, &Error{HTTPStatus: 404, Message: "Model not found"}, 0, 30*time.Second); yes {
		t.Fatal("retried expired window")
	}
	if m.retryPolicyFor(a, "other") != nil {
		t.Fatal("matched wrong model")
	}
	a.Provider = "other"
	if m.retryPolicyFor(a, "policy-model") != nil {
		t.Fatal("matched wrong provider")
	}
	a.Provider = "policy-provider"
	p.BaseURL = "https://api.example"
	m.SetConfig(&config.Config{RetryPolicies: []config.RetryPolicy{p}})
	if m.retryPolicyFor(a, "policy-model") != nil {
		t.Fatal("matched wrong route")
	}
	a.Attributes = map[string]string{"base_url": "https://api.example/"}
	if m.retryPolicyFor(a, "policy-model") == nil {
		t.Fatal("route did not match")
	}
	for _, err := range []error{&Error{HTTPStatus: 404, Message: "different error"}, &Error{HTTPStatus: 401, Message: "invalid key"}, &net.OpError{Op: "read", Err: errors.New("timeout")}} {
		if retryPolicyMatches(&p, err) {
			t.Fatalf("unsafe retry: %v", err)
		}
	}
}
