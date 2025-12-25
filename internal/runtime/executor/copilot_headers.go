package executor

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	copilotauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/copilot"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// responsesAPIAgentTypes lists input types that indicate agent/tool activity in the
// OpenAI Responses API format. When any of these types appear in the input array,
// the request should be marked as an agent call (X-Initiator: agent).
// See: https://platform.openai.com/docs/api-reference/responses
var responsesAPIAgentTypes = map[string]bool{
	"function_call":           true,
	"function_call_output":    true,
	"computer_call":           true,
	"computer_call_output":    true,
	"web_search_call":         true,
	"file_search_call":        true,
	"code_interpreter_call":   true,
	"local_shell_call":        true,
	"local_shell_call_output": true,
	"mcp_call":                true,
	"mcp_list_tools":          true,
	"mcp_approval_request":    true,
	"mcp_approval_response":   true,
	"image_generation_call":   true,
	"reasoning":               true,
}

// isResponsesAPIAgentItem checks if a single item from the Responses API input array
// indicates agent/tool activity. This is used to determine the X-Initiator header value.
func isResponsesAPIAgentItem(item gjson.Result) bool {
	// Check for assistant role
	if item.Get("role").String() == "assistant" {
		return true
	}
	// Check for agent-related input types
	return responsesAPIAgentTypes[item.Get("type").String()]
}

// isResponsesAPIVisionContent checks if a content part from the Responses API
// contains image data, indicating a vision request.
func isResponsesAPIVisionContent(part gjson.Result) bool {
	return part.Get("type").String() == "input_image"
}

type copilotHeaderHints struct {
	hasVision             bool
	agentFromPayload      bool
	forceAgentFromHeaders bool
	promptCacheKey        string
}

type copilotHeaderProfile string

const (
	copilotHeaderProfileCLI        copilotHeaderProfile = "cli"
	copilotHeaderProfileVSCodeChat copilotHeaderProfile = "vscode-chat"
)

// defaultCopilotCLIHeaderModels lists models that use the CLI header profile by default.
// Models not in this list will use the vscode-chat profile.
var defaultCopilotCLIHeaderModels = map[string]struct{}{
	"claude-sonnet-4.5":    {},
	"claude-haiku-4.5":     {},
	"claude-opus-4.5":      {},
	"claude-sonnet-4":      {},
	"gpt-5.1-codex-max":    {},
	"gpt-5.1-codex":        {},
	"gpt-5.2":              {},
	"gpt-5.1":              {},
	"gpt-5":                {},
	"gpt-5.1-codex-mini":   {},
	"gpt-5-mini":           {},
	"gpt-4o":               {},
	"gpt-4.1":              {},
	"gemini-3-pro-preview": {},
}

func normalizeModelID(model string) string {
	return strings.TrimSpace(strings.ToLower(model))
}

// copilotHeaderProfileForModel determines which header profile to use based on model and config.
// All model comparisons are done against the de-aliased model (copilot- prefix stripped).
func copilotHeaderProfileForModel(entry *config.CopilotKey, model string) copilotHeaderProfile {
	m := normalizeModelID(model)
	if m == "" {
		return copilotHeaderProfileCLI
	}

	// De-alias: treat copilot-<id> as <id> for all comparisons
	mDeAliased := strings.TrimPrefix(m, "copilot-")
	if mDeAliased == "" {
		return copilotHeaderProfileCLI
	}

	// Back-compat: gpt-4 always uses CLI profile
	if mDeAliased == "gpt-4" {
		return copilotHeaderProfileCLI
	}

	if entry != nil {
		// Config per-model overrides (checked against de-aliased model)
		if len(entry.CLIHeaderModels) > 0 {
			for _, v := range entry.CLIHeaderModels {
				if normalizeModelID(v) == mDeAliased {
					return copilotHeaderProfileCLI
				}
			}
		}
		if len(entry.VSCodeChatHeaderModels) > 0 {
			for _, v := range entry.VSCodeChatHeaderModels {
				if normalizeModelID(v) == mDeAliased {
					return copilotHeaderProfileVSCodeChat
				}
			}
		}

		// Config global default profile (overrides allowlist)
		switch copilotHeaderProfile(strings.ToLower(strings.TrimSpace(entry.HeaderProfile))) {
		case copilotHeaderProfileCLI:
			return copilotHeaderProfileCLI
		case copilotHeaderProfileVSCodeChat:
			return copilotHeaderProfileVSCodeChat
		default:
			// Unknown or empty values fall through to allowlist
		}
	}

	// Built-in allowlist (checked against de-aliased model)
	if _, ok := defaultCopilotCLIHeaderModels[mDeAliased]; ok {
		return copilotHeaderProfileCLI
	}
	return copilotHeaderProfileVSCodeChat
}

func applyCopilotVSCodeChatHeaderProfile(r *http.Request) {
	// Matches VS Code Copilot Chat extension behavior
	r.Header.Set("Copilot-Integration-Id", "vscode-chat")
	r.Header.Set("Editor-Plugin-Version", "copilot-chat/0.35.2")
	r.Header.Set("Editor-Version", "vscode/1.108.0-insider")
	r.Header.Set("VScode-SessionId", "00000000-0000-0000-0000-000000000000")
	r.Header.Set("VScode-MachineId", "00000000-0000-0000-0000-000000000000")
	r.Header.Set("OpenAI-Intent", "conversation-agent")
}

func applyCopilotCLIHeaderProfile(r *http.Request) {
	// No-op: defaults are already applied via copilotauth.CopilotHeaders + executor extras.
}

func (e *CopilotExecutor) copilotKeyConfig() *config.CopilotKey {
	if e == nil || e.cfg == nil || len(e.cfg.CopilotKey) == 0 {
		return nil
	}
	return &e.cfg.CopilotKey[0]
}

func (e *CopilotExecutor) applyCopilotHeaderProfile(r *http.Request, model string) {
	entry := e.copilotKeyConfig()
	profile := copilotHeaderProfileForModel(entry, model)
	switch profile {
	case copilotHeaderProfileVSCodeChat:
		applyCopilotVSCodeChatHeaderProfile(r)
	case copilotHeaderProfileCLI:
		applyCopilotCLIHeaderProfile(r)
	default:
		applyCopilotCLIHeaderProfile(r)
	}
}

func forceAgentCallFromHeaders(headers http.Header) bool {
	if headers == nil {
		return false
	}
	raw := strings.TrimSpace(headers.Get("force-copilot-agent"))
	if raw == "" {
		raw = strings.TrimSpace(headers.Get("Force-Copilot-Agent"))
	}
	if raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func promptCacheKeyFromPayload(payload []byte) string {
	if v := gjson.GetBytes(payload, "prompt_cache_key"); v.Exists() {
		if key := strings.TrimSpace(v.String()); key != "" {
			return key
		}
	}
	if v := gjson.GetBytes(payload, "metadata.prompt_cache_key"); v.Exists() {
		if key := strings.TrimSpace(v.String()); key != "" {
			return key
		}
	}
	return ""
}

func collectCopilotHeaderHints(payload []byte, headers http.Header) copilotHeaderHints {
	hints := copilotHeaderHints{
		promptCacheKey:        promptCacheKeyFromPayload(payload),
		forceAgentFromHeaders: forceAgentCallFromHeaders(headers),
	}

	// Chat Completions format (messages array)
	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		for _, msg := range messages.Array() {
			content := msg.Get("content")
			if content.IsArray() {
				for _, part := range content.Array() {
					if part.Get("type").String() == "image_url" {
						hints.hasVision = true
					}
				}
			}
			role := msg.Get("role").String()
			if role == "assistant" || role == "tool" {
				hints.agentFromPayload = true
			}
		}
	}

	// Responses API format (input array)
	input := gjson.GetBytes(payload, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			content := item.Get("content")
			if content.IsArray() {
				for _, part := range content.Array() {
					if isResponsesAPIVisionContent(part) {
						hints.hasVision = true
					}
				}
			}
			if isResponsesAPIAgentItem(item) {
				hints.agentFromPayload = true
			}
		}
	}

	return hints
}

func (e *CopilotExecutor) forceAgentCallEnabled() bool {
	if e == nil || e.cfg == nil {
		return false
	}
	for i := range e.cfg.CopilotKey {
		if e.cfg.CopilotKey[i].ForceAgentCall {
			return true
		}
	}
	return false
}

func (e *CopilotExecutor) agentInitiatorPersistEnabled() bool {
	if e == nil || e.cfg == nil {
		return false
	}
	for i := range e.cfg.CopilotKey {
		if e.cfg.CopilotKey[i].AgentInitiatorPersist {
			return true
		}
	}
	return false
}

func (e *CopilotExecutor) shouldUseAgentInitiator(h copilotHeaderHints) bool {
	if h.forceAgentFromHeaders {
		return true
	}
	if e != nil && e.forceAgentCallEnabled() {
		return true
	}
	if e != nil && e.agentInitiatorPersistEnabled() && h.promptCacheKey != "" {
		e.mu.Lock()
		count := e.initiatorCount[h.promptCacheKey]
		e.initiatorCount[h.promptCacheKey] = count + 1
		e.mu.Unlock()

		if h.agentFromPayload {
			return true
		}
		return count > 0
	}

	return h.agentFromPayload
}

// applyCopilotHeaders applies all necessary headers to the request.
// It handles both Chat Completions format (messages array) and Responses API format (input array).
func (e *CopilotExecutor) applyCopilotHeaders(r *http.Request, copilotToken string, payload []byte, incoming http.Header) {
	hints := collectCopilotHeaderHints(payload, incoming)
	isAgentCall := e.shouldUseAgentInitiator(hints)

	headers := copilotauth.CopilotHeaders(copilotToken, "", hints.hasVision)
	for k, v := range headers {
		r.Header.Set(k, v)
	}

	// Align with Copilot CLI defaults
	r.Header.Set("X-Interaction-Type", "conversation-agent")
	r.Header.Set("Openai-Intent", "conversation-agent")
	r.Header.Set("X-Stainless-Retry-Count", "0")
	r.Header.Set("X-Stainless-Lang", "js")
	r.Header.Set("X-Stainless-Package-Version", "5.20.1")
	r.Header.Set("X-Stainless-OS", "Linux")
	r.Header.Set("X-Stainless-Arch", "arm64")
	r.Header.Set("X-Stainless-Runtime", "node")
	r.Header.Set("X-Stainless-Runtime-Version", "v22.15.0")
	r.Header.Set("User-Agent", copilotauth.CopilotUserAgent)
	if isAgentCall {
		r.Header.Set("X-Initiator", "agent")
		log.Info("copilot executor: [agent call]")
	} else {
		r.Header.Set("X-Initiator", "user")
		log.Info("copilot executor: [user call]")
	}

	// Apply header profile after defaults are set so it can override relevant headers.
	e.applyCopilotHeaderProfile(r, gjson.GetBytes(payload, "model").String())
}
