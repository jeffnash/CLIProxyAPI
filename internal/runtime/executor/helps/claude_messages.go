package helps

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// NormalizeClaudeConsecutiveTurns applies the Anthropic Messages contract that
// consecutive user or assistant records form one turn. It preserves content
// block order and leaves records with unknown message-level extensions intact.
func NormalizeClaudeConsecutiveTurns(rawJSON []byte) []byte {
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.IsArray() {
		return rawJSON
	}

	items := messages.Array()
	if len(items) < 2 {
		return rawJSON
	}

	normalized := []byte(`[]`)
	changed := false
	for start := 0; start < len(items); {
		role := items[start].Get("role").String()
		end := start + 1
		if role == "user" || role == "assistant" {
			for end < len(items) && items[end].Get("role").String() == role {
				end++
			}
		}

		if end-start > 1 {
			if merged, ok := mergeClaudeTurn(items[start:end], role); ok {
				normalized, _ = sjson.SetRawBytes(normalized, "-1", merged)
				changed = true
				start = end
				continue
			}
		}

		for _, message := range items[start:end] {
			normalized, _ = sjson.SetRawBytes(normalized, "-1", []byte(message.Raw))
		}
		start = end
	}

	if !changed {
		return rawJSON
	}
	updated, err := sjson.SetRawBytes(rawJSON, "messages", normalized)
	if err != nil {
		return rawJSON
	}
	return updated
}

func mergeClaudeTurn(messages []gjson.Result, role string) ([]byte, bool) {
	merged := []byte(`{"role":"","content":[]}`)
	merged, _ = sjson.SetBytes(merged, "role", role)

	for _, message := range messages {
		fields := message.Map()
		if len(fields) != 2 || !fields["role"].Exists() || !fields["content"].Exists() {
			return nil, false
		}

		content := fields["content"]
		switch {
		case content.Type == gjson.String:
			part := []byte(`{"type":"text","text":""}`)
			part, _ = sjson.SetBytes(part, "text", content.String())
			merged, _ = sjson.SetRawBytes(merged, "content.-1", part)
		case content.IsArray():
			content.ForEach(func(_, part gjson.Result) bool {
				merged, _ = sjson.SetRawBytes(merged, "content.-1", []byte(part.Raw))
				return true
			})
		default:
			return nil, false
		}
	}

	return merged, true
}
