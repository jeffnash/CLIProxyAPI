package claude

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	cachedCodexClaudeToolCallLimit = 4096
	cachedCodexClaudeToolCallTTL   = 15 * time.Minute
	codexClaudeDefaultCacheScope   = "global"
)

type cachedCodexClaudeToolCallEntry struct {
	item      json.RawMessage
	expiresAt time.Time
}

var codexClaudeToolCallCache = struct {
	sync.Mutex
	items map[string]cachedCodexClaudeToolCallEntry
	order []string
}{
	items: make(map[string]cachedCodexClaudeToolCallEntry),
}

func rememberCodexClaudeToolCall(scope, callID string, item gjson.Result) {
	if callID == "" || !item.Exists() {
		return
	}
	key := codexClaudeToolCallCacheKey(scope, callID)

	raw := []byte(`{"type":"function_call","call_id":"","name":"","arguments":{}}`)
	raw, _ = sjson.SetBytes(raw, "call_id", callID)
	raw, _ = sjson.SetBytes(raw, "name", item.Get("name").String())
	if args := item.Get("arguments"); args.Exists() {
		raw, _ = sjson.SetRawBytes(raw, "arguments", []byte(args.Raw))
	}
	if id := item.Get("id"); id.Exists() {
		raw, _ = sjson.SetRawBytes(raw, "id", []byte(id.Raw))
	}
	if status := item.Get("status"); status.Exists() {
		raw, _ = sjson.SetRawBytes(raw, "status", []byte(status.Raw))
	}

	codexClaudeToolCallCache.Lock()
	defer codexClaudeToolCallCache.Unlock()

	evictExpiredCodexClaudeToolCallsLocked(time.Now())
	if _, exists := codexClaudeToolCallCache.items[key]; !exists {
		codexClaudeToolCallCache.order = append(codexClaudeToolCallCache.order, key)
	}
	codexClaudeToolCallCache.items[key] = cachedCodexClaudeToolCallEntry{
		item:      json.RawMessage(raw),
		expiresAt: time.Now().Add(cachedCodexClaudeToolCallTTL),
	}

	for len(codexClaudeToolCallCache.order) > cachedCodexClaudeToolCallLimit {
		oldest := codexClaudeToolCallCache.order[0]
		codexClaudeToolCallCache.order = codexClaudeToolCallCache.order[1:]
		delete(codexClaudeToolCallCache.items, oldest)
	}
}

func cachedCodexClaudeToolCall(scope, callID string) (json.RawMessage, bool) {
	if callID == "" {
		return nil, false
	}
	key := codexClaudeToolCallCacheKey(scope, callID)
	codexClaudeToolCallCache.Lock()
	defer codexClaudeToolCallCache.Unlock()
	now := time.Now()
	evictExpiredCodexClaudeToolCallsLocked(now)
	entry, ok := codexClaudeToolCallCache.items[key]
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
		delete(codexClaudeToolCallCache.items, key)
		return nil, false
	}
	return append(json.RawMessage(nil), entry.item...), true
}

func codexClaudeToolCallCacheKey(scope, callID string) string {
	if scope == "" {
		scope = codexClaudeDefaultCacheScope
	}
	return scope + "\x00" + callID
}

func codexClaudeToolCallScope(rawJSON []byte) string {
	if len(rawJSON) == 0 || !gjson.ValidBytes(rawJSON) {
		return codexClaudeDefaultCacheScope
	}
	for _, paths := range [][]string{
		{
			"metadata.conversation_id",
			"metadata.session_id",
			"conversation_id",
			"session_id",
			"previous_response_id",
		},
		{"model"},
		{"tools"},
	} {
		if scope := codexClaudeToolCallScopeFromPaths(rawJSON, paths); scope != "" {
			return scope
		}
	}
	return codexClaudeDefaultCacheScope
}

func codexClaudeToolCallScopeFromPaths(rawJSON []byte, paths []string) string {
	h := sha256.New()
	wrote := false
	for _, path := range paths {
		value := gjson.GetBytes(rawJSON, path)
		if !value.Exists() {
			continue
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(value.Raw))
		_, _ = h.Write([]byte{0})
		wrote = true
	}
	if !wrote {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func evictExpiredCodexClaudeToolCallsLocked(now time.Time) {
	if len(codexClaudeToolCallCache.order) == 0 {
		return
	}
	kept := codexClaudeToolCallCache.order[:0]
	for _, key := range codexClaudeToolCallCache.order {
		entry, ok := codexClaudeToolCallCache.items[key]
		if !ok {
			continue
		}
		if !entry.expiresAt.IsZero() && now.After(entry.expiresAt) {
			delete(codexClaudeToolCallCache.items, key)
			continue
		}
		kept = append(kept, key)
	}
	codexClaudeToolCallCache.order = kept
}

func resetCodexClaudeToolCallCacheForTest() {
	codexClaudeToolCallCache.Lock()
	defer codexClaudeToolCallCache.Unlock()
	codexClaudeToolCallCache.items = make(map[string]cachedCodexClaudeToolCallEntry)
	codexClaudeToolCallCache.order = nil
}
