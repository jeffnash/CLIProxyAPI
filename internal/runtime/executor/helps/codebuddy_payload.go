package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

// ResolveCodeBuddyModel resolves the requested model against the selected
// auth's own catalog snapshot. Resolved registry info is honored first, with
// membership enforced against the snapshot; direct calls without resolved
// info match the exact public ID. Arbitrary prefixes are never stripped.
func ResolveCodeBuddyModel(auth *cliproxyauth.Auth, req cliproxyexecutor.Request) (codebuddy.Model, error) {
	if auth == nil {
		return codebuddy.Model{}, requestScopedCodeBuddyError("missing auth for codebuddy request")
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), codebuddy.Provider) {
		return codebuddy.Model{}, requestScopedCodeBuddyError("auth provider is not codebuddy")
	}
	if auth.Disabled {
		return codebuddy.Model{}, requestScopedCodeBuddyError("codebuddy auth is disabled")
	}
	credentials, err := codebuddy.CredentialsFromMetadata(auth.Metadata)
	if err != nil {
		return codebuddy.Model{}, &CodeBuddyError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "invalid codebuddy credential: " + err.Error(), CredentialScoped: true}
	}
	catalog, err := codebuddy.CatalogFromMetadata(auth.Metadata)
	if err != nil {
		return codebuddy.Model{}, requestScopedCodeBuddyError("invalid codebuddy catalog snapshot: " + err.Error())
	}
	base := thinking.ParseSuffix(req.Model).ModelName
	if modelInfo, ok := cliproxyauth.ResolvedModelInfo(req); ok && modelInfo != nil && strings.TrimSpace(modelInfo.UpstreamID) != "" {
		upstream := strings.TrimSpace(modelInfo.UpstreamID)
		for _, model := range catalog.Models {
			if model.ID != upstream {
				continue
			}
			if strings.HasPrefix(base, codebuddy.Provider+"-") && base != codebuddy.PublicModelID(credentials.Realm, model.ID) {
				return codebuddy.Model{}, modelNotFoundCodeBuddyError(base)
			}
			return model, nil
		}
		return codebuddy.Model{}, modelNotFoundCodeBuddyError(base)
	}
	for _, model := range catalog.Models {
		if codebuddy.PublicModelID(credentials.Realm, model.ID) == base {
			return model, nil
		}
	}
	return codebuddy.Model{}, modelNotFoundCodeBuddyError(base)
}

// requestScopedCodeBuddyError builds a 400 request fault.
func requestScopedCodeBuddyError(message string) *CodeBuddyError {
	return &CodeBuddyError{Status: http.StatusBadRequest, Code: "bad_request", Message: message, RequestScoped: true}
}

// modelNotFoundCodeBuddyError builds a model-scoped 404 referencing the exact
// requested model.
func modelNotFoundCodeBuddyError(requested string) *CodeBuddyError {
	return &CodeBuddyError{Status: http.StatusNotFound, Code: "model_not_found", Message: fmt.Sprintf("model %q is not available on the selected codebuddy account", requested)}
}

// PrepareCodeBuddyPayload normalizes already-translated OpenAI JSON plus
// already-normalized canonical thinking into the exact upstream body. It runs
// a single JSON decode/edit/encode pass, never mutates the input slice, and
// rejects invalid or unsupported requests instead of weakening them.
func PrepareCodeBuddyPayload(body []byte, model codebuddy.Model, realm codebuddy.Realm) ([]byte, error) {
	if strings.TrimSpace(model.ID) == "" {
		return nil, requestScopedCodeBuddyError("missing selected codebuddy model")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, requestScopedCodeBuddyError("invalid request JSON: " + err.Error())
	}
	if err := ensureNoTrailingPayload(decoder); err != nil {
		return nil, requestScopedCodeBuddyError(err.Error())
	}
	if payload == nil {
		return nil, requestScopedCodeBuddyError("invalid request JSON")
	}
	if err := normalizeCodeBuddyMaxTokens(payload); err != nil {
		return nil, err
	}
	messages, err := normalizeCodeBuddyRoles(payload)
	if err != nil {
		return nil, err
	}
	if err := validateCodeBuddyMessageContent(messages); err != nil {
		return nil, err
	}
	if realm != codebuddy.RealmCN && !hasInitialSystemMessage(messages) {
		messages = append([]any{map[string]any{"role": "system", "content": "You are a helpful assistant."}}, messages...)
		payload["messages"] = messages
	}
	tools, hasTools := codeBuddyTools(payload)
	if err := normalizeCodeBuddyToolChoice(payload, tools, hasTools, model); err != nil {
		return nil, err
	}
	if err := validateCodeBuddyToolDefinitions(payload); err != nil {
		return nil, err
	}
	if err := validateCodeBuddyToolHistory(messages); err != nil {
		return nil, err
	}
	if err := validateCodeBuddyCardinality(payload); err != nil {
		return nil, err
	}
	if err := validateCodeBuddyResponseFormat(payload); err != nil {
		return nil, err
	}
	if err := validateCodeBuddyReasoning(payload, model); err != nil {
		return nil, err
	}
	payload["model"] = model.ID
	payload["stream"] = true
	streamOptions, _ := payload["stream_options"].(map[string]any)
	if streamOptions == nil {
		streamOptions = map[string]any{}
	}
	streamOptions["include_usage"] = true
	payload["stream_options"] = streamOptions
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, requestScopedCodeBuddyError("encode codebuddy payload: " + err.Error())
	}
	return encoded, nil
}

// ensureNoTrailingPayload rejects trailing content after the request object.
func ensureNoTrailingPayload(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("invalid request JSON: trailing content")
		}
		return fmt.Errorf("invalid request JSON: %w", err)
	}
	return nil
}

// normalizeCodeBuddyMaxTokens moves max_completion_tokens to max_tokens and
// validates positive integral values. Existing max_tokens wins when both are
// present; the alias is always removed.
func normalizeCodeBuddyMaxTokens(payload map[string]any) error {
	rawMax, hasMax := payload["max_tokens"]
	rawAlias, hasAlias := payload["max_completion_tokens"]
	if hasMax && rawMax != nil {
		value, err := codeBuddyPositiveInt(rawMax)
		if err != nil || value <= 0 {
			return requestScopedCodeBuddyError("invalid max_tokens value")
		}
		payload["max_tokens"] = value
	} else if hasAlias && rawAlias != nil {
		value, err := codeBuddyPositiveInt(rawAlias)
		if err != nil || value <= 0 {
			return requestScopedCodeBuddyError("invalid max_completion_tokens value")
		}
		payload["max_tokens"] = value
	}
	delete(payload, "max_completion_tokens")
	return nil
}

// codeBuddyPositiveInt parses a positive integral JSON number.
func codeBuddyPositiveInt(raw any) (int64, error) {
	number, ok := raw.(json.Number)
	if !ok {
		return 0, fmt.Errorf("not a number")
	}
	text := strings.TrimSpace(number.String())
	if text == "" {
		return 0, fmt.Errorf("empty number")
	}
	var value int64
	for i := 0; i < len(text); i++ {
		digit := text[i]
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("not an integer")
		}
		value = value*10 + int64(digit-'0')
		if value < 0 {
			return 0, fmt.Errorf("overflow")
		}
	}
	return value, nil
}

// normalizeCodeBuddyRoles maps developer roles to system and rejects unknown
// role values. It returns the message list.
func normalizeCodeBuddyRoles(payload map[string]any) ([]any, error) {
	raw, ok := payload["messages"]
	if !ok || raw == nil {
		return nil, requestScopedCodeBuddyError("request has no messages")
	}
	messages, ok := raw.([]any)
	if !ok {
		return nil, requestScopedCodeBuddyError("request messages must be an array")
	}
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			return nil, requestScopedCodeBuddyError("request message must be an object")
		}
		role, ok := message["role"].(string)
		if !ok {
			return nil, requestScopedCodeBuddyError("request message has no role")
		}
		switch role {
		case "developer":
			message["role"] = "system"
		case "system", "user", "assistant", "tool":
		default:
			return nil, requestScopedCodeBuddyError(fmt.Sprintf("unsupported message role %q", role))
		}
	}
	return messages, nil
}

// validateCodeBuddyMessageContent accepts text content and text-only content
// parts, rejecting multimodal parts explicitly. Null content (for example on
// assistant tool-call messages) is accepted.
func validateCodeBuddyMessageContent(messages []any) error {
	for _, item := range messages {
		message, _ := item.(map[string]any)
		if message == nil {
			continue
		}
		content, ok := message["content"]
		if !ok || content == nil {
			continue
		}
		switch typed := content.(type) {
		case string:
		case []any:
			for _, part := range typed {
				partObj, ok := part.(map[string]any)
				if !ok {
					return requestScopedCodeBuddyError("message content part must be an object")
				}
				partType, _ := partObj["type"].(string)
				if partType != "text" {
					return requestScopedCodeBuddyError(fmt.Sprintf("unsupported message content part %q; this integration is text-only", partType))
				}
			}
		default:
			return requestScopedCodeBuddyError("message content must be text")
		}
	}
	return nil
}

// hasInitialSystemMessage reports whether the first message is a system prompt.
func hasInitialSystemMessage(messages []any) bool {
	if len(messages) == 0 {
		return false
	}
	first, _ := messages[0].(map[string]any)
	if first == nil {
		return false
	}
	role, _ := first["role"].(string)
	return role == "system"
}

// codeBuddyTools extracts the tool definitions.
func codeBuddyTools(payload map[string]any) ([]any, bool) {
	raw, ok := payload["tools"]
	if !ok || raw == nil {
		return nil, false
	}
	tools, ok := raw.([]any)
	if !ok {
		return nil, false
	}
	return tools, len(tools) > 0
}

// normalizeCodeBuddyToolChoice applies tool-choice semantics. tool_choice
// "none" strips definitions for this request; named function choices are
// validated and converted to the exact function-name string upstream
// requires. Tools on an unsupported or unverified capability are rejected.
func normalizeCodeBuddyToolChoice(payload map[string]any, tools []any, hasTools bool, model codebuddy.Model) error {
	rawChoice, hasChoice := payload["tool_choice"]
	choiceName := func() (string, bool) {
		text, ok := rawChoice.(string)
		return text, hasChoice && ok
	}
	if text, ok := choiceName(); ok {
		switch text {
		case "none":
			delete(payload, "tools")
			delete(payload, "tool_choice")
			return nil
		case "auto", "required":
			if !hasTools {
				if text == "required" {
					return requestScopedCodeBuddyError("tool_choice \"required\" without tools")
				}
				delete(payload, "tool_choice")
				return nil
			}
			if err := requireCodeBuddyToolSupport(model); err != nil {
				return err
			}
			return nil
		default:
			if !hasTools {
				return requestScopedCodeBuddyError(fmt.Sprintf("invalid tool_choice %q", text))
			}
			if err := requireCodeBuddyToolSupport(model); err != nil {
				return err
			}
			// A string naming a supplied tool is preserved for idempotence.
			for _, item := range tools {
				tool, _ := item.(map[string]any)
				if tool == nil {
					continue
				}
				function, _ := tool["function"].(map[string]any)
				if function == nil {
					continue
				}
				if name, _ := function["name"].(string); name == text && name != "" {
					return nil
				}
			}
			return requestScopedCodeBuddyError(fmt.Sprintf("invalid tool_choice %q", text))
		}
	}
	if hasChoice && rawChoice != nil {
		choice, ok := rawChoice.(map[string]any)
		if !ok {
			return requestScopedCodeBuddyError("invalid tool_choice value")
		}
		if !hasTools {
			return requestScopedCodeBuddyError("tool_choice without tools")
		}
		if err := requireCodeBuddyToolSupport(model); err != nil {
			return err
		}
		choiceType, _ := choice["type"].(string)
		if choiceType != "function" {
			return requestScopedCodeBuddyError("unsupported tool_choice type; only function choices are supported")
		}
		function, _ := choice["function"].(map[string]any)
		name, _ := function["name"].(string)
		if function == nil || strings.TrimSpace(name) == "" {
			return requestScopedCodeBuddyError("tool_choice function has no name")
		}
		for _, item := range tools {
			tool, _ := item.(map[string]any)
			if tool == nil {
				continue
			}
			definition, _ := tool["function"].(map[string]any)
			if definition == nil {
				continue
			}
			if candidate, _ := definition["name"].(string); candidate == name && candidate != "" {
				payload["tool_choice"] = name
				return nil
			}
		}
		return requestScopedCodeBuddyError(fmt.Sprintf("tool_choice function %q is not defined", name))
	}
	if hasTools {
		return requireCodeBuddyToolSupport(model)
	}
	return nil
}

// requireCodeBuddyToolSupport rejects tools on unsupported or unverified
// capabilities with distinct messages.
func requireCodeBuddyToolSupport(model codebuddy.Model) error {
	if model.SupportsTools == nil {
		return requestScopedCodeBuddyError(fmt.Sprintf("model %q has no verified tool capability; refusing tools", model.ID))
	}
	if !*model.SupportsTools {
		return requestScopedCodeBuddyError(fmt.Sprintf("model %q does not support tools", model.ID))
	}
	return nil
}

// validateCodeBuddyToolDefinitions requires translated tools to be named
// function tools. Any other shape is unverified against Tencent and must
// fail instead of reaching upstream in a guessed form.
func validateCodeBuddyToolDefinitions(payload map[string]any) error {
	raw, ok := payload["tools"]
	if !ok || raw == nil {
		return nil
	}
	tools, ok := raw.([]any)
	if !ok {
		return requestScopedCodeBuddyError("tools must be an array")
	}
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		if tool == nil {
			return requestScopedCodeBuddyError("tool definition must be an object")
		}
		toolType, _ := tool["type"].(string)
		if toolType != "function" {
			return requestScopedCodeBuddyError(fmt.Sprintf("unsupported tool type %q; only function tools are supported", toolType))
		}
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		if function == nil || strings.TrimSpace(name) == "" {
			return requestScopedCodeBuddyError("function tool has no name")
		}
	}
	return nil
}

// validateCodeBuddyToolHistory enforces complete tool linkage per assistant
// tool-call group: unique call IDs, every call resolved by exactly one later
// tool result, and each group's results in one contiguous block immediately
// following its calls. Multiple complete groups (sequential tool rounds) are
// accepted; results may arrive in any order within their group's block.
func validateCodeBuddyToolHistory(messages []any) error {
	callIndex := map[string]int{}
	callsAt := map[int][]string{}
	for index, item := range messages {
		message, _ := item.(map[string]any)
		if message == nil {
			continue
		}
		if role, _ := message["role"].(string); role != "assistant" {
			continue
		}
		rawCalls, ok := message["tool_calls"]
		if !ok || rawCalls == nil {
			continue
		}
		calls, ok := rawCalls.([]any)
		if !ok {
			return requestScopedCodeBuddyError("assistant tool_calls must be an array")
		}
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				return requestScopedCodeBuddyError("assistant tool call must be an object")
			}
			id, _ := call["id"].(string)
			if strings.TrimSpace(id) == "" {
				return requestScopedCodeBuddyError("assistant tool call has no id")
			}
			if _, dup := callIndex[id]; dup {
				return requestScopedCodeBuddyError(fmt.Sprintf("duplicate tool call id %q", id))
			}
			callIndex[id] = index
			callsAt[index] = append(callsAt[index], id)
		}
	}
	open := map[string]bool{}
	openOrder := []string{}
	resolved := map[string]bool{}
	for index, item := range messages {
		message, _ := item.(map[string]any)
		if message == nil {
			continue
		}
		if role, _ := message["role"].(string); role == "tool" {
			id, _ := message["tool_call_id"].(string)
			if strings.TrimSpace(id) == "" {
				return requestScopedCodeBuddyError("tool result has no tool_call_id")
			}
			if resolved[id] {
				return requestScopedCodeBuddyError(fmt.Sprintf("duplicate tool result id %q", id))
			}
			if !open[id] {
				if callAt, ok := callIndex[id]; ok && callAt > index {
					return requestScopedCodeBuddyError(fmt.Sprintf("tool result id %q precedes its call", id))
				}
				return requestScopedCodeBuddyError(fmt.Sprintf("orphan tool result id %q", id))
			}
			delete(open, id)
			resolved[id] = true
			continue
		}
		if len(open) != 0 {
			return requestScopedCodeBuddyError(fmt.Sprintf("tool call id %q has no matching tool result before the next message", firstOpenCodeBuddyCall(open, openOrder)))
		}
		for _, id := range callsAt[index] {
			if !open[id] {
				open[id] = true
				openOrder = append(openOrder, id)
			}
		}
	}
	if len(open) != 0 {
		return requestScopedCodeBuddyError(fmt.Sprintf("tool call id %q has no matching tool result", firstOpenCodeBuddyCall(open, openOrder)))
	}
	return nil
}

// firstOpenCodeBuddyCall names the earliest still-unresolved call for stable
// error messages; openOrder may retain already-resolved IDs.
func firstOpenCodeBuddyCall(open map[string]bool, openOrder []string) string {
	for _, id := range openOrder {
		if open[id] {
			return id
		}
	}
	for id := range open {
		return id
	}
	return ""
}

// validateCodeBuddyCardinality accepts only n == 1 when n is present.
func validateCodeBuddyCardinality(payload map[string]any) error {
	raw, ok := payload["n"]
	if !ok || raw == nil {
		return nil
	}
	value, err := codeBuddyPositiveInt(raw)
	if err != nil || value != 1 {
		return requestScopedCodeBuddyError("only n=1 is supported")
	}
	return nil
}

// validateCodeBuddyResponseFormat accepts absent or text formats and rejects
// unverified structured output explicitly.
func validateCodeBuddyResponseFormat(payload map[string]any) error {
	raw, ok := payload["response_format"]
	if !ok || raw == nil {
		return nil
	}
	format, ok := raw.(map[string]any)
	if !ok {
		return requestScopedCodeBuddyError("invalid response_format value")
	}
	formatType, _ := format["type"].(string)
	if formatType == "text" {
		return nil
	}
	return requestScopedCodeBuddyError(fmt.Sprintf("unsupported response_format %q; structured output is not verified", formatType))
}

// validateCodeBuddySourceTools rejects source-format tool declarations that
// translation would silently drop or misrepresent. Responses drops server
// tools when converting to Chat Completions, and the Anthropic converter
// rewrites every declaration as a function tool without checking its type,
// so both must be validated here, before lossy translation. Server tools
// have no CodeBuddy equivalent and fail instead of running a weaker request.
func validateCodeBuddySourceTools(source []byte, from sdktranslator.Format) error {
	switch from {
	case sdktranslator.FormatOpenAIResponse:
		return validateCodeBuddyResponsesSourceTools(source)
	case sdktranslator.FormatClaude:
		return validateCodeBuddyClaudeSourceTools(source)
	default:
		return nil
	}
}

// validateCodeBuddyResponsesSourceTools mirrors the request translator's
// declaration walk: function and custom tools (including namespace children
// and additional_tools items) convert to Chat Completions; every other type
// is dropped and therefore rejected here.
func validateCodeBuddyResponsesSourceTools(source []byte) error {
	if tools := gjson.GetBytes(source, "tools"); tools.Exists() {
		if !tools.IsArray() {
			return requestScopedCodeBuddyError("responses tools must be an array")
		}
		for _, tool := range tools.Array() {
			if err := validateCodeBuddyResponsesSourceTool(tool); err != nil {
				return err
			}
		}
	}
	if input := gjson.GetBytes(source, "input"); input.IsArray() {
		for _, item := range input.Array() {
			if !item.IsObject() || item.Get("type").String() != "additional_tools" {
				continue
			}
			tools := item.Get("tools")
			if !tools.Exists() {
				continue
			}
			if !tools.IsArray() {
				return requestScopedCodeBuddyError("responses additional_tools must be an array")
			}
			for _, tool := range tools.Array() {
				if err := validateCodeBuddyResponsesSourceTool(tool); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateCodeBuddyResponsesSourceTool(tool gjson.Result) error {
	if !tool.IsObject() {
		return requestScopedCodeBuddyError("responses tool definition must be an object")
	}
	switch toolType := strings.TrimSpace(tool.Get("type").String()); toolType {
	case "", "function", "custom":
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(tool.Get("function.name").String())
		}
		if name == "" {
			return requestScopedCodeBuddyError("responses function tool has no name")
		}
		return nil
	case "namespace":
		children := tool.Get("tools")
		if !children.IsArray() || len(children.Array()) == 0 {
			return requestScopedCodeBuddyError("responses namespace tool has no tools")
		}
		for _, child := range children.Array() {
			if err := validateCodeBuddyResponsesSourceTool(child); err != nil {
				return err
			}
		}
		return nil
	default:
		return requestScopedCodeBuddyError(fmt.Sprintf("unsupported responses tool type %q; only function tools are supported", toolType))
	}
}

// validateCodeBuddyClaudeSourceTools rejects Anthropic server tools, which
// the Chat Completions converter would otherwise rewrite into bogus function
// tools. Custom tools carry no type.
func validateCodeBuddyClaudeSourceTools(source []byte) error {
	tools := gjson.GetBytes(source, "tools")
	if !tools.Exists() {
		return nil
	}
	if !tools.IsArray() {
		return requestScopedCodeBuddyError("anthropic tools must be an array")
	}
	for _, tool := range tools.Array() {
		if !tool.IsObject() {
			return requestScopedCodeBuddyError("anthropic tool definition must be an object")
		}
		switch toolType := strings.TrimSpace(tool.Get("type").String()); toolType {
		case "", "custom":
		default:
			return requestScopedCodeBuddyError(fmt.Sprintf("unsupported anthropic tool type %q; only custom function tools are supported", toolType))
		}
		if strings.TrimSpace(tool.Get("name").String()) == "" {
			return requestScopedCodeBuddyError("anthropic tool has no name")
		}
	}
	return nil
}

// CodeBuddyPreparedRequest is the shared executor preparation pipeline
// output: translated body, original translation, credentials and model.
type CodeBuddyPreparedRequest struct {
	Body               []byte
	OriginalTranslated []byte
	Credentials        codebuddy.Credentials
	Model              codebuddy.Model
}

// PrepareCodeBuddyRequest runs the repeated executor preparation pipeline:
// resolve the selected model against the auth snapshot, translate the source
// request to OpenAI (always streaming upstream), apply canonical thinking
// with the account's projected capabilities, apply payload overrides, then
// validate the final body and restore the exact model, stream and
// include_usage flags after configurable overrides.
func PrepareCodeBuddyRequest(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (CodeBuddyPreparedRequest, error) {
	model, err := ResolveCodeBuddyModel(auth, req)
	if err != nil {
		return CodeBuddyPreparedRequest{}, err
	}
	credentials, err := codebuddy.CredentialsFromMetadata(auth.Metadata)
	if err != nil {
		return CodeBuddyPreparedRequest{}, err
	}
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	if err := validateCodeBuddySourceTools(req.Payload, from); err != nil {
		return CodeBuddyPreparedRequest{}, err
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	originalSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalSource = opts.OriginalRequest
	}
	originalPayload := bytes.Clone(originalSource)
	originalTranslated := TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, cfg, from, to, baseModel, bytes.Clone(originalPayload), true)
	body := TranslateRequestWithCodexMultiAgentV2(ctx, opts.Headers, cfg, from, to, baseModel, bytes.Clone(req.Payload), true)
	projected := codebuddy.RegistryModels(credentials, codebuddy.Catalog{Models: []codebuddy.Model{model}})
	if len(projected) == 0 {
		return CodeBuddyPreparedRequest{}, modelNotFoundCodeBuddyError(req.Model)
	}
	summary := translatedRequestSummaryConfig(body, req.Payload, originalPayload, req.Model, from.String(), "openai")
	body, err = thinking.ApplyThinkingWithModelInfoAndSummary(body, originalPayload, req.Model, from.String(), "openai", "codebuddy", projected[0], summary)
	if err != nil {
		return CodeBuddyPreparedRequest{}, err
	}
	requestedModel := PayloadRequestedModel(opts, req.Model)
	requestPath := PayloadRequestPath(opts)
	body = ApplyPayloadConfigWithRequest(cfg, baseModel, to.String(), from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body, err = PrepareCodeBuddyPayload(body, model, credentials.Realm)
	if err != nil {
		return CodeBuddyPreparedRequest{}, err
	}
	// Restore cached reasoning traces last: nothing downstream may strip
	// them, and only genuine cache hits are applied, never fabrications.
	body = RepairCodeBuddyReasoningContent(auth, body)
	return CodeBuddyPreparedRequest{Body: body, OriginalTranslated: originalTranslated, Credentials: credentials, Model: model}, nil
}

// validateCodeBuddyReasoning checks the final normalized reasoning effort
// against the account snapshot. Absent controls stay absent.
func validateCodeBuddyReasoning(payload map[string]any, model codebuddy.Model) error {
	raw, ok := payload["reasoning_effort"]
	if !ok || raw == nil {
		return nil
	}
	effort, ok := raw.(string)
	if !ok || strings.TrimSpace(effort) == "" {
		return requestScopedCodeBuddyError("invalid reasoning_effort value")
	}
	if model.SupportsReasoning == nil {
		return requestScopedCodeBuddyError(fmt.Sprintf("model %q has no verified reasoning capability; refusing reasoning control", model.ID))
	}
	if !*model.SupportsReasoning {
		return requestScopedCodeBuddyError(fmt.Sprintf("model %q does not support reasoning control", model.ID))
	}
	if len(model.Efforts) == 0 {
		return nil
	}
	for _, allowed := range model.Efforts {
		if effort == allowed {
			return nil
		}
	}
	return requestScopedCodeBuddyError(fmt.Sprintf("reasoning effort %q is not supported by model %q", effort, model.ID))
}
