package cliproxy

import (
	"context"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
)

func TestApplyCoreAuthAddOrUpdate_PreservesRuntimeState(t *testing.T) {
	t.Parallel()

	mgr := coreauth.NewManager(nil, nil, nil)

	// Register an auth with runtime state
	existingAuth := &coreauth.Auth{
		ID:            "test-auth-1",
		Provider:      "iflow",
		Status:        coreauth.StatusError,
		StatusMessage: "upstream error",
		Unavailable:   true,
		NextRetryAfter: time.Now().Add(5 * time.Minute),
		LastError: &coreauth.Error{
			Code:    "upstream_error",
			Message: "Too many people chatting",
		},
		Quota: coreauth.QuotaState{
			Exceeded:      true,
			Reason:        "rate_limit",
			NextRecoverAt: time.Now().Add(10 * time.Minute),
			BackoffLevel:  2,
		},
		ModelStates: map[string]*coreauth.ModelState{
			"qwen-max": {
				Unavailable:    true,
				Status:         coreauth.StatusError,
				StatusMessage:  "model temporarily unavailable",
				NextRetryAfter: time.Now().Add(3 * time.Minute),
			},
		},
	}

	if _, err := mgr.Register(context.Background(), existingAuth); err != nil {
		t.Fatalf("mgr.Register: %v", err)
	}

	s := &Service{coreManager: mgr}

	// Simulate an auth update from file watcher (fresh auth without runtime state)
	freshAuth := &coreauth.Auth{
		ID:       "test-auth-1",
		Provider: "iflow",
		// No runtime state - simulates what comes from file re-parse
	}

	s.applyCoreAuthAddOrUpdate(context.Background(), freshAuth)

	// Verify runtime state was preserved
	updated, ok := mgr.GetByID("test-auth-1")
	if !ok || updated == nil {
		t.Fatal("expected auth to exist after update")
	}

	if !updated.Unavailable {
		t.Error("expected Unavailable to be preserved as true")
	}
	if updated.NextRetryAfter.IsZero() {
		t.Error("expected NextRetryAfter to be preserved")
	}
	if updated.Status != coreauth.StatusError {
		t.Errorf("expected Status to be preserved as StatusError, got %v", updated.Status)
	}
	if updated.LastError == nil || updated.LastError.Message != "Too many people chatting" {
		t.Error("expected LastError to be preserved")
	}
	if updated.StatusMessage != "upstream error" {
		t.Errorf("expected StatusMessage to be preserved, got %q", updated.StatusMessage)
	}
	if !updated.Quota.Exceeded {
		t.Error("expected Quota.Exceeded to be preserved as true")
	}
	if updated.Quota.BackoffLevel != 2 {
		t.Errorf("expected Quota.BackoffLevel to be 2, got %d", updated.Quota.BackoffLevel)
	}
	if updated.ModelStates == nil || len(updated.ModelStates) == 0 {
		t.Fatal("expected ModelStates to be preserved")
	}
	if ms, ok := updated.ModelStates["qwen-max"]; !ok || ms == nil {
		t.Error("expected qwen-max model state to be preserved")
	} else if !ms.Unavailable {
		t.Error("expected qwen-max model state Unavailable to be preserved as true")
	}
}

func TestApplyCoreAuthAddOrUpdate_NewAuthHasNoRuntimeState(t *testing.T) {
	t.Parallel()

	mgr := coreauth.NewManager(nil, nil, nil)
	s := &Service{coreManager: mgr}

	// Register a new auth (no existing auth with this ID)
	newAuth := &coreauth.Auth{
		ID:       "new-auth-1",
		Provider: "gemini",
	}

	s.applyCoreAuthAddOrUpdate(context.Background(), newAuth)

	registered, ok := mgr.GetByID("new-auth-1")
	if !ok || registered == nil {
		t.Fatal("expected auth to be registered")
	}

	// New auth should have default (zero) runtime state
	if registered.Unavailable {
		t.Error("expected new auth to not be Unavailable")
	}
	if !registered.NextRetryAfter.IsZero() {
		t.Error("expected new auth NextRetryAfter to be zero")
	}
	if len(registered.ModelStates) != 0 {
		t.Error("expected new auth to have no ModelStates")
	}
}
