package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestApplyPayloadConfigWithRoot_PassthruRoutePayload(t *testing.T) {
	cfg := &config.Config{
		Passthru: []config.PassthruRoute{
			{
				Model:    "deepseek-v4-pro-xhigh",
				Protocol: "openai",
				BaseURL:  "https://api.deepseek.com",
				Payload: &config.PassthruPayload{
					Override: map[string]any{
						"thinking.type":    "enabled",
						"reasoning_effort": "xhigh",
					},
					Filter: []string{"temperature", "top_p"},
				},
			},
		},
	}
	cfg.AddPassthruPayloadRules()

	input := []byte(`{"model":"deepseek-v4-pro-xhigh","messages":[],"temperature":0.7,"top_p":0.9}`)
	got := ApplyPayloadConfigWithRoot(cfg, "deepseek-v4-pro-xhigh", "openai", "", input, input, "", "")

	if gotValue := gjson.GetBytes(got, "thinking.type").String(); gotValue != "enabled" {
		t.Fatalf("thinking.type = %q, want enabled; payload=%s", gotValue, got)
	}
	if gotValue := gjson.GetBytes(got, "reasoning_effort").String(); gotValue != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh; payload=%s", gotValue, got)
	}
	if gjson.GetBytes(got, "temperature").Exists() {
		t.Fatalf("temperature should be filtered; payload=%s", got)
	}
	if gjson.GetBytes(got, "top_p").Exists() {
		t.Fatalf("top_p should be filtered; payload=%s", got)
	}
}
