package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type stoppableSelectorForTest struct {
	stopped atomic.Int32
}

func (s *stoppableSelectorForTest) Pick(context.Context, string, string, cliproxyexecutor.Options, []*Auth) (*Auth, error) {
	return nil, nil
}

func (s *stoppableSelectorForTest) Stop() {
	s.stopped.Add(1)
}

func TestManagerSetSelectorStopsReplacedSelector(t *testing.T) {
	oldSelector := &stoppableSelectorForTest{}
	manager := NewManager(nil, oldSelector, nil)

	manager.SetSelector(&RoundRobinSelector{})

	if got := oldSelector.stopped.Load(); got != 1 {
		t.Fatalf("old selector Stop calls = %d, want 1", got)
	}
}

func TestManagerRegisterExecutorNormalizesProviderKey(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "MiXeD"})

	if _, ok := manager.Executor("mixed"); !ok {
		t.Fatal("Executor(mixed) missing after mixed-case registration")
	}
	manager.UnregisterExecutor("mixed")
	if _, ok := manager.Executor("mixed"); ok {
		t.Fatal("Executor(mixed) still present after lowercase unregister")
	}
}

func TestManagerRemoveDeletesModelPoolOffsetsForAuth(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	manager.auths["auth-1"] = &Auth{ID: "auth-1", Provider: "openai"}
	manager.modelPoolOffsets["auth-1|openai|model-a"] = 3
	manager.modelPoolOffsets["auth-1|openai|model-b"] = 4
	manager.modelPoolOffsets["auth-2|openai|model-a"] = 5

	manager.Remove(context.Background(), "auth-1")

	for key := range manager.modelPoolOffsets {
		if key == "auth-1|openai|model-a" || key == "auth-1|openai|model-b" {
			t.Fatalf("modelPoolOffsets still contains removed auth key %q", key)
		}
	}
	if got := manager.modelPoolOffsets["auth-2|openai|model-a"]; got != 5 {
		t.Fatalf("remaining offset = %d, want 5", got)
	}
}

func TestSessionCacheStopIsConcurrentSafe(t *testing.T) {
	cache := NewSessionCache(time.Millisecond)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.Stop()
		}()
	}
	wg.Wait()
}
