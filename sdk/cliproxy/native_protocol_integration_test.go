package cliproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	auth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	ex "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestNativeProtocolsThroughManager(t *testing.T) {
	for _, protocol := range []struct {
		name, path        string
		format            translator.Format
		request, response string
	}{
		{"claude", "/v1/messages", translator.FormatClaude, `"max_tokens":2048,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"result"}]}]`, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":8}}`},
		{"openai", "/v1/chat/completions", translator.FormatOpenAI, `"messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"result"}],"prompt_cache_key":"stable-key"`, `{"id":"chat_1","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":1,"prompt_tokens_details":{"cached_tokens":8}}}`},
		{"responses", "/v1/responses", translator.FormatOpenAIResponse, `"input":[{"role":"user","content":"hi"},{"type":"reasoning","summary":[],"encrypted_content":"opaque-replay"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"result"}],"store":false,"include":["reasoning.encrypted_content"],"prompt_cache_key":"stable-key","max_output_tokens":2048`, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"reasoning","summary":[],"encrypted_content":"opaque-next"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":10,"output_tokens":1,"input_tokens_details":{"cached_tokens":8}}}`},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol.name, stream), func(t *testing.T) {
				calls := 0
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					if r.URL.Path != protocol.path {
						t.Errorf("path=%s want %s", r.URL.Path, protocol.path)
					}
					body, _ := io.ReadAll(r.Body)
					if gjson.GetBytes(body, "model").String() != "muse-spark-1.3-contributor" {
						t.Errorf("model lost: %s", body)
					}
					if !strings.Contains(string(body), "call_1") {
						t.Errorf("tool correlation lost: %s", body)
					}
					if protocol.name == "responses" {
						for _, marker := range []string{"opaque-replay", "stable-key", "reasoning.encrypted_content"} {
							if !strings.Contains(string(body), marker) {
								t.Errorf("lost %s: %s", marker, body)
							}
						}
						if gjson.GetBytes(body, "store").Bool() || gjson.GetBytes(body, "stream_options").Exists() {
							t.Errorf("responses request altered: %s", body)
						}
					}
					w.Header().Set("Content-Type", "application/json")
					if !stream {
						fmt.Fprint(w, protocol.response)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					switch protocol.name {
					case "responses":
						fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", protocol.response)
					case "openai":
						fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"}}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n")
					case "claude":
						fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":%s}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", protocol.response)
					}
				}))
				defer upstream.Close()
				cfg := &config.Config{Passthru: []config.PassthruRoute{{Model: "muse-spark-1.3-contributor", Protocols: []string{"claude", "openai", "responses"}, BaseURL: upstream.URL, APIKey: "test-only", ProxyURL: "direct"}}}
				records, err := synthesizer.NewConfigSynthesizer().Synthesize(&synthesizer.SynthesisContext{Config: cfg, Now: time.Now(), IDGenerator: synthesizer.NewStableIDGenerator()})
				if err != nil || len(records) != 1 {
					t.Fatalf("records=%v err=%v", records, err)
				}
				manager := auth.NewManager(nil, nil, nil)
				manager.SetConfig(cfg)
				service := &Service{cfg: cfg, coreManager: manager}
				service.registerExecutorForAuth(records[0], true)
				service.registerModelsForAuth(t.Context(), records[0])
				defer registry.GetGlobalRegistry().UnregisterClient(records[0].ID)
				if _, err = manager.Register(t.Context(), records[0]); err != nil {
					t.Fatal(err)
				}
				body := []byte(fmt.Sprintf(`{"model":"muse-spark-1.3-contributor","stream":%v,%s}`, stream, protocol.request))
				req := ex.Request{Model: "muse-spark-1.3-contributor", Payload: body}
				opts := ex.Options{SourceFormat: protocol.format, OriginalRequest: body, Stream: stream}
				var output []byte
				if stream {
					result, e := manager.ExecuteStream(t.Context(), []string{"passthru-native"}, req, opts)
					if e != nil {
						t.Fatal(e)
					}
					for c := range result.Chunks {
						if c.Err != nil {
							t.Fatal(c.Err)
						}
						output = append(output, c.Payload...)
					}
				} else {
					result, e := manager.Execute(t.Context(), []string{"passthru-native"}, req, opts)
					if e != nil {
						t.Fatal(e)
					}
					output = result.Payload
				}
				if calls != 1 || !strings.Contains(string(output), "OK") {
					t.Fatalf("calls=%d output=%s", calls, output)
				}
				if protocol.name == "responses" && !strings.Contains(string(output), "opaque-next") {
					t.Fatalf("encrypted response lost: %s", output)
				}
			})
		}
	}
}
