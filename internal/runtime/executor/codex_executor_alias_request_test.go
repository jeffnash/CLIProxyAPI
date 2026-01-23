package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutor_AliasSetsModelAndReasoningEffortInUpstreamRequest(t *testing.T) {
	t.Parallel()

	received := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		received <- body

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"type":"response.completed","response":{"id":"r1","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	}))
	t.Cleanup(srv.Close)

	exec := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "codex-auth-1",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":   "test",
			"base_url":  srv.URL,
			"api_type":  "api_key",
			"accountId": "test",
		},
	}

	req := cliproxyexecutor.Request{
		Model:   "gpt-5.1-codex-max-xhigh",
		Payload: []byte(`{"input":[]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("codex"),
		Stream:       false,
	}

	_, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute(): %v", err)
	}

	var upstreamBody []byte
	select {
	case upstreamBody = <-received:
	default:
		t.Fatalf("expected upstream request body to be captured")
	}

	if got := gjson.GetBytes(upstreamBody, "model").String(); got != "gpt-5.1-codex-max" {
		t.Fatalf("upstream model=%q, want %q", got, "gpt-5.1-codex-max")
	}
	if got := gjson.GetBytes(upstreamBody, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("upstream reasoning.effort=%q, want %q", got, "xhigh")
	}
	if got := gjson.GetBytes(upstreamBody, "stream").Bool(); !got {
		t.Fatalf("upstream stream=%v, want true", got)
	}
}

