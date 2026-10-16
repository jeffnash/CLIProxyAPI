package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestMetaProxyConnectRetryHTTP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stream    bool
		retries   int
		failures  int32
		wantCalls int32
		wantError bool
	}{
		{"execute recovery", false, 2, 2, 3, false},
		{"stream recovery", true, 2, 2, 3, false},
		{"execute exhausted", false, 2, 3, 3, true},
		{"stream exhausted", true, 2, 3, 3, true},
		{"retries disabled", false, 0, 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"meta_proxy_ok\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"muse-wonder-full-1.0-contributor\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"META_PROXY_OK\"}]}]}}\n\n")
			}))
			defer upstream.Close()
			var connects atomic.Int32
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodConnect || r.Host != strings.TrimPrefix(upstream.URL, "https://") {
					t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if connects.Add(1) <= tc.failures {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				origin, errDial := net.Dial("tcp", r.Host)
				if errDial != nil {
					t.Error(errDial)
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				defer origin.Close()
				client, buffered, errHijack := w.(http.Hijacker).Hijack()
				if errHijack != nil {
					t.Error(errHijack)
					return
				}
				defer client.Close()
				if _, errWrite := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); errWrite != nil {
					t.Error(errWrite)
					return
				}
				go func() { _, _ = io.Copy(origin, buffered) }()
				_, _ = io.Copy(client, origin)
			}))
			defer proxy.Close()
			proxyURL, err := url.Parse(proxy.URL)
			if err != nil {
				t.Fatal(err)
			}
			transport := upstream.Client().Transport.(*http.Transport).Clone()
			transport.Proxy = http.ProxyURL(proxyURL)
			defer transport.CloseIdleConnections()
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", transport)
			const model = "muse-wonder-full-1.0-contributor"
			cfg := &config.Config{RequestRetry: tc.retries}
			manager := auth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.SetRetryConfig(tc.retries, 0, 0)
			manager.RegisterExecutor(runtimeexecutor.NewMetaExecutor(cfg))
			id := "meta-proxy-connect-retry"
			registry.GetGlobalRegistry().RegisterClient(id, "meta", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(id)
			_, err = manager.Register(ctx, &auth.Auth{ID: id, Provider: "meta", Attributes: map[string]string{"base_url": upstream.URL + "/v1", "api_key": "test-only"}})
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(fmt.Sprintf(`{"model":%q,"input":"hello","stream":%v}`, model, tc.stream))
			req := ex.Request{Model: model, Payload: payload}
			opts := ex.Options{SourceFormat: translator.FormatOpenAIResponse, OriginalRequest: payload, Stream: tc.stream}
			var output []byte
			if tc.stream {
				response, errStream := manager.ExecuteStream(ctx, []string{"meta"}, req, opts)
				err = errStream
				if response != nil {
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output = append(output, chunk.Payload...)
					}
				}
			} else {
				response, errExecute := manager.Execute(ctx, []string{"meta"}, req, opts)
				err = errExecute
				output = response.Payload
			}
			if connects.Load() != tc.wantCalls || (err != nil) != tc.wantError {
				t.Fatalf("CONNECT attempts=%d, err=%v", connects.Load(), err)
			}
			if tc.wantError {
				if !strings.Contains(err.Error(), "Forbidden") {
					t.Fatalf("lost proxy error: %v", err)
				}
			} else if !strings.Contains(string(output), "META_PROXY_OK") {
				t.Fatalf("lost response text: %s", output)
			}
			credential, _ := manager.GetByID(id)
			if credential.Unavailable || !credential.NextRetryAfter.IsZero() {
				t.Fatal("proxy CONNECT failure suspended the Meta credential")
			}
			if state := credential.ModelStates[model]; state != nil && (state.Unavailable || !state.NextRetryAfter.IsZero()) {
				t.Fatal("proxy CONNECT failure cooled down the Meta model")
			}
		})
	}
}

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

func TestMetaModelNotFoundRetryHTTP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		stream    bool
		retries   int
		failures  int32
		wantCalls int32
		wantError bool
	}{
		{"execute recovery", false, 2, 2, 3, false},
		{"stream recovery", true, 2, 2, 3, false},
		{"execute exhausted", false, 2, 3, 3, true},
		{"stream exhausted", true, 2, 3, 3, true},
		{"retries disabled", false, 0, 2, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			const model = "muse-wonder-full-1.0-contributor"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/responses" {
					t.Errorf("path=%s", r.URL.Path)
				}
				body, errRead := io.ReadAll(r.Body)
				if errRead != nil {
					t.Error(errRead)
					return
				}
				if !strings.Contains(string(body), model) {
					t.Errorf("lost model: %s", body)
				}
				if calls.Add(1) <= tc.failures {
					w.WriteHeader(http.StatusNotFound)
					fmt.Fprint(w, `{"error":{"code":"model_not_found","message":"The requested model was not found.","param":null,"type":"invalid_request_error"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"meta_retry_ok\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"muse-wonder-full-1.0-contributor\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"META_RETRY_OK\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
			}))
			defer server.Close()
			cfg := &config.Config{RequestRetry: tc.retries}
			manager := auth.NewManager(nil, nil, nil)
			manager.SetConfig(cfg)
			manager.SetRetryConfig(tc.retries, 0, 0)
			manager.RegisterExecutor(runtimeexecutor.NewMetaExecutor(cfg))
			id := "meta-model-not-found-retry"
			registry.GetGlobalRegistry().RegisterClient(id, "meta", []*registry.ModelInfo{{ID: model}})
			defer registry.GetGlobalRegistry().UnregisterClient(id)
			_, err := manager.Register(t.Context(), &auth.Auth{ID: id, Provider: "meta", ProxyURL: "direct", Attributes: map[string]string{"base_url": server.URL + "/v1", "api_key": "test-only"}})
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte(fmt.Sprintf(`{"model":%q,"input":"Reply with exactly: META_RETRY_OK","stream":%v}`, model, tc.stream))
			req := ex.Request{Model: model, Payload: payload}
			opts := ex.Options{SourceFormat: translator.FormatOpenAIResponse, OriginalRequest: payload, Stream: tc.stream}
			var output []byte
			if tc.stream {
				response, errStream := manager.ExecuteStream(t.Context(), []string{"meta"}, req, opts)
				err = errStream
				if response != nil {
					for chunk := range response.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
						output = append(output, chunk.Payload...)
					}
				}
			} else {
				response, errExecute := manager.Execute(t.Context(), []string{"meta"}, req, opts)
				err = errExecute
				output = response.Payload
			}
			if calls.Load() != tc.wantCalls || (err != nil) != tc.wantError {
				t.Fatalf("calls=%d err=%v output=%s", calls.Load(), err, output)
			}
			if tc.wantError {
				statusError, ok := err.(interface{ StatusCode() int })
				if !ok || statusError.StatusCode() != http.StatusNotFound || !strings.Contains(err.Error(), "model_not_found") {
					t.Fatalf("lost upstream model error: %v", err)
				}
			} else if !strings.Contains(string(output), "META_RETRY_OK") {
				t.Fatalf("lost response text: %s", output)
			}
			credential, _ := manager.GetByID(id)
			if credential.Unavailable {
				t.Fatal("transient Meta model failure suspended credential")
			}
		})
	}
}
