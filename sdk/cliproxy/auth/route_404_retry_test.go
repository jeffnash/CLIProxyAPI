package auth

import (
	"net/http"
	"testing"
	"time"
)

func TestExplicitRoute404Retry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		model    string
		provider string
		attempt  int
		disabled bool
		cooling  bool
		message  string
		want     bool
	}{
		{"retry", "muse", "claude", 0, false, false, "Model not found or access denied", true},
		{"exhausted", "muse", "claude", 10, false, false, "Model not found", false},
		{"other model", "other", "claude", 0, false, false, "Model not found", false},
		{"other provider", "muse", "openai", 0, false, false, "Model not found", false},
		{"disabled", "muse", "claude", 0, true, false, "Model not found", false},
		{"normal cooling", "muse", "claude", 0, false, true, "Model not found", false},
		{"missing history", "muse", "claude", 0, false, false, requestScopedNotFoundMessage, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			m.auths["test"] = &Auth{ID: "test", Provider: "claude", Disabled: tc.disabled,
				Metadata:    map[string]any{"request_retry": 10, "disable_cooling": !tc.cooling},
				ModelStates: map[string]*ModelState{"muse": {LastError: &Error{HTTPStatus: 404}}},
			}
			wait, got := m.shouldRetryAfterError(&Error{HTTPStatus: http.StatusNotFound, Message: tc.message}, tc.attempt, []string{tc.provider}, tc.model, 30*time.Second)
			if got != tc.want || (got && wait != time.Second) {
				t.Fatalf("got retry=%v wait=%v, want retry=%v", got, wait, tc.want)
			}
		})
	}
}
