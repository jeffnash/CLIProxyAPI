package helps

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	reasoningContentCacheMax = 4096
	reasoningContentCacheTTL = 6 * time.Hour
)

type reasoningContentCacheEntry struct {
	value     string
	updatedAt time.Time
}

type reasoningContentToolCache struct {
	mu      sync.Mutex
	entries map[string]reasoningContentCacheEntry
}

var defaultReasoningContentToolCache = &reasoningContentToolCache{
	entries: make(map[string]reasoningContentCacheEntry),
}

// PreserveReasoningContentEnabled reports whether an auth route opted into
// repairing missing assistant reasoning_content for tool-call continuations.
func PreserveReasoningContentEnabled(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return truthyAttr(auth.Attributes["preserve_reasoning_content"]) ||
		truthyAttr(auth.Attributes["preserve-reasoning-content"])
}

// RepairMissingReasoningContentForToolCalls fills assistant reasoning_content
// from cached upstream responses when clients drop it before tool-result turns.
func RepairMissingReasoningContentForToolCalls(auth *cliproxyauth.Auth, payload []byte) []byte {
	if !PreserveReasoningContentEnabled(auth) || len(payload) == 0 {
		return payload
	}
	scope := reasoningContentScope(auth)
	if scope == "" {
		return payload
	}
	return repairMissingReasoningContentForToolCalls(scope, payload)
}

// RecordOpenAIReasoningContentForToolCalls stores reasoning_content from
// OpenAI-compatible non-stream responses that include assistant tool calls.
func RecordOpenAIReasoningContentForToolCalls(auth *cliproxyauth.Auth, payload []byte) {
	if !PreserveReasoningContentEnabled(auth) || len(payload) == 0 {
		return
	}
	scope := reasoningContentScope(auth)
	if scope == "" {
		return
	}
	recordOpenAIReasoningContentForToolCalls(scope, payload)
}

// OpenAIReasoningContentStreamRecorder accumulates OpenAI-compatible stream
// deltas and caches reasoning_content once tool call IDs are known.
type OpenAIReasoningContentStreamRecorder struct {
	scope   string
	choices map[int]*openAIReasoningContentChoice
}

type openAIReasoningContentChoice struct {
	reasoning   string
	toolCallIDs map[int]string
}

// NewOpenAIReasoningContentStreamRecorder creates a recorder for opted-in auths.
func NewOpenAIReasoningContentStreamRecorder(auth *cliproxyauth.Auth) *OpenAIReasoningContentStreamRecorder {
	if !PreserveReasoningContentEnabled(auth) {
		return nil
	}
	scope := reasoningContentScope(auth)
	if scope == "" {
		return nil
	}
	return &OpenAIReasoningContentStreamRecorder{
		scope:   scope,
		choices: make(map[int]*openAIReasoningContentChoice),
	}
}

// Observe records reasoning/tool-call deltas from a raw SSE data line.
func (r *OpenAIReasoningContentStreamRecorder) Observe(line []byte) {
	if r == nil || r.scope == "" || len(line) == 0 {
		return
	}
	payload := openAIStreamDataPayload(line)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !gjson.ValidBytes(payload) {
		return
	}
	for _, choice := range gjson.GetBytes(payload, "choices").Array() {
		index := int(choice.Get("index").Int())
		state := r.choices[index]
		if state == nil {
			state = &openAIReasoningContentChoice{toolCallIDs: make(map[int]string)}
			r.choices[index] = state
		}
		delta := choice.Get("delta")
		if rc := delta.Get("reasoning_content"); rc.Exists() && rc.String() != "" {
			state.reasoning += rc.String()
		}
		for _, toolCall := range delta.Get("tool_calls").Array() {
			toolIndex := int(toolCall.Get("index").Int())
			if id := strings.TrimSpace(toolCall.Get("id").String()); id != "" {
				state.toolCallIDs[toolIndex] = id
			}
		}
		r.flushChoice(state)
	}
}

func (r *OpenAIReasoningContentStreamRecorder) flushChoice(state *openAIReasoningContentChoice) {
	if state == nil || strings.TrimSpace(state.reasoning) == "" || len(state.toolCallIDs) == 0 {
		return
	}
	for _, id := range state.toolCallIDs {
		defaultReasoningContentToolCache.set(r.scope, id, state.reasoning)
	}
}

func openAIStreamDataPayload(line []byte) []byte {
	line = bytes.TrimSpace(line)
	if !bytes.HasPrefix(line, []byte("data:")) {
		return nil
	}
	return bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
}

func recordOpenAIReasoningContentForToolCalls(scope string, payload []byte) {
	for _, choice := range gjson.GetBytes(payload, "choices").Array() {
		message := choice.Get("message")
		reasoning := strings.TrimSpace(message.Get("reasoning_content").String())
		if reasoning == "" {
			continue
		}
		for _, toolCall := range message.Get("tool_calls").Array() {
			if id := strings.TrimSpace(toolCall.Get("id").String()); id != "" {
				defaultReasoningContentToolCache.set(scope, id, message.Get("reasoning_content").String())
			}
		}
	}
}

func repairMissingReasoningContentForToolCalls(scope string, payload []byte) []byte {
	messages := gjson.GetBytes(payload, "messages")
	if !messages.IsArray() {
		return payload
	}
	out := payload
	repaired := 0
	fallbacks := 0
	for i, msg := range messages.Array() {
		if !strings.EqualFold(strings.TrimSpace(msg.Get("role").String()), "assistant") {
			continue
		}
		if rc := msg.Get("reasoning_content"); rc.Exists() && strings.TrimSpace(rc.String()) != "" {
			continue
		}
		toolCalls := msg.Get("tool_calls")
		reasoning := ""
		if toolCalls.IsArray() {
			reasoning = reasoningContentForToolCalls(scope, toolCalls)
		}
		if reasoning == "" {
			reasoning = fallbackReasoningContent(msg)
			fallbacks++
		}
		updated, errSet := sjson.SetBytes(out, fmt.Sprintf("messages.%d.reasoning_content", i), reasoning)
		if errSet == nil {
			out = updated
			repaired++
		}
	}
	if repaired > 0 {
		log.WithFields(log.Fields{
			"repaired":  repaired,
			"fallbacks": fallbacks,
		}).Debug("reasoning_content repair: filled missing assistant reasoning_content")
	}
	return out
}

func reasoningContentForToolCalls(scope string, toolCalls gjson.Result) string {
	seen := make(map[string]struct{})
	parts := make([]string, 0)
	for _, toolCall := range toolCalls.Array() {
		id := strings.TrimSpace(toolCall.Get("id").String())
		if id == "" {
			continue
		}
		value, ok := defaultReasoningContentToolCache.get(scope, id)
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		parts = append(parts, value)
	}
	return strings.Join(parts, "\n\n")
}

func fallbackReasoningContent(msg gjson.Result) string {
	content := strings.TrimSpace(msg.Get("content").String())
	if content != "" {
		return content
	}
	return "[reasoning unavailable]"
}

func reasoningContentScope(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes == nil {
		return strings.TrimSpace(auth.ID)
	}
	parts := []string{
		strings.TrimSpace(auth.Attributes["base_url"]),
		strings.TrimSpace(auth.Attributes["passthru_routing_name"]),
		strings.TrimSpace(auth.Attributes["upstream_model"]),
	}
	hasRoutePart := false
	for _, part := range parts {
		if part != "" {
			hasRoutePart = true
			break
		}
	}
	if hasRoutePart {
		return strings.Join(parts, "\x00")
	}
	return strings.TrimSpace(auth.ID)
}

func truthyAttr(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func (c *reasoningContentToolCache) set(scope, toolCallID, value string) {
	scope = strings.TrimSpace(scope)
	toolCallID = strings.TrimSpace(toolCallID)
	if scope == "" || toolCallID == "" || strings.TrimSpace(value) == "" {
		return
	}
	now := time.Now()
	key := reasoningContentCacheKey(scope, toolCallID)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = reasoningContentCacheEntry{value: value, updatedAt: now}
	if len(c.entries) > reasoningContentCacheMax {
		c.pruneLocked(now)
	}
}

func (c *reasoningContentToolCache) get(scope, toolCallID string) (string, bool) {
	scope = strings.TrimSpace(scope)
	toolCallID = strings.TrimSpace(toolCallID)
	if scope == "" || toolCallID == "" {
		return "", false
	}
	now := time.Now()
	key := reasoningContentCacheKey(scope, toolCallID)
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if now.Sub(entry.updatedAt) > reasoningContentCacheTTL {
		delete(c.entries, key)
		return "", false
	}
	return entry.value, true
}

func (c *reasoningContentToolCache) pruneLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.Sub(entry.updatedAt) > reasoningContentCacheTTL {
			delete(c.entries, key)
		}
	}
	if len(c.entries) <= reasoningContentCacheMax {
		return
	}
	keys := make([]string, 0, len(c.entries))
	for key := range c.entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return c.entries[keys[i]].updatedAt.Before(c.entries[keys[j]].updatedAt)
	})
	for len(c.entries) > reasoningContentCacheMax && len(keys) > 0 {
		delete(c.entries, keys[0])
		keys = keys[1:]
	}
}

func reasoningContentCacheKey(scope, toolCallID string) string {
	return scope + "\x00" + toolCallID
}
