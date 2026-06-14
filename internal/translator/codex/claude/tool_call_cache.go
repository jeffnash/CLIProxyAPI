package claude

import (
	"encoding/json"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const cachedCodexClaudeToolCallLimit = 4096

var codexClaudeToolCallCache = struct {
	sync.Mutex
	items map[string]json.RawMessage
	order []string
}{
	items: make(map[string]json.RawMessage),
}

func rememberCodexClaudeToolCall(callID string, item gjson.Result) {
	if callID == "" || !item.Exists() {
		return
	}

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

	if _, exists := codexClaudeToolCallCache.items[callID]; !exists {
		codexClaudeToolCallCache.order = append(codexClaudeToolCallCache.order, callID)
	}
	codexClaudeToolCallCache.items[callID] = json.RawMessage(raw)

	for len(codexClaudeToolCallCache.order) > cachedCodexClaudeToolCallLimit {
		oldest := codexClaudeToolCallCache.order[0]
		codexClaudeToolCallCache.order = codexClaudeToolCallCache.order[1:]
		delete(codexClaudeToolCallCache.items, oldest)
	}
}

func cachedCodexClaudeToolCall(callID string) (json.RawMessage, bool) {
	if callID == "" {
		return nil, false
	}
	codexClaudeToolCallCache.Lock()
	defer codexClaudeToolCallCache.Unlock()
	item, ok := codexClaudeToolCallCache.items[callID]
	if !ok {
		return nil, false
	}
	return append(json.RawMessage(nil), item...), true
}

func resetCodexClaudeToolCallCacheForTest() {
	codexClaudeToolCallCache.Lock()
	defer codexClaudeToolCallCache.Unlock()
	codexClaudeToolCallCache.items = make(map[string]json.RawMessage)
	codexClaudeToolCallCache.order = nil
}
