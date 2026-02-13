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

func TestStripCodexPrefix(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		// Models with codex- prefix should be stripped.
		{"codex-gpt-5", "gpt-5"},
		{"codex-gpt-5-minimal", "gpt-5-minimal"},
		{"codex-gpt-5-low", "gpt-5-low"},
		{"codex-gpt-5-medium", "gpt-5-medium"},
		{"codex-gpt-5-high", "gpt-5-high"},
		{"codex-gpt-5-codex", "gpt-5-codex"},
		{"codex-gpt-5-codex-low", "gpt-5-codex-low"},
		{"codex-gpt-5-codex-medium", "gpt-5-codex-medium"},
		{"codex-gpt-5-codex-high", "gpt-5-codex-high"},
		{"codex-gpt-5-codex-mini", "gpt-5-codex-mini"},
		{"codex-gpt-5-codex-mini-medium", "gpt-5-codex-mini-medium"},
		{"codex-gpt-5-codex-mini-high", "gpt-5-codex-mini-high"},
		{"codex-gpt-5.1", "gpt-5.1"},
		{"codex-gpt-5.1-none", "gpt-5.1-none"},
		{"codex-gpt-5.1-low", "gpt-5.1-low"},
		{"codex-gpt-5.1-medium", "gpt-5.1-medium"},
		{"codex-gpt-5.1-high", "gpt-5.1-high"},
		{"codex-gpt-5.1-codex", "gpt-5.1-codex"},
		{"codex-gpt-5.1-codex-max", "gpt-5.1-codex-max"},
		{"codex-gpt-5.1-codex-max-low", "gpt-5.1-codex-max-low"},
		{"codex-gpt-5.1-codex-max-medium", "gpt-5.1-codex-max-medium"},
		{"codex-gpt-5.1-codex-max-high", "gpt-5.1-codex-max-high"},
		{"codex-gpt-5.1-codex-max-xhigh", "gpt-5.1-codex-max-xhigh"},
		{"codex-gpt-5.2", "gpt-5.2"},
		{"codex-gpt-5.2-none", "gpt-5.2-none"},
		{"codex-gpt-5.2-low", "gpt-5.2-low"},
		{"codex-gpt-5.2-medium", "gpt-5.2-medium"},
		{"codex-gpt-5.2-high", "gpt-5.2-high"},
		{"codex-gpt-5.2-xhigh", "gpt-5.2-xhigh"},
		{"codex-gpt-5.2-codex", "gpt-5.2-codex"},
		{"codex-gpt-5.2-codex-low", "gpt-5.2-codex-low"},
		{"codex-gpt-5.2-codex-medium", "gpt-5.2-codex-medium"},
		{"codex-gpt-5.2-codex-high", "gpt-5.2-codex-high"},
		{"codex-gpt-5.2-codex-xhigh", "gpt-5.2-codex-xhigh"},
		{"codex-gpt-5.3-codex", "gpt-5.3-codex"},
		{"codex-gpt-5.3-codex-low", "gpt-5.3-codex-low"},
		{"codex-gpt-5.3-codex-medium", "gpt-5.3-codex-medium"},
		{"codex-gpt-5.3-codex-high", "gpt-5.3-codex-high"},
		{"codex-gpt-5.3-codex-xhigh", "gpt-5.3-codex-xhigh"},

		// Models without codex- prefix should pass through unchanged.
		{"gpt-5", "gpt-5"},
		{"gpt-5.2-xhigh", "gpt-5.2-xhigh"},
		{"gpt-5.3-codex-xhigh", "gpt-5.3-codex-xhigh"},
		{"gpt-5-codex", "gpt-5-codex"},
		{"claude-opus-4.5", "claude-opus-4.5"},
		{"gemini-3-pro-preview", "gemini-3-pro-preview"},

		// Edge cases.
		{"", ""},
		{"codex-", ""},
		{"CODEX-gpt-5", "CODEX-gpt-5"}, // case-sensitive: uppercase not stripped
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripCodexPrefix(tt.input)
			if got != tt.want {
				t.Errorf("stripCodexPrefix(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestCodexExecutor_CodexPrefixStrippedInUpstreamRequest verifies that when a
// request arrives with a codex- prefixed model name, the codex executor strips
// the prefix before sending the model name upstream.
func TestCodexExecutor_CodexPrefixStrippedInUpstreamRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		requestModel  string
		wantModel     string
		wantEffort    string
		wantHasEffort bool
	}{
		// codex- prefix with effort alias: prefix stripped, then alias resolved.
		{
			requestModel:  "codex-gpt-5.2-xhigh",
			wantModel:     "gpt-5.2",
			wantEffort:    "xhigh",
			wantHasEffort: true,
		},
		// codex- prefix with base model (no effort alias).
		{
			requestModel:  "codex-gpt-5.2-codex",
			wantModel:     "gpt-5.2-codex",
			wantHasEffort: false,
		},
		// codex- prefix with 5.3 codex-xhigh effort alias.
		{
			requestModel:  "codex-gpt-5.3-codex-xhigh",
			wantModel:     "gpt-5.3-codex",
			wantEffort:    "xhigh",
			wantHasEffort: true,
		},
		// codex- prefix with gpt-5.1-codex-max-xhigh effort alias.
		{
			requestModel:  "codex-gpt-5.1-codex-max-xhigh",
			wantModel:     "gpt-5.1-codex-max",
			wantEffort:    "xhigh",
			wantHasEffort: true,
		},
		// codex- prefix with gpt-5-high effort alias.
		{
			requestModel:  "codex-gpt-5-high",
			wantModel:     "gpt-5",
			wantEffort:    "high",
			wantHasEffort: true,
		},
		// No prefix — should work identically (regression guard).
		{
			requestModel:  "gpt-5.2-xhigh",
			wantModel:     "gpt-5.2",
			wantEffort:    "xhigh",
			wantHasEffort: true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.requestModel, func(t *testing.T) {
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
				ID:       "codex-auth-test",
				Provider: "codex",
				Attributes: map[string]string{
					"api_key":  "test",
					"base_url": srv.URL,
				},
			}

			req := cliproxyexecutor.Request{
				Model:   tt.requestModel,
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
				t.Fatal("expected upstream request body to be captured")
			}

			gotModel := gjson.GetBytes(upstreamBody, "model").String()
			if gotModel != tt.wantModel {
				t.Errorf("upstream model=%q, want %q", gotModel, tt.wantModel)
			}

			gotEffort := gjson.GetBytes(upstreamBody, "reasoning.effort").String()
			if tt.wantHasEffort {
				if gotEffort != tt.wantEffort {
					t.Errorf("upstream reasoning.effort=%q, want %q", gotEffort, tt.wantEffort)
				}
			} else {
				if gotEffort != "" {
					t.Errorf("upstream reasoning.effort=%q, want empty (no effort for base model)", gotEffort)
				}
			}
		})
	}
}
