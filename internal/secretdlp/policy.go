package secretdlp

import "strings"

type PathGlob struct {
	Pattern []string
	Kind    SegKind
}

type PathPack struct {
	Name       string
	Redactable []PathGlob
	RawOnly    bool
}

func packForRoute(path string) PathPack {
	switch path {
	case "/v1/chat/completions":
		return PathPack{
			Name: "openai-chat",
			Redactable: []PathGlob{
				{Pattern: []string{"system"}, Kind: ContentText},
				{Pattern: []string{"messages", "*", "content"}, Kind: ContentText},
				{Pattern: []string{"messages", "*", "content", "*", "text"}, Kind: ContentText},
				{Pattern: []string{"messages", "*", "tool_calls", "*", "function", "arguments"}, Kind: ToolArgs},
			},
		}
	case "/v1/messages":
		return PathPack{
			Name: "anthropic-messages",
			Redactable: []PathGlob{
				{Pattern: []string{"system"}, Kind: ContentText},
				{Pattern: []string{"messages", "*", "content", "*", "text"}, Kind: ContentText},
				{Pattern: []string{"messages", "*", "content", "*", "input", "**"}, Kind: ToolArgs},
				{Pattern: []string{"messages", "*", "content", "*", "content"}, Kind: ToolResult},
				{Pattern: []string{"messages", "*", "content", "*", "content", "**"}, Kind: ToolResult},
				{Pattern: []string{"content", "*", "input", "**"}, Kind: ToolArgs},
			},
		}
	case "/v1/responses", "/backend-api/codex/responses":
		return PathPack{
			Name: "openai-responses",
			Redactable: []PathGlob{
				{Pattern: []string{"instructions"}, Kind: ContentText},
				{Pattern: []string{"input", "*", "content", "*", "text"}, Kind: ContentText},
				{Pattern: []string{"input", "*", "content", "*", "input_text"}, Kind: ContentText},
				{Pattern: []string{"input", "*", "arguments"}, Kind: ToolArgs},
				{Pattern: []string{"input", "*", "function_call", "arguments"}, Kind: ToolArgs},
				{Pattern: []string{"input", "*", "function_call_output", "output"}, Kind: ToolResult},
				{Pattern: []string{"output", "*", "content", "*", "text"}, Kind: ContentText},
				{Pattern: []string{"output", "*", "arguments"}, Kind: ToolArgs},
				{Pattern: []string{"function_call", "arguments"}, Kind: ToolArgs},
				{Pattern: []string{"function_call_output", "output"}, Kind: ToolResult},
			},
		}
	default:
		return PathPack{Name: "raw-fallback", RawOnly: true}
	}
}

func (p PathPack) segmentKind(path []string) (SegKind, bool) {
	if p.RawOnly {
		return ContentText, false
	}
	if isSecretDLPBlockedJSONPath(path) {
		return ContentText, false
	}
	for _, glob := range p.Redactable {
		if matchPathGlob(glob.Pattern, path) {
			return glob.Kind, true
		}
	}
	return ContentText, false
}

func matchPathGlob(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[len(pattern)-1] == "**" {
		prefix := pattern[:len(pattern)-1]
		if len(path) < len(prefix) {
			return false
		}
		return matchPathGlob(prefix, path[:len(prefix)])
	}
	if len(pattern) != len(path) {
		return false
	}
	for i := range pattern {
		switch pattern[i] {
		case "*":
			continue
		default:
			if !strings.EqualFold(normalizeJSONPathKey(pattern[i]), normalizeJSONPathKey(path[i])) {
				return false
			}
		}
	}
	return true
}

func pathLast(path []string) string {
	if len(path) == 0 {
		return ""
	}
	return path[len(path)-1]
}
