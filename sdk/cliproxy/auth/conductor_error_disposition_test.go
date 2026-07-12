package auth

import (
	"context"
	"net/http"
	"sync"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type dispositionTestError struct {
	scope        cliproxyexecutor.RetryScope
	attributable bool
}

func (e *dispositionTestError) Error() string                           { return "classified execution failure" }
func (e *dispositionTestError) StatusCode() int                         { return http.StatusTooManyRequests }
func (e *dispositionTestError) RetryScope() cliproxyexecutor.RetryScope { return e.scope }
func (e *dispositionTestError) AuthAttributable() bool                  { return e.attributable }

type dispositionTestExecutor struct {
	mu       sync.Mutex
	calls    []string
	failures int
	err      error
}

func (e *dispositionTestExecutor) Identifier() string { return "disposition-test" }
func (e *dispositionTestExecutor) Execute(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, auth.ID)
	if len(e.calls) <= e.failures {
		return cliproxyexecutor.Response{}, e.err
	}
	return cliproxyexecutor.Response{Payload: []byte(`{"ok":true}`)}, nil
}

func (e *dispositionTestExecutor) ExecuteStream(_ context.Context, auth *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, auth.ID)
	return nil, e.err
}
func (e *dispositionTestExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}
func (e *dispositionTestExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *dispositionTestExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func registerDispositionTestAuths(t *testing.T, manager *Manager) {
	t.Helper()
	for _, id := range []string{"disposition-a", "disposition-b"} {
		if _, err := manager.Register(context.Background(), &Auth{ID: id, Provider: "disposition-test"}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
}

func TestManagerSelectedExecutionFailureDoesNotRotateOrPoisonCredentials(t *testing.T) {
	executor := &dispositionTestExecutor{
		failures: 2,
		err: &dispositionTestError{
			scope: cliproxyexecutor.RetryScopeSelectedExecution,
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	registerDispositionTestAuths(t, manager)

	_, err := manager.Execute(context.Background(), []string{"disposition-test"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("selected-execution failure must be returned")
	}
	if len(executor.calls) != 1 {
		t.Fatalf("executor calls = %v, want exactly one selected execution", executor.calls)
	}
	for _, id := range []string{"disposition-a", "disposition-b"} {
		auth, ok := manager.GetByID(id)
		if !ok || auth.Unavailable || auth.LastError != nil {
			t.Fatalf("credential %s was poisoned by a non-attributable execution error: %#v", id, auth)
		}
	}
}

func TestManagerSelectedExecutionStreamFailureDoesNotRotateOrPoisonCredentials(t *testing.T) {
	executor := &dispositionTestExecutor{
		failures: 2,
		err: &dispositionTestError{
			scope: cliproxyexecutor.RetryScopeSelectedExecution,
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	registerDispositionTestAuths(t, manager)

	_, err := manager.ExecuteStream(context.Background(), []string{"disposition-test"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("selected-execution stream failure must be returned")
	}
	if len(executor.calls) != 1 {
		t.Fatalf("stream executor calls = %v, want exactly one selected execution", executor.calls)
	}
	for _, id := range []string{"disposition-a", "disposition-b"} {
		auth, ok := manager.GetByID(id)
		if !ok || auth.Unavailable || auth.LastError != nil {
			t.Fatalf("credential %s was poisoned by a non-attributable stream error: %#v", id, auth)
		}
	}
}

func TestManagerAuthAttributableFailureMayRotateCredentials(t *testing.T) {
	executor := &dispositionTestExecutor{
		failures: 1,
		err: &dispositionTestError{
			scope:        cliproxyexecutor.RetryScopeDefault,
			attributable: true,
		},
	}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	registerDispositionTestAuths(t, manager)

	if _, err := manager.Execute(context.Background(), []string{"disposition-test"}, cliproxyexecutor.Request{}, cliproxyexecutor.Options{}); err != nil {
		t.Fatalf("credential-attributable failure should fall back to the next credential: %v", err)
	}
	if len(executor.calls) != 2 || executor.calls[0] == executor.calls[1] {
		t.Fatalf("executor calls = %v, want two distinct credentials", executor.calls)
	}
}
