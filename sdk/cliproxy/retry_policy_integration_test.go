package cliproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestRetryPolicyClaudeHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/messages" {
					t.Errorf("path=%s", r.URL.Path)
				}
				body, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(body), "policy-http-model") {
					t.Errorf("lost model: %s", body)
				}
				if calls.Add(1) <= 2 {
					w.WriteHeader(404)
					fmt.Fprint(w, `{"type":"error","error":{"type":"not_found_error","message":"Model not found or access denied"}}`)
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"policy-http-model\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"HTTP_RETRY_OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"test","type":"message","role":"assistant","model":"policy-http-model","content":[{"type":"text","text":"HTTP_RETRY_OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()
			cfg := &config.Config{RetryPolicies: []config.RetryPolicy{{Providers: []string{"claude"}, Models: []string{"policy-http-model"}, BaseURL: server.URL, MaxRetries: 2, DelaysSeconds: []float64{0}, Errors: []config.RetryPolicyError{{Status: 404, Contains: "Model not found"}}}}}
			m := auth.NewManager(nil, nil, nil)
			m.SetConfig(cfg)
			m.SetRetryConfig(0, 0, 30)
			m.RegisterExecutor(runtimeexecutor.NewClaudeExecutor(cfg))
			id := "policy-http-auth"
			registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: "policy-http-model"}})
			defer registry.GetGlobalRegistry().UnregisterClient(id)
			_, err := m.Register(t.Context(), &auth.Auth{ID: id, Provider: "claude", ProxyURL: "direct", Attributes: map[string]string{"base_url": server.URL, "api_key": "test-only"}})
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(fmt.Sprintf(`{"model":"policy-http-model","max_tokens":64,"stream":%v,"messages":[{"role":"user","content":"hello"}]}`, stream))
			req := ex.Request{Model: "policy-http-model", Payload: payload}
			opts := ex.Options{SourceFormat: translator.FormatClaude, OriginalRequest: payload, Stream: stream}
			var output []byte
			if stream {
				r, e := m.ExecuteStream(t.Context(), []string{"claude"}, req, opts)
				err = e
				if r != nil {
					for c := range r.Chunks {
						if c.Err != nil {
							t.Fatal(c.Err)
						}
						output = append(output, c.Payload...)
					}
				}
			} else {
				r, e := m.Execute(t.Context(), []string{"claude"}, req, opts)
				err = e
				output = r.Payload
			}
			if err != nil || calls.Load() != 3 || !strings.Contains(string(output), "HTTP_RETRY_OK") {
				t.Fatalf("calls=%d err=%v output=%s", calls.Load(), err, output)
			}
		})
	}
}
