package executor

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/tidwall/gjson"
)

func TestIsResponsesAPIAgentItem(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		expected bool
	}{
		// User messages - not agent
		{
			name:     "user message",
			json:     `{"role": "user", "content": [{"type": "input_text", "text": "hello"}]}`,
			expected: false,
		},
		{
			name:     "system message",
			json:     `{"role": "system", "content": "You are helpful"}`,
			expected: false,
		},
		// Assistant/tool messages - agent
		{
			name:     "assistant message",
			json:     `{"role": "assistant", "content": [{"type": "output_text", "text": "hi"}]}`,
			expected: true,
		},
		{
			name:     "tool role message",
			json:     `{"role": "tool", "content": [{"type": "output_text", "text": "tool output"}]}`,
			expected: true,
		},
		// Function/tool types - agent
		{
			name:     "function_call",
			json:     `{"type": "function_call", "call_id": "123", "name": "test"}`,
			expected: true,
		},
		{
			name:     "function_call_output",
			json:     `{"type": "function_call_output", "call_id": "123", "output": "done"}`,
			expected: true,
		},
		// Computer use types - agent
		{
			name:     "computer_call",
			json:     `{"type": "computer_call", "call_id": "123"}`,
			expected: true,
		},
		{
			name:     "computer_call_output",
			json:     `{"type": "computer_call_output", "call_id": "123"}`,
			expected: true,
		},
		// Search types - agent
		{
			name:     "web_search_call",
			json:     `{"type": "web_search_call", "id": "123"}`,
			expected: true,
		},
		{
			name:     "file_search_call",
			json:     `{"type": "file_search_call", "id": "123"}`,
			expected: true,
		},
		// Code interpreter - agent
		{
			name:     "code_interpreter_call",
			json:     `{"type": "code_interpreter_call", "id": "123"}`,
			expected: true,
		},
		// Local shell types - agent
		{
			name:     "local_shell_call",
			json:     `{"type": "local_shell_call", "call_id": "123"}`,
			expected: true,
		},
		{
			name:     "local_shell_call_output",
			json:     `{"type": "local_shell_call_output", "call_id": "123"}`,
			expected: true,
		},
		// MCP types - agent
		{
			name:     "mcp_call",
			json:     `{"type": "mcp_call", "id": "123"}`,
			expected: true,
		},
		{
			name:     "mcp_list_tools",
			json:     `{"type": "mcp_list_tools"}`,
			expected: true,
		},
		{
			name:     "mcp_approval_request",
			json:     `{"type": "mcp_approval_request"}`,
			expected: true,
		},
		{
			name:     "mcp_approval_response",
			json:     `{"type": "mcp_approval_response"}`,
			expected: true,
		},
		// Other agent types
		{
			name:     "image_generation_call",
			json:     `{"type": "image_generation_call", "id": "123"}`,
			expected: true,
		},
		{
			name:     "reasoning",
			json:     `{"type": "reasoning", "content": "thinking..."}`,
			expected: true,
		},
		// Unknown types - not agent
		{
			name:     "unknown type",
			json:     `{"type": "unknown_future_type"}`,
			expected: false,
		},
		{
			name:     "message type with user role",
			json:     `{"type": "message", "role": "user"}`,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := gjson.Parse(tt.json)
			got := isResponsesAPIAgentItem(item)
			if got != tt.expected {
				t.Errorf("isResponsesAPIAgentItem(%s) = %v, want %v", tt.json, got, tt.expected)
			}
		})
	}
}

func TestIsResponsesAPIVisionContent(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		expected bool
	}{
		{
			name:     "input_image type",
			json:     `{"type": "input_image", "image_url": {"url": "data:image/png;base64,..."}}`,
			expected: true,
		},
		{
			name:     "input_text type",
			json:     `{"type": "input_text", "text": "hello"}`,
			expected: false,
		},
		{
			name:     "output_text type",
			json:     `{"type": "output_text", "text": "hi"}`,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			part := gjson.Parse(tt.json)
			got := isResponsesAPIVisionContent(part)
			if got != tt.expected {
				t.Errorf("isResponsesAPIVisionContent(%s) = %v, want %v", tt.json, got, tt.expected)
			}
		})
	}
}

func TestApplyCopilotHeaders_XInitiator(t *testing.T) {
	tests := []struct {
		name              string
		payload           string
		expectedInitiator string
	}{
		// Chat Completions format tests
		{
			name:              "chat completions - user only",
			payload:           `{"messages":[{"role":"user","content":"hello"}]}`,
			expectedInitiator: "user",
		},
		{
			name:              "chat completions - with assistant",
			payload:           `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "chat completions - user then user (history + new)",
			payload:           `{"messages":[{"role":"user","content":"old"},{"role":"user","content":"new"}]}`,
			expectedInitiator: "user",
		},
		{
			name:              "chat completions - with tool",
			payload:           `{"messages":[{"role":"user","content":"hello"},{"role":"tool","tool_call_id":"123","content":"result"}]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "chat completions - system and user only",
			payload:           `{"messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hello"}]}`,
			expectedInitiator: "agent",
		},
		// Responses API format tests
		{
			name:              "responses - user only",
			payload:           `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			expectedInitiator: "user",
		},
		{
			name:              "responses - with function_call",
			payload:           `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"function_call","call_id":"123","name":"test","arguments":"{}"}]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "responses - user then user (history + new)",
			payload:           `{"input":[{"role":"user","content":[{"type":"input_text","text":"old"}]},{"role":"user","content":[{"type":"input_text","text":"new"}]}]}`,
			expectedInitiator: "user",
		},
		{
			name:              "responses - with function_call_output",
			payload:           `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"function_call_output","call_id":"123","output":"done"}]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "responses - with assistant role",
			payload:           `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "responses - with reasoning",
			payload:           `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"reasoning","content":"thinking..."}]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "responses - with local_shell_call",
			payload:           `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"local_shell_call","call_id":"123","action":{"command":["ls"]}}]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "responses - with mcp_call",
			payload:           `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"mcp_call","id":"123"}]}`,
			expectedInitiator: "agent",
		},
		// Edge cases
		{
			name:              "empty messages",
			payload:           `{"messages":[]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "empty input",
			payload:           `{"input":[]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "no messages or input",
			payload:           `{"model":"gpt-4"}`,
			expectedInitiator: "agent",
		},
		// Mixed format tests - both messages[] and input[] present
		{
			name:              "mixed format - user messages with agent input",
			payload:           `{"messages":[{"role":"user","content":"hello"}],"input":[{"type":"function_call","call_id":"123","name":"test","arguments":"{}"}]}`,
			expectedInitiator: "agent",
		},
		{
			name:              "mixed format - agent messages with user input",
			payload:           `{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"}],"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			expectedInitiator: "agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewCopilotExecutor(&config.Config{})
			req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
			e.applyCopilotHeaders(req, "test-token", []byte(tt.payload), nil)

			got := req.Header.Get("X-Initiator")
			if got != tt.expectedInitiator {
				t.Errorf("X-Initiator = %q, want %q", got, tt.expectedInitiator)
			}
		})
	}
}

func TestApplyCopilotHeaders_XInitiator_ForcedByIncomingHeader(t *testing.T) {
	e := NewCopilotExecutor(&config.Config{})
	req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
	incoming := http.Header{}
	incoming.Set("force-copilot-agent", "true")

	payload := `{"messages":[{"role":"user","content":"hello"}]}`
	e.applyCopilotHeaders(req, "test-token", []byte(payload), incoming)

	if got := req.Header.Get("X-Initiator"); got != "agent" {
		t.Fatalf("X-Initiator = %q, want agent", got)
	}
}

func TestApplyCopilotHeaders_XInitiator_PersistAcrossCalls(t *testing.T) {
	payload := `{"prompt_cache_key":"thread-1","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`

	t.Run("disabled flag keeps user initiator", func(t *testing.T) {
		e := NewCopilotExecutor(&config.Config{})
		req1 := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
		e.applyCopilotHeaders(req1, "test-token", []byte(payload), nil)

		if got := req1.Header.Get("X-Initiator"); got != "user" {
			t.Fatalf("first call initiator = %q, want user", got)
		}

		req2 := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
		e.applyCopilotHeaders(req2, "test-token", []byte(payload), nil)

		if got := req2.Header.Get("X-Initiator"); got != "user" {
			t.Fatalf("second call initiator = %q, want user when flag disabled", got)
		}
	})

	t.Run("enabled flag promotes to agent after first", func(t *testing.T) {
		e := NewCopilotExecutor(&config.Config{CopilotKey: []config.CopilotKey{{AgentInitiatorPersist: true}}})
		req1 := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
		e.applyCopilotHeaders(req1, "test-token", []byte(payload), nil)

		if got := req1.Header.Get("X-Initiator"); got != "user" {
			t.Fatalf("first call initiator = %q, want user", got)
		}

		req2 := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
		e.applyCopilotHeaders(req2, "test-token", []byte(payload), nil)

		if got := req2.Header.Get("X-Initiator"); got != "agent" {
			t.Fatalf("second call initiator = %q, want agent when flag enabled", got)
		}
	})
}

func TestApplyCopilotHeaders_Vision(t *testing.T) {
	tests := []struct {
		name           string
		payload        string
		expectedVision bool
	}{
		// Chat Completions format
		{
			name:           "chat completions - no vision",
			payload:        `{"messages":[{"role":"user","content":"hello"}]}`,
			expectedVision: false,
		},
		{
			name:           "chat completions - with image_url",
			payload:        `{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}]}]}`,
			expectedVision: true,
		},
		// Responses API format
		{
			name:           "responses - no vision",
			payload:        `{"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			expectedVision: false,
		},
		{
			name:           "responses - with input_image",
			payload:        `{"input":[{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":{"url":"data:image/png;base64,..."}}]}]}`,
			expectedVision: true,
		},
		// Mixed format tests - both messages[] and input[] present
		{
			name:           "mixed format - vision in messages only",
			payload:        `{"messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}]}],"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}]}`,
			expectedVision: true,
		},
		{
			name:           "mixed format - vision in input only",
			payload:        `{"messages":[{"role":"user","content":"hello"}],"input":[{"role":"user","content":[{"type":"input_text","text":"describe"},{"type":"input_image","image_url":{"url":"data:image/png;base64,..."}}]}]}`,
			expectedVision: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewCopilotExecutor(&config.Config{})
			req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
			e.applyCopilotHeaders(req, "test-token", []byte(tt.payload), nil)

			got := req.Header.Get("Copilot-Vision-Request")
			hasVision := got == "true"
			if hasVision != tt.expectedVision {
				t.Errorf("Copilot-Vision-Request = %q (hasVision=%v), want hasVision=%v", got, hasVision, tt.expectedVision)
			}
		})
	}
}

func TestCopilotHeaderProfileForModel(t *testing.T) {
	tests := []struct {
		name            string
		model           string
		copilotConfig   *config.CopilotKey
		expectedProfile copilotHeaderProfile
	}{
		// Empty model defaults to CLI
		{
			name:            "empty model defaults to cli",
			model:           "",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		// gpt-4 family defaults to CLI
		{
			name:            "gpt-4 defaults to cli",
			model:           "gpt-4",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:            "gpt-4o defaults to cli",
			model:           "gpt-4o",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:            "gpt-4.1 defaults to cli",
			model:           "gpt-4.1",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		// Allowlisted models default to CLI
		{
			name:            "gpt-5 in allowlist defaults to cli",
			model:           "gpt-5",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:            "claude-sonnet-4 in allowlist defaults to cli",
			model:           "claude-sonnet-4",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:            "claude-opus-4.5 in allowlist defaults to cli",
			model:           "claude-opus-4.5",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		// Models not in allowlist default to vscode-chat
		{
			name:            "gemini-2.5-pro not in allowlist defaults to vscode-chat",
			model:           "gemini-2.5-pro",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileVSCodeChat,
		},
		{
			name:            "unknown model defaults to vscode-chat",
			model:           "some-unknown-model",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileVSCodeChat,
		},
		// De-aliasing: copilot- prefix should be stripped for matching
		{
			name:            "copilot-gpt-5 de-aliased to gpt-5 matches allowlist",
			model:           "copilot-gpt-5",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:            "copilot-claude-sonnet-4 de-aliased matches allowlist",
			model:           "copilot-claude-sonnet-4",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:            "copilot-gemini-2.5-pro de-aliased still not in allowlist",
			model:           "copilot-gemini-2.5-pro",
			copilotConfig:   nil,
			expectedProfile: copilotHeaderProfileVSCodeChat,
		},
		// Config override: CLIHeaderModels
		{
			name:  "config CLIHeaderModels override",
			model: "gemini-2.5-pro",
			copilotConfig: &config.CopilotKey{
				CLIHeaderModels: []string{"gemini-2.5-pro"},
			},
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:  "config CLIHeaderModels override with copilot prefix",
			model: "copilot-gemini-2.5-pro",
			copilotConfig: &config.CopilotKey{
				CLIHeaderModels: []string{"gemini-2.5-pro"},
			},
			expectedProfile: copilotHeaderProfileCLI,
		},
		// Config override: VSCodeChatHeaderModels
		{
			name:  "config VSCodeChatHeaderModels override",
			model: "gpt-5",
			copilotConfig: &config.CopilotKey{
				VSCodeChatHeaderModels: []string{"gpt-5"},
			},
			expectedProfile: copilotHeaderProfileVSCodeChat,
		},
		{
			name:  "config VSCodeChatHeaderModels override with copilot prefix",
			model: "copilot-gpt-5",
			copilotConfig: &config.CopilotKey{
				VSCodeChatHeaderModels: []string{"gpt-5"},
			},
			expectedProfile: copilotHeaderProfileVSCodeChat,
		},
		// Config default HeaderProfile
		{
			name:  "config HeaderProfile cli for unknown model",
			model: "some-unknown-model",
			copilotConfig: &config.CopilotKey{
				HeaderProfile: "cli",
			},
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:  "config HeaderProfile vscode-chat overrides allowlist",
			model: "gpt-5",
			copilotConfig: &config.CopilotKey{
				HeaderProfile: "vscode-chat",
			},
			expectedProfile: copilotHeaderProfileVSCodeChat,
		},
		// Model-specific config takes precedence over HeaderProfile default
		{
			name:  "CLIHeaderModels takes precedence over HeaderProfile vscode-chat",
			model: "gemini-3-pro",
			copilotConfig: &config.CopilotKey{
				HeaderProfile:   "vscode-chat",
				CLIHeaderModels: []string{"gemini-3-pro"},
			},
			expectedProfile: copilotHeaderProfileCLI,
		},
		{
			name:  "VSCodeChatHeaderModels takes precedence over HeaderProfile cli",
			model: "gpt-5",
			copilotConfig: &config.CopilotKey{
				HeaderProfile:          "cli",
				VSCodeChatHeaderModels: []string{"gpt-5"},
			},
			expectedProfile: copilotHeaderProfileVSCodeChat,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := copilotHeaderProfileForModel(tt.copilotConfig, tt.model)
			if got != tt.expectedProfile {
				t.Errorf("copilotHeaderProfileForModel(%q) = %v, want %v", tt.model, got, tt.expectedProfile)
			}
		})
	}
}

func TestApplyCopilotHeaderProfile(t *testing.T) {
	tests := []struct {
		name                 string
		model                string
		copilotConfig        []config.CopilotKey
		expectedIntegration  string
		expectedEditorPlugin string
	}{
		{
			name:                 "cli profile is no-op (headers not overridden)",
			model:                "gpt-5",
			copilotConfig:        nil,
			expectedIntegration:  "", // CLI profile does not set this header
			expectedEditorPlugin: "", // CLI profile does not set this header
		},
		{
			name:                 "vscode-chat profile sets VS Code headers",
			model:                "gemini-2.5-pro",
			copilotConfig:        nil,
			expectedIntegration:  "vscode-chat",
			expectedEditorPlugin: "copilot-chat/0.35.2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := NewCopilotExecutor(&config.Config{CopilotKey: tt.copilotConfig})
			req := httptest.NewRequest(http.MethodPost, "/chat/completions", nil)
			e.applyCopilotHeaderProfile(req, tt.model)

			if got := req.Header.Get("Copilot-Integration-Id"); got != tt.expectedIntegration {
				t.Errorf("Copilot-Integration-Id = %q, want %q", got, tt.expectedIntegration)
			}
			if got := req.Header.Get("Editor-Plugin-Version"); got != tt.expectedEditorPlugin {
				t.Errorf("Editor-Plugin-Version = %q, want %q", got, tt.expectedEditorPlugin)
			}
		})
	}
}
