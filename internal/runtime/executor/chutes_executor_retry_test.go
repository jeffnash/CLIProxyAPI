package executor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
)

func withChutesBackoffSchedule(schedule []time.Duration, fn func()) {
	prev := chutesBackoffSchedule
	chutesBackoffSchedule = schedule
	defer func() { chutesBackoffSchedule = prev }()
	fn()
}

// Tests for parseChutesBackoffSchedule

func TestParseChutesBackoffSchedule_NilConfig(t *testing.T) {
	schedule := parseChutesBackoffSchedule(nil)
	if len(schedule) != len(chutesBackoffSchedule) {
		t.Fatalf("expected default schedule length %d, got %d", len(chutesBackoffSchedule), len(schedule))
	}
}

func TestParseChutesBackoffSchedule_EmptyString(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = ""
	schedule := parseChutesBackoffSchedule(cfg)
	if len(schedule) != len(chutesBackoffSchedule) {
		t.Fatalf("expected default schedule length %d, got %d", len(chutesBackoffSchedule), len(schedule))
	}
}

func TestParseChutesBackoffSchedule_ValidSingleValue(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "10"
	schedule := parseChutesBackoffSchedule(cfg)
	if len(schedule) != 1 {
		t.Fatalf("expected 1 value, got %d", len(schedule))
	}
	if schedule[0] != 10*time.Second {
		t.Fatalf("expected 10s, got %s", schedule[0])
	}
}

func TestParseChutesBackoffSchedule_ValidMultipleValues(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "5,15,30,60"
	schedule := parseChutesBackoffSchedule(cfg)
	if len(schedule) != 4 {
		t.Fatalf("expected 4 values, got %d", len(schedule))
	}
	expected := []time.Duration{5 * time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	for i, exp := range expected {
		if schedule[i] != exp {
			t.Fatalf("expected %s at index %d, got %s", exp, i, schedule[i])
		}
	}
}

func TestParseChutesBackoffSchedule_ValidWithSpaces(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = " 10 , 20 , 30 "
	schedule := parseChutesBackoffSchedule(cfg)
	if len(schedule) != 3 {
		t.Fatalf("expected 3 values, got %d", len(schedule))
	}
	expected := []time.Duration{10 * time.Second, 20 * time.Second, 30 * time.Second}
	for i, exp := range expected {
		if schedule[i] != exp {
			t.Fatalf("expected %s at index %d, got %s", exp, i, schedule[i])
		}
	}
}

func TestParseChutesBackoffSchedule_InvalidValue(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "5,invalid,30"
	schedule := parseChutesBackoffSchedule(cfg)
	// Should fall back to default on invalid value
	if len(schedule) != len(chutesBackoffSchedule) {
		t.Fatalf("expected default schedule on invalid input, got length %d", len(schedule))
	}
}

func TestParseChutesBackoffSchedule_NegativeValue(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "5,-10,30"
	schedule := parseChutesBackoffSchedule(cfg)
	// Should fall back to default on negative value
	if len(schedule) != len(chutesBackoffSchedule) {
		t.Fatalf("expected default schedule on negative value, got length %d", len(schedule))
	}
}

func TestParseChutesBackoffSchedule_ZeroValue(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "0,5,10"
	schedule := parseChutesBackoffSchedule(cfg)
	if len(schedule) != 3 {
		t.Fatalf("expected 3 values, got %d", len(schedule))
	}
	if schedule[0] != 0 {
		t.Fatalf("expected 0s at index 0, got %s", schedule[0])
	}
}

func TestParseChutesBackoffSchedule_LargeValues(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "30,60,120,300"
	schedule := parseChutesBackoffSchedule(cfg)
	if len(schedule) != 4 {
		t.Fatalf("expected 4 values, got %d", len(schedule))
	}
	expected := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second}
	for i, exp := range expected {
		if schedule[i] != exp {
			t.Fatalf("expected %s at index %d, got %s", exp, i, schedule[i])
		}
	}
}

// Tests for chutesBackoffDuration with config

func TestChutesBackoffDuration_UsesConfigSchedule(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "10,20,30"

	if d := chutesBackoffDuration(cfg, 0); d != 10*time.Second {
		t.Fatalf("expected 10s for attempt 0, got %s", d)
	}
	if d := chutesBackoffDuration(cfg, 1); d != 20*time.Second {
		t.Fatalf("expected 20s for attempt 1, got %s", d)
	}
	if d := chutesBackoffDuration(cfg, 2); d != 30*time.Second {
		t.Fatalf("expected 30s for attempt 2, got %s", d)
	}
}

func TestChutesBackoffDuration_RepeatsLastValue(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "5,10"

	// Attempt beyond schedule length should repeat last value
	if d := chutesBackoffDuration(cfg, 5); d != 10*time.Second {
		t.Fatalf("expected 10s for attempt 5 (repeat last), got %s", d)
	}
}

func TestChutesBackoffDuration_NilConfigUsesDefault(t *testing.T) {
	d := chutesBackoffDuration(nil, 0)
	if d != chutesBackoffSchedule[0] {
		t.Fatalf("expected default first backoff %s, got %s", chutesBackoffSchedule[0], d)
	}
}

func TestChutesBackoffDuration_NegativeAttempt(t *testing.T) {
	cfg := &config.Config{}
	cfg.Chutes.RetryBackoff = "10,20,30"

	// Negative attempt should be treated as 0
	if d := chutesBackoffDuration(cfg, -1); d != 10*time.Second {
		t.Fatalf("expected 10s for negative attempt, got %s", d)
	}
}

func TestChutesExecutorExecuteStream_RetriesOn429ThenSucceeds(t *testing.T) {
	withChutesBackoffSchedule([]time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}, func() {
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("not found"))
				return
			}
			if calls < 2 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte("rate limited"))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"choices\": []}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer srv.Close()

		cfg := &config.Config{}
		cfg.Chutes.BaseURL = srv.URL
		cfg.Chutes.APIKey = "k"
		cfg.Chutes.MaxRetries = 3

		exec := NewChutesExecutor(cfg)
		auth := &cliproxyauth.Auth{ID: "a", Provider: "chutes", Attributes: map[string]string{"api_key": "k", "base_url": srv.URL}}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		ch, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"model":"m","messages":[]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		for range ch {
			// drain
		}
		if calls != 2 {
			t.Fatalf("expected 2 calls (429 then 200), got %d", calls)
		}
	})
}

func TestChutesExecutorExecute_MaxRetriesZero_NoRetry(t *testing.T) {
	withChutesBackoffSchedule([]time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}, func() {
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
		}))
		defer srv.Close()

		cfg := &config.Config{}
		cfg.Chutes.BaseURL = srv.URL
		cfg.Chutes.APIKey = "k"
		cfg.Chutes.MaxRetries = 0

		exec := NewChutesExecutor(cfg)
		auth := &cliproxyauth.Auth{ID: "a", Provider: "chutes", Attributes: map[string]string{"api_key": "k", "base_url": srv.URL}}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"model":"m","messages":[]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
		if err == nil {
			t.Fatalf("expected error")
		}
		if calls != 1 {
			t.Fatalf("expected 1 call when MaxRetries=0, got %d", calls)
		}
	})
}

func TestChutesExecutorExecuteStream_ReturnsRetryAfterOn429WhenOutOfRetries(t *testing.T) {
	withChutesBackoffSchedule([]time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
		}))
		defer srv.Close()

		cfg := &config.Config{}
		cfg.Chutes.BaseURL = srv.URL
		cfg.Chutes.APIKey = "k"
		cfg.Chutes.MaxRetries = 0

		exec := NewChutesExecutor(cfg)
		auth := &cliproxyauth.Auth{ID: "a", Provider: "chutes", Attributes: map[string]string{"api_key": "k", "base_url": srv.URL}}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"model":"m","messages":[]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
		if err == nil {
			t.Fatalf("expected error")
		}
		se, ok := err.(interface{ StatusCode() int; RetryAfter() *time.Duration })
		if !ok {
			t.Fatalf("expected status error with retry-after, got %T", err)
		}
		if se.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("expected status 429, got %d", se.StatusCode())
		}
		ra := se.RetryAfter()
		if ra == nil {
			t.Fatalf("expected RetryAfter")
		}
		// Should cap to short cooldown (5s), even if upstream says 60s.
		if *ra > 5*time.Second {
			t.Fatalf("expected capped RetryAfter <= 5s, got %s", ra.String())
		}
	})
}

func TestChutesExecutorExecute_RetriesOn429ThenSucceeds(t *testing.T) {
	withChutesBackoffSchedule([]time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}, func() {
		var calls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("not found"))
				return
			}
			if calls < 3 {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte("rate limited"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}))
		defer srv.Close()

		cfg := &config.Config{}
		cfg.Chutes.BaseURL = srv.URL
		cfg.Chutes.APIKey = "k"
		cfg.Chutes.MaxRetries = 4

		exec := NewChutesExecutor(cfg)
		auth := &cliproxyauth.Auth{ID: "a", Provider: "chutes", Attributes: map[string]string{"api_key": "k", "base_url": srv.URL}}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"model":"m","messages":[]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
		if err != nil {
			t.Fatalf("expected success, got error: %v", err)
		}
		if calls != 3 {
			t.Fatalf("expected 3 calls (2x429 then 200), got %d", calls)
		}
	})
}

func TestChutesExecutorExecute_ReturnsRetryAfterOn429WhenOutOfRetries(t *testing.T) {
	withChutesBackoffSchedule([]time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}, func() {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte("rate limited"))
		}))
		defer srv.Close()

		cfg := &config.Config{}
		cfg.Chutes.BaseURL = srv.URL
		cfg.Chutes.APIKey = "k"
		cfg.Chutes.MaxRetries = 0

		exec := NewChutesExecutor(cfg)
		auth := &cliproxyauth.Auth{ID: "a", Provider: "chutes", Attributes: map[string]string{"api_key": "k", "base_url": srv.URL}}

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		_, err := exec.Execute(ctx, auth, cliproxyexecutor.Request{Model: "m", Payload: []byte(`{"model":"m","messages":[]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
		if err == nil {
			t.Fatalf("expected error")
		}

		se, ok := err.(interface{ StatusCode() int; RetryAfter() *time.Duration })
		if !ok {
			t.Fatalf("expected status error with retry-after, got %T", err)
		}
		if se.StatusCode() != http.StatusTooManyRequests {
			t.Fatalf("expected status 429, got %d", se.StatusCode())
		}
		ra := se.RetryAfter()
		if ra == nil || *ra <= 0 {
			t.Fatalf("expected non-nil positive RetryAfter, got %v", ra)
		}
	})
}
