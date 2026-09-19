package helps

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClassifyCodeBuddyError(t *testing.T) {
	now := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	classify := func(status int, headers map[string]string, body string) *CodeBuddyError {
		t.Helper()
		header := http.Header{}
		for key, value := range headers {
			header.Set(key, value)
		}
		err := ClassifyCodeBuddyError(status, header, []byte(body), now)
		typed, ok := err.(*CodeBuddyError)
		if !ok {
			t.Fatalf("type = %T", err)
		}
		return typed
	}
	cases := []struct {
		name      string
		status    int
		headers   map[string]string
		body      string
		wantCode  int
		wantErr   string
		reqScope  bool
		credScope bool
		wantDelay bool
	}{
		{"expired token", 401, nil, `{"code":0,"msg":"ok"}`, 401, "unauthorized", false, true, false},
		{"offline session", 200, nil, `{"code":12153,"msg":"Offline user session not found"}`, 401, "session_expired", false, true, false},
		{"rate 11140", 200, map[string]string{"Retry-After": "3"}, `{"code":11140,"msg":"The model provider is rate-limiting requests."}`, 429, "rate_limited", false, false, true},
		{"forbidden 11140", 403, nil, `{"code":11140,"msg":"request illegal"}`, 403, "account_forbidden", false, true, false},
		{"credits", 402, nil, `{"code":0,"msg":"insufficient credit"}`, 402, "insufficient_credits", false, true, false},
		{"trial", 403, nil, `{"code":14017,"msg":"trial not activated"}`, 403, "trial_not_activated", false, true, false},
		{"model blocked", 400, nil, `{"code":11102,"msg":"service info not found"}`, 404, "model_not_found", false, false, false},
		{"prompt long", 200, nil, `{"code":11115,"msg":"prompt is too long"}`, 400, "prompt_too_long", true, false, false},
		{"bad params", 400, nil, `{"code":11101,"msg":"Unmarshal chat params failed"}`, 400, "bad_request", true, false, false},
		{"missing reasoning", 200, nil, `{"code":11155,"msg":"reasoning_content_missing"}`, 400, "reasoning_content_missing", true, false, false},
		{"missing reasoning text", 400, nil, `{"code":0,"msg":"Rejected: reasoning_content_missing on turn 3"}`, 400, "reasoning_content_missing", true, false, false},
		{"policy", 400, nil, `{"code":0,"msg":"blocked by security policy"}`, 400, "content_policy", true, false, false},
		{"waf", 403, nil, `<html>blocked</html>`, 403, "waf_blocked", false, false, false},
		{"bare 429", 429, nil, `{"code":0,"msg":"slow down"}`, 429, "too_many_requests", false, false, false},
		{"account quota 429", 429, nil, `{"code":0,"msg":"account quota exceeded"}`, 429, "account_quota", false, true, false},
		{"server", 503, nil, `oops`, 503, "upstream_error", false, false, false},
		{"generic 400", 400, nil, `{"code":0,"msg":"bad"}`, 400, "bad_request", true, false, false},
		{"unknown business", 200, nil, `{"code":99999,"msg":"mystery"}`, 502, "upstream_error", false, false, false},
	}
	for _, tc := range cases {
		err := classify(tc.status, tc.headers, tc.body)
		if err.StatusCode() != tc.wantCode || err.Code != tc.wantErr {
			t.Fatalf("%s: got %d/%s", tc.name, err.StatusCode(), err.Code)
		}
		if err.IsRequestScoped() != tc.reqScope || err.IsCredentialScoped() != tc.credScope {
			t.Fatalf("%s: scopes req=%v cred=%v", tc.name, err.IsRequestScoped(), err.IsCredentialScoped())
		}
		if (err.RetryAfter() != nil) != tc.wantDelay {
			t.Fatalf("%s: delay = %v", tc.name, err.RetryAfter())
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(err.Error()), &payload); err != nil {
			t.Fatalf("%s: envelope invalid: %v", tc.name, err)
		}
	}
	t.Run("retry after variants", func(t *testing.T) {
		err := classify(429, map[string]string{"Retry-After-Ms": "1500"}, `{"code":0,"msg":"x"}`)
		if err.RetryAfter() == nil || *err.RetryAfter() != 1500*time.Millisecond {
			t.Fatalf("delay = %v", err.RetryAfter())
		}
		err = classify(429, map[string]string{"Retry-After": "Fri, 18 Sep 2026 00:01:00 GMT"}, `{"code":0,"msg":"x"}`)
		if err.RetryAfter() == nil || *err.RetryAfter() != time.Minute {
			t.Fatalf("delay = %v", err.RetryAfter())
		}
		for _, headers := range []map[string]string{{"Retry-After": "-1"}, {"Retry-After": "999999"}, {"Retry-After-Ms": "nope"}} {
			if err := classify(429, headers, `{"code":0,"msg":"x"}`); err.RetryAfter() != nil {
				t.Fatalf("invalid delay honored: %v", err.RetryAfter())
			}
		}
	})
	t.Run("model not found envelope", func(t *testing.T) {
		err := classify(400, nil, `{"code":11102,"msg":"service info not found for hy4-preview-z"}`)
		if !strings.Contains(err.Error(), "model_not_found") || !strings.Contains(err.Error(), "hy4-preview-z") {
			t.Fatalf("envelope = %s", err.Error())
		}
	})
}

func TestWrapCodeBuddyStreamError(t *testing.T) {
	if err := WrapCodeBuddyStreamError(nil); err != nil {
		t.Fatalf("nil wrapped: %v", err)
	}
	typed := &CodeBuddyError{Status: http.StatusBadRequest, Code: "bad_request"}
	if err := WrapCodeBuddyStreamError(typed); err != typed {
		t.Fatalf("typed error rewritten: %v", err)
	}
	wrapped := WrapCodeBuddyStreamError(errors.New("codebuddy: empty stream (no payload before close)"))
	got, ok := wrapped.(*CodeBuddyError)
	if !ok || got.StatusCode() != http.StatusBadGateway || got.Code != "upstream_error" {
		t.Fatalf("wrapped = %#v", wrapped)
	}
	if got.IsRequestScoped() || got.IsCredentialScoped() {
		t.Fatalf("upstream failure scoped: %#v", got)
	}
}
