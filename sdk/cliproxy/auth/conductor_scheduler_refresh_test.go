package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type schedulerProviderTestExecutor struct {
	provider string
}

func (e schedulerProviderTestExecutor) Identifier() string { return e.provider }

func (e schedulerProviderTestExecutor) Execute(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerProviderTestExecutor) ExecuteStream(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (e schedulerProviderTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (e schedulerProviderTestExecutor) CountTokens(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e schedulerProviderTestExecutor) HttpRequest(ctx context.Context, auth *Auth, req *http.Request) (*http.Response, error) {
	return nil, nil
}

type unauthorizedRefreshTestExecutor struct {
	schedulerProviderTestExecutor
}

func (e unauthorizedRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return nil, errors.New("token refresh failed with status 401: invalid_grant")
}

type transientRefreshTestExecutor struct {
	schedulerProviderTestExecutor
}

func (e transientRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return nil, errors.New("token refresh failed with status 503: upstream unavailable")
}

type classifiedRefreshTestError struct {
	status    int
	retryable bool
}

func (e *classifiedRefreshTestError) Error() string   { return "refresh temporarily rate limited" }
func (e *classifiedRefreshTestError) StatusCode() int { return e.status }
func (e *classifiedRefreshTestError) Retryable() bool { return e.retryable }

type rateLimitedRefreshTestExecutor struct {
	schedulerProviderTestExecutor
}

func (e rateLimitedRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	return nil, &classifiedRefreshTestError{status: http.StatusTooManyRequests, retryable: false}
}

type recoveringRefreshTestExecutor struct {
	schedulerProviderTestExecutor
	calls int
}

func (e *recoveringRefreshTestExecutor) Refresh(ctx context.Context, auth *Auth) (*Auth, error) {
	e.calls++
	if e.calls == 1 {
		return nil, errors.New("token refresh failed with status 401: invalid_grant")
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = "refreshed-token"
	return auth, nil
}

func TestManager_RefreshAuthPermanentCredentialFailureUnschedules(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(unauthorizedRefreshTestExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "codex"},
	})

	auth := &Auth{
		ID:       "unauthorized-refresh",
		Provider: "codex",
		Metadata: map[string]any{
			"email": "x@example.com",
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.refreshAuth(ctx, auth.ID)

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after refresh", auth.ID)
	}
	if updated.LastError == nil {
		t.Fatal("expected permanent refresh failure to be recorded")
	}
	if updated.LastError.Retryable {
		t.Fatal("expected permanent refresh failure to be non-retryable")
	}
	if updated.LastError.Code != "refresh_rejected" {
		t.Fatalf("LastError.Code = %q, want refresh_rejected", updated.LastError.Code)
	}
	if !updated.NextRefreshAfter.IsZero() {
		t.Fatalf("NextRefreshAfter = %v, want terminal refresh failure unscheduled", updated.NextRefreshAfter)
	}
	if !updated.Unavailable || updated.Status != StatusError {
		t.Fatalf("refresh rejection state = unavailable:%v status:%v, want unavailable error", updated.Unavailable, updated.Status)
	}
	now := time.Now()
	if manager.shouldRefresh(updated, now) {
		t.Fatal("expected permanent refresh failure not to refresh automatically")
	}
	if _, shouldSchedule := nextRefreshCheckAt(now, updated, time.Second); shouldSchedule {
		t.Fatal("expected permanent refresh failure to be removed from the auto-refresh schedule")
	}
}

func TestManager_RefreshAuthTransientFailureBacksOffAndRetries(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(transientRefreshTestExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "test-provider"},
	})
	auth := &Auth{ID: "transient-refresh", Provider: "test-provider"}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.refreshAuth(ctx, auth.ID)
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated.LastError == nil {
		t.Fatal("expected transient refresh failure to be recorded")
	}
	if !updated.LastError.Retryable || updated.LastError.Code != "refresh_failed" {
		t.Fatalf("LastError = %#v, want retryable refresh_failed", updated.LastError)
	}
	if updated.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter is zero, want transient failure backoff")
	}
	if _, shouldSchedule := nextRefreshCheckAt(time.Now(), updated, time.Second); !shouldSchedule {
		t.Fatal("expected transient refresh failure to remain scheduled")
	}
}

func TestManager_RefreshAuthRateLimitRemainsScheduledEvenWhenProviderMarksItNonRetryable(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(rateLimitedRefreshTestExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "rate-limited-provider"},
	})
	auth := &Auth{ID: "rate-limited-refresh", Provider: "rate-limited-provider"}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.refreshAuth(ctx, auth.ID)
	updated, ok := manager.GetByID(auth.ID)
	if !ok || updated.LastError == nil {
		t.Fatal("expected rate-limited refresh failure to be recorded")
	}
	if !updated.LastError.Retryable || updated.LastError.Code != "refresh_failed" {
		t.Fatalf("LastError = %#v, want retryable refresh_failed", updated.LastError)
	}
	if updated.LastError.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("LastError.HTTPStatus = %d, want %d", updated.LastError.HTTPStatus, http.StatusTooManyRequests)
	}
	if updated.NextRefreshAfter.IsZero() {
		t.Fatal("NextRefreshAfter is zero, want rate-limit backoff")
	}
	if _, shouldSchedule := nextRefreshCheckAt(time.Now(), updated, time.Second); !shouldSchedule {
		t.Fatal("expected rate-limited credential to remain scheduled")
	}
}

func TestManager_SuccessfulRefreshClearsOnlyRefreshFailureState(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	exec := &recoveringRefreshTestExecutor{
		schedulerProviderTestExecutor: schedulerProviderTestExecutor{provider: "test-provider"},
	}
	manager.RegisterExecutor(exec)
	auth := &Auth{ID: "recovering-refresh", Provider: "test-provider"}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	manager.refreshAuth(ctx, auth.ID)
	failed, _ := manager.GetByID(auth.ID)
	if failed == nil || failed.LastError == nil || !failed.Unavailable {
		t.Fatalf("first refresh state = %#v, want quarantined credential", failed)
	}

	manager.refreshAuth(ctx, auth.ID)
	recovered, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after successful refresh", auth.ID)
	}
	if recovered.LastError != nil || recovered.Unavailable || recovered.Status != StatusActive || recovered.StatusMessage != "" {
		t.Fatalf("recovered state = %#v, want active credential with cleared refresh failure", recovered)
	}
	if got := recovered.Metadata["access_token"]; got != "refreshed-token" {
		t.Fatalf("access_token = %v, want refreshed-token", got)
	}
}

func TestManager_RefreshUpdatePreservesLiveModelCooldown(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	nextRetry := time.Now().Add(30 * time.Minute).Truncate(time.Second)

	auth := &Auth{
		ID:       "refresh-merge",
		Provider: "codex",
		Status:   StatusError,
		Metadata: map[string]any{
			"access_token": "old-token",
		},
		ModelStates: map[string]*ModelState{
			"model-1": {
				Status:         StatusError,
				StatusMessage:  "quota",
				NextRetryAfter: nextRetry,
				Quota: QuotaState{
					Exceeded:      true,
					Reason:        "quota",
					NextRecoverAt: nextRetry,
					BackoffLevel:  2,
				},
			},
		},
	}
	if _, errRegister := manager.Register(ctx, auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	refreshed := auth.Clone()
	refreshed.Metadata = map[string]any{"access_token": "new-token"}
	refreshed.ModelStates = nil
	refreshed.Status = StatusActive
	refreshed.StatusMessage = ""
	refreshed.LastRefreshedAt = time.Now()
	refreshed.UpdatedAt = refreshed.LastRefreshedAt
	refreshed.NextRefreshAfter = refreshed.LastRefreshedAt.Add(time.Hour)

	manager.applyRefreshUpdate(ctx, auth.ID, refreshed)

	updated, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatalf("expected auth %q after refresh update", auth.ID)
	}
	if got := updated.Metadata["access_token"]; got != "new-token" {
		t.Fatalf("access_token = %v, want new-token", got)
	}
	state := updated.ModelStates["model-1"]
	if state == nil {
		t.Fatal("model cooldown state was removed")
	}
	if !state.NextRetryAfter.Equal(nextRetry) {
		t.Fatalf("NextRetryAfter = %s, want %s", state.NextRetryAfter, nextRetry)
	}
	if !state.Quota.Exceeded || state.Quota.BackoffLevel != 2 {
		t.Fatalf("quota state = %#v, want exceeded backoff level 2", state.Quota)
	}
}

func TestManager_RefreshSchedulerEntry_RebuildsSupportedModelSetAfterModelRegistration(t *testing.T) {
	ctx := context.Background()

	testCases := []struct {
		name  string
		prime func(*Manager, *Auth) error
	}{
		{
			name: "register",
			prime: func(manager *Manager, auth *Auth) error {
				_, errRegister := manager.Register(ctx, auth)
				return errRegister
			},
		},
		{
			name: "update",
			prime: func(manager *Manager, auth *Auth) error {
				_, errRegister := manager.Register(ctx, auth)
				if errRegister != nil {
					return errRegister
				}
				updated := auth.Clone()
				updated.Metadata = map[string]any{"updated": true}
				_, errUpdate := manager.Update(ctx, updated)
				return errUpdate
			},
		},
	}

	for _, testCase := range testCases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			manager := NewManager(nil, &RoundRobinSelector{}, nil)
			auth := &Auth{
				ID:       "refresh-entry-" + testCase.name,
				Provider: "gemini",
			}
			if errPrime := testCase.prime(manager, auth); errPrime != nil {
				t.Fatalf("prime auth %s: %v", testCase.name, errPrime)
			}

			registerSchedulerModels(t, "gemini", "scheduler-refresh-model", auth.ID)

			got, errPick := manager.scheduler.pickSingle(ctx, "gemini", "scheduler-refresh-model", cliproxyexecutor.Options{}, nil)
			var authErr *Error
			if !errors.As(errPick, &authErr) || authErr == nil {
				t.Fatalf("pickSingle() before refresh error = %v, want auth_not_found", errPick)
			}
			if authErr.Code != "auth_not_found" {
				t.Fatalf("pickSingle() before refresh code = %q, want %q", authErr.Code, "auth_not_found")
			}
			if got != nil {
				t.Fatalf("pickSingle() before refresh auth = %v, want nil", got)
			}

			manager.RefreshSchedulerEntry(auth.ID)

			got, errPick = manager.scheduler.pickSingle(ctx, "gemini", "scheduler-refresh-model", cliproxyexecutor.Options{}, nil)
			if errPick != nil {
				t.Fatalf("pickSingle() after refresh error = %v", errPick)
			}
			if got == nil || got.ID != auth.ID {
				t.Fatalf("pickSingle() after refresh auth = %v, want %q", got, auth.ID)
			}
		})
	}
}

func TestManager_PickNext_RebuildsSchedulerAfterModelCooldownError(t *testing.T) {
	ctx := context.Background()
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "gemini"})

	registerSchedulerModels(t, "gemini", "scheduler-cooldown-rebuild-model", "cooldown-stale-old")

	oldAuth := &Auth{
		ID:       "cooldown-stale-old",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, oldAuth); errRegister != nil {
		t.Fatalf("register old auth: %v", errRegister)
	}

	manager.MarkResult(ctx, Result{
		AuthID:   oldAuth.ID,
		Provider: "gemini",
		Model:    "scheduler-cooldown-rebuild-model",
		Success:  false,
		Error:    &Error{HTTPStatus: http.StatusTooManyRequests, Message: "quota"},
	})

	newAuth := &Auth{
		ID:       "cooldown-stale-new",
		Provider: "gemini",
	}
	if _, errRegister := manager.Register(ctx, newAuth); errRegister != nil {
		t.Fatalf("register new auth: %v", errRegister)
	}

	reg := registry.GetGlobalRegistry()
	reg.RegisterClient(newAuth.ID, "gemini", []*registry.ModelInfo{{ID: "scheduler-cooldown-rebuild-model"}})
	t.Cleanup(func() {
		reg.UnregisterClient(newAuth.ID)
	})

	got, errPick := manager.scheduler.pickSingle(ctx, "gemini", "scheduler-cooldown-rebuild-model", cliproxyexecutor.Options{}, nil)
	var cooldownErr *modelCooldownError
	if !errors.As(errPick, &cooldownErr) {
		t.Fatalf("pickSingle() before sync error = %v, want modelCooldownError", errPick)
	}
	if got != nil {
		t.Fatalf("pickSingle() before sync auth = %v, want nil", got)
	}

	got, executor, errPick := manager.pickNext(ctx, "gemini", "scheduler-cooldown-rebuild-model", cliproxyexecutor.Options{}, nil)
	if errPick != nil {
		t.Fatalf("pickNext() error = %v", errPick)
	}
	if executor == nil {
		t.Fatal("pickNext() executor = nil")
	}
	if got == nil || got.ID != newAuth.ID {
		t.Fatalf("pickNext() auth = %v, want %q", got, newAuth.ID)
	}
}
