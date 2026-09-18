package auth

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	ex "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

type retryPolicyContextKey struct{}
type retryPolicyState struct {
	mu         sync.Mutex
	started    time.Time
	policy     *config.RetryPolicy
	authID     string
	proxyIndex map[string]int
}

func withRetryPolicyState(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, retryPolicyContextKey{}, &retryPolicyState{started: time.Now(), proxyIndex: make(map[string]int)})
}

func (m *Manager) retryPolicyFor(auth *Auth, model string) *config.RetryPolicy {
	// Home owns credential retry budgets and proxy selection remotely.
	if m.HomeEnabled() || auth == nil {
		return nil
	}
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil {
		return nil
	}
	matches := func(values []string, value string) bool {
		if len(values) == 0 {
			return true
		}
		for _, v := range values {
			if strings.EqualFold(v, value) {
				return true
			}
		}
		return false
	}
	for i := range cfg.RetryPolicies {
		p := &cfg.RetryPolicies[i]
		if matches(p.Providers, executorKeyFromAuth(auth)) && matches(p.Models, canonicalModelKey(model)) && (p.BaseURL == "" || strings.TrimRight(p.BaseURL, "/") == strings.TrimRight(auth.Attributes["base_url"], "/")) {
			return p
		}
	}
	return nil
}

func (m *Manager) retryPolicyAuth(ctx context.Context, auth *Auth, model string) *Auth {
	state, _ := ctx.Value(retryPolicyContextKey{}).(*retryPolicyState)
	if state == nil {
		return auth
	}
	p := m.retryPolicyFor(auth, model)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.policy, state.authID = p, auth.ID
	if p == nil || len(p.ProxyURLs) == 0 {
		return auth
	}
	clone := auth.Clone()
	clone.ProxyURL = p.ProxyURLs[state.proxyIndex[auth.ID]%len(p.ProxyURLs)]
	return clone
}

// Only pre-response connection failures are safe to replay as proxy errors.
func retryableProxyError(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && (op.Op == "proxyconnect" || op.Op == "dial") {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "proxyconnect tcp:") || strings.Contains(message, "dial tcp")
}

func retryPolicyMatches(p *config.RetryPolicy, err error) bool {
	if p == nil || err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errorRetryScope(err) == ex.RetryScopeSelectedExecution {
		return false
	}
	if p.RetryProxyErrors && retryableProxyError(err) {
		return true
	}
	for _, rule := range p.Errors {
		if statusCodeFromError(err) == rule.Status && (rule.Contains == "" || strings.Contains(strings.ToLower(err.Error()), strings.ToLower(rule.Contains))) {
			return true
		}
	}
	return false
}

func (m *Manager) policyRetryDecision(ctx context.Context, err error, attempt int, maxWait time.Duration) (time.Duration, bool, bool) {
	state, _ := ctx.Value(retryPolicyContextKey{}).(*retryPolicyState)
	if state == nil {
		return 0, false, false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	p := state.policy
	if p == nil {
		return 0, false, false
	}
	if ctx.Err() != nil || len(p.DelaysSeconds) == 0 || !retryPolicyMatches(p, err) || attempt >= p.MaxRetries {
		return 0, false, true
	}
	delay := time.Duration(p.DelaysSeconds[min(attempt, len(p.DelaysSeconds)-1)] * float64(time.Second))
	if after := retryAfterFromError(err); after != nil && *after > delay {
		delay = *after
	}
	if delay > 0 && (maxWait <= 0 || delay > maxWait) {
		return 0, false, true
	}
	delay = jitteredCooldownWait(delay, maxWait)
	if p.WindowSeconds > 0 && time.Since(state.started)+delay >= time.Duration(p.WindowSeconds*float64(time.Second)) {
		return 0, false, true
	}
	if p.RetryProxyErrors && retryableProxyError(err) {
		state.proxyIndex[state.authID]++
	}
	logEntryWithRequestID(ctx).WithFields(log.Fields{"attempt": attempt + 1, "delay": delay, "status": statusCodeFromError(err)}).Info("retrying configured upstream route")
	return delay, true, true
}

func waitForPolicyRetry(ctx context.Context, delay, maxWait time.Duration) error {
	state, _ := ctx.Value(retryPolicyContextKey{}).(*retryPolicyState)
	if state != nil {
		state.mu.Lock()
		configured := state.policy != nil
		state.mu.Unlock()
		if configured {
			maxWait = delay
		} // Policy delay already includes jitter.
	}
	return waitForCooldown(ctx, delay, maxWait)
}

func (m *Manager) policyRoundExclusions(ctx context.Context, round, defaultRetry int, model string) map[string]struct{} {
	excluded := m.requestRetryRoundExclusions(round, defaultRetry)
	if m.HomeEnabled() {
		return excluded
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for id, auth := range m.auths {
		if p := m.retryPolicyFor(auth, model); p != nil {
			expired := false
			if state, ok := ctx.Value(retryPolicyContextKey{}).(*retryPolicyState); ok && round > 0 && p.WindowSeconds > 0 {
				expired = time.Since(state.started) >= time.Duration(p.WindowSeconds*float64(time.Second))
			}
			if round > p.MaxRetries || expired {
				excluded[id] = struct{}{}
			} else {
				delete(excluded, id)
			}
		}
	}
	return excluded
}

func (m *Manager) policyNeutralResult(result Result) bool {
	cfg := m.runtimeConfigSnapshot()
	if cfg == nil || len(cfg.RetryPolicies) == 0 || result.Success || result.Error == nil || result.Error.Code == ErrorCodeForceCooldown {
		return false
	}
	auth, ok := m.GetByID(result.AuthID)
	return ok && retryPolicyMatches(m.retryPolicyFor(auth, result.RouteModel), result.Error)
}
