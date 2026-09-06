package executor

import (
	"bytes"
	"fmt"
	"hash/fnv"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Branch-maintained truncate-tools support: passthru routes may set the
// truncate_tools auth attribute, in which case tool names longer than 64
// characters are deterministically shortened upstream and restored in
// responses and stream lines.

func isTruncateToolsEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	return auth.Attributes["truncate_tools"] == "true"
}
func truncateToolName(name string) string {
	if len(name) <= 64 {
		return name
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	hash := fmt.Sprintf("%08x", h.Sum32())
	keep := 64 - 9 // 55 + "_" + 8 = 64
	if len(name) < keep {
		keep = len(name)
	}
	return name[:keep] + "_" + hash
}
func fnvHash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
func prepareTruncatedToolNamesForUpstream(body []byte) ([]byte, map[string]string) {
	reverseMap := make(map[string]string)
	// Helper to record mapping, handling collisions by adding suffix
	record := func(original, truncated string) {
		if _, exists := reverseMap[truncated]; !exists {
			reverseMap[truncated] = original
		} else if reverseMap[truncated] != original {
			// Collision: append counter to hash (should be extremely rare)
			for i := 1; ; i++ {
				alt := truncated[:55] + fmt.Sprintf("_%06x%d", fnvHash(original)%0xffffff, i)
				alt = alt[:64]
				if _, ok := reverseMap[alt]; !ok {
					reverseMap[alt] = original
					break
				}
			}
		}
	}
	_ = record
	// 1. tools[].name
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && tools.IsArray() {
		tools.ForEach(func(idx, tool gjson.Result) bool {
			// Skip built-in tools with type field
			if tool.Get("type").Exists() && tool.Get("type").String() != "" {
				return true
			}
			name := tool.Get("name").String()
			if len(name) > 64 {
				trunc := truncateToolName(name)
				// Handle rare hash collision with existing mapping
				if existing, ok := reverseMap[trunc]; ok && existing != name {
					// Find alternative by tweaking hash
					for i := 1; ; i++ {
						h := fnv.New32a()
						h.Write([]byte(fmt.Sprintf("%s%d", name, i)))
						hash := fmt.Sprintf("%08x", h.Sum32())
						alt := name[:55] + "_" + hash
						if _, exists := reverseMap[alt]; !exists {
							trunc = alt
							break
						}
					}
				}
				path := fmt.Sprintf("tools.%d.name", idx.Int())
				body, _ = sjson.SetBytes(body, path, trunc)
				reverseMap[trunc] = name
			}
			return true
		})
	}
	// 2. tool_choice.name
	if gjson.GetBytes(body, "tool_choice.type").String() == "tool" {
		tcName := gjson.GetBytes(body, "tool_choice.name").String()
		if len(tcName) > 64 {
			trunc := truncateToolName(tcName)
			if _, ok := reverseMap[trunc]; !ok {
				reverseMap[trunc] = tcName
			}
			body, _ = sjson.SetBytes(body, "tool_choice.name", trunc)
		} else if trunc, ok := reverseMap[tcName]; ok {
			// Already mapped via tools, but tool_choice may reference long name not in tools array
			_ = trunc
		} else {
			// Also handle case where tcName is short but original was long and mapped
			// Look for reverse mapping where original == tcName
			for short, orig := range reverseMap {
				if orig == tcName && short != tcName {
					body, _ = sjson.SetBytes(body, "tool_choice.name", short)
					break
				}
			}
		}
		// Ensure tool_choice is also mapped if it was truncated via tools
		tcNameAfter := gjson.GetBytes(body, "tool_choice.name").String()
		if orig, ok := reverseMap[tcNameAfter]; ok && orig != tcNameAfter {
			// Already handled
		} else if len(tcName) > 64 {
			// Already set above
		} else {
			// Check if tcName corresponds to a long original that was truncated
			for short, orig := range reverseMap {
				if orig == tcName {
					body, _ = sjson.SetBytes(body, "tool_choice.name", short)
					break
				}
			}
		}
	}
	// 3. messages[].content[].tool_use.name and tool_reference.tool_name
	messages := gjson.GetBytes(body, "messages")
	if messages.Exists() && messages.IsArray() {
		messages.ForEach(func(msgIdx, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.Exists() || !content.IsArray() {
				return true
			}
			content.ForEach(func(contentIdx, part gjson.Result) bool {
				switch part.Get("type").String() {
				case "tool_use":
					name := part.Get("name").String()
					if len(name) > 64 {
						trunc := truncateToolName(name)
						if _, ok := reverseMap[trunc]; !ok {
							reverseMap[trunc] = name
						}
						path := fmt.Sprintf("messages.%d.content.%d.name", msgIdx.Int(), contentIdx.Int())
						body, _ = sjson.SetBytes(body, path, trunc)
					} else {
						// Also map if this name was previously truncated as a tool
						for short, orig := range reverseMap {
							if orig == name {
								path := fmt.Sprintf("messages.%d.content.%d.name", msgIdx.Int(), contentIdx.Int())
								body, _ = sjson.SetBytes(body, path, short)
								break
							}
						}
					}
				case "tool_reference":
					toolName := part.Get("tool_name").String()
					if len(toolName) > 64 {
						trunc := truncateToolName(toolName)
						if _, ok := reverseMap[trunc]; !ok {
							reverseMap[trunc] = toolName
						}
						path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIdx.Int(), contentIdx.Int())
						body, _ = sjson.SetBytes(body, path, trunc)
					} else {
						for short, orig := range reverseMap {
							if orig == toolName {
								path := fmt.Sprintf("messages.%d.content.%d.tool_name", msgIdx.Int(), contentIdx.Int())
								body, _ = sjson.SetBytes(body, path, short)
								break
							}
						}
					}
				case "tool_result":
					nestedContent := part.Get("content")
					if nestedContent.Exists() && nestedContent.IsArray() {
						nestedContent.ForEach(func(nestedIdx, nestedPart gjson.Result) bool {
							if nestedPart.Get("type").String() == "tool_reference" {
								nestedToolName := nestedPart.Get("tool_name").String()
								if len(nestedToolName) > 64 {
									trunc := truncateToolName(nestedToolName)
									if _, ok := reverseMap[trunc]; !ok {
										reverseMap[trunc] = nestedToolName
									}
									nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIdx.Int(), contentIdx.Int(), nestedIdx.Int())
									body, _ = sjson.SetBytes(body, nestedPath, trunc)
								} else {
									for short, orig := range reverseMap {
										if orig == nestedToolName {
											nestedPath := fmt.Sprintf("messages.%d.content.%d.content.%d.tool_name", msgIdx.Int(), contentIdx.Int(), nestedIdx.Int())
											body, _ = sjson.SetBytes(body, nestedPath, short)
											break
										}
									}
								}
							}
							return true
						})
					}
				}
				return true
			})
			return true
		})
	}
	if len(reverseMap) == 0 {
		return body, nil
	}
	return body, reverseMap
}
func restoreTruncatedToolNamesFromResponse(body []byte, reverseMap map[string]string) []byte {
	if len(reverseMap) == 0 {
		return body
	}
	content := gjson.GetBytes(body, "content")
	if !content.Exists() || !content.IsArray() {
		return body
	}
	content.ForEach(func(idx, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "tool_use":
			name := part.Get("name").String()
			if orig, ok := reverseMap[name]; ok {
				path := fmt.Sprintf("content.%d.name", idx.Int())
				body, _ = sjson.SetBytes(body, path, orig)
			}
		case "tool_reference":
			toolName := part.Get("tool_name").String()
			if orig, ok := reverseMap[toolName]; ok {
				path := fmt.Sprintf("content.%d.tool_name", idx.Int())
				body, _ = sjson.SetBytes(body, path, orig)
			}
		case "tool_result":
			nestedContent := part.Get("content")
			if nestedContent.Exists() && nestedContent.IsArray() {
				nestedContent.ForEach(func(nestedIdx, nestedPart gjson.Result) bool {
					if nestedPart.Get("type").String() == "tool_reference" {
						nestedToolName := nestedPart.Get("tool_name").String()
						if orig, ok := reverseMap[nestedToolName]; ok {
							nestedPath := fmt.Sprintf("content.%d.content.%d.tool_name", idx.Int(), nestedIdx.Int())
							body, _ = sjson.SetBytes(body, nestedPath, orig)
						}
					}
					return true
				})
			}
		}
		return true
	})
	return body
}
func restoreTruncatedToolNamesFromStreamLine(line []byte, reverseMap map[string]string) []byte {
	if len(reverseMap) == 0 {
		return line
	}
	payload := helps.JSONPayload(line)
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return line
	}
	contentBlock := gjson.GetBytes(payload, "content_block")
	if !contentBlock.Exists() {
		return line
	}
	blockType := contentBlock.Get("type").String()
	var updated []byte
	var err error
	switch blockType {
	case "tool_use":
		name := contentBlock.Get("name").String()
		if orig, ok := reverseMap[name]; ok {
			updated, err = sjson.SetBytes(payload, "content_block.name", orig)
			if err != nil {
				return line
			}
		} else {
			return line
		}
	case "tool_reference":
		toolName := contentBlock.Get("tool_name").String()
		if orig, ok := reverseMap[toolName]; ok {
			updated, err = sjson.SetBytes(payload, "content_block.tool_name", orig)
			if err != nil {
				return line
			}
		} else {
			return line
		}
	default:
		return line
	}
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		return append([]byte("data: "), updated...)
	}
	return updated
}
