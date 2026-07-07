package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func toolNames(payload []byte) []string {
	var out []string
	for _, t := range gjson.GetBytes(payload, "tools").Array() {
		name := t.Get("name").String()
		if name == "" {
			name = t.Get("function.name").String()
		}
		out = append(out, name)
	}
	return out
}

func TestDropToolsFromPayload(t *testing.T) {
	// Anthropic-style tools (tools[].name), incl. an MCP-prefixed name matched by suffix.
	anthropic := []byte(`{"tools":[` +
		`{"name":"Bash"},` +
		`{"name":"mcp__plugin_chrome-devtools-mcp_chrome-devtools__get_console_message"},` +
		`{"name":"Read"}` +
		`]}`)
	out := dropToolsFromPayload(anthropic, "", []string{"get_console_message"})
	got := toolNames(out)
	if len(got) != 2 || got[0] != "Bash" || got[1] != "Read" {
		t.Fatalf("suffix drop: got %v, want [Bash Read]", got)
	}

	// OpenAI-style tools (tools[].function.name), exact match.
	openai := []byte(`{"tools":[` +
		`{"type":"function","function":{"name":"keep_me"}},` +
		`{"type":"function","function":{"name":"drop_me"}}` +
		`]}`)
	out = dropToolsFromPayload(openai, "", []string{"drop_me"})
	got = toolNames(out)
	if len(got) != 1 || got[0] != "keep_me" {
		t.Fatalf("openai exact drop: got %v, want [keep_me]", got)
	}

	// No matching names leaves the payload unchanged.
	out = dropToolsFromPayload(anthropic, "", []string{"nonexistent"})
	if len(toolNames(out)) != 3 {
		t.Fatalf("no-match should keep all 3 tools, got %v", toolNames(out))
	}

	// Empty names / no tools array are safe no-ops.
	if got := string(dropToolsFromPayload(anthropic, "", nil)); got != string(anthropic) {
		t.Fatalf("nil names should be a no-op")
	}
	if got := string(dropToolsFromPayload([]byte(`{"messages":[]}`), "", []string{"x"})); got != `{"messages":[]}` {
		t.Fatalf("missing tools array should be a no-op")
	}
}

func TestApplyPayloadConfig_DropToolsRule(t *testing.T) {
	cfg := &config.Config{
		Payload: config.PayloadConfig{
			DropTools: []config.PayloadDropToolsRule{{
				Models: []config.PayloadModelRule{{Name: "claude-fable-5-a6", Protocol: "claude"}},
				Tools:  []string{"get_console_message", "get_network_request"},
			}},
		},
	}
	payload := []byte(`{"model":"claude-fable-5-a6","tools":[` +
		`{"name":"Bash"},` +
		`{"name":"mcp__x__get_console_message"},` +
		`{"name":"mcp__x__get_network_request"},` +
		`{"name":"Read"}` +
		`]}`)

	out := ApplyPayloadConfigWithRequest(cfg, "claude-fable-5-a6", "claude", "", "", payload, nil, "", "", nil)
	got := toolNames(out)
	if len(got) != 2 || got[0] != "Bash" || got[1] != "Read" {
		t.Fatalf("drop-tools rule: got %v, want [Bash Read]", got)
	}

	// A different model must not be affected by the rule.
	out2 := ApplyPayloadConfigWithRequest(cfg, "some-other-model", "claude", "", "", payload, nil, "", "", nil)
	if len(toolNames(out2)) != 4 {
		t.Fatalf("non-matching model should keep all 4 tools, got %v", toolNames(out2))
	}
}
