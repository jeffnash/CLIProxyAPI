package helps

import (
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeClaudeConsecutiveTurns(t *testing.T) {
	tests := []struct {
		name      string
		messages  string
		wantRole  string
		wantTexts []string
	}{
		{
			name: "two user strings",
			messages: `[
				{"role":"user","content":"goal context"},
				{"role":"user","content":"visible request"}
			]`,
			wantRole:  "user",
			wantTexts: []string{"goal context", "visible request"},
		},
		{
			name: "four user records preserve mixed block order",
			messages: `[
				{"role":"user","content":[{"type":"text","text":"goal context"}]},
				{"role":"user","content":"visible request"},
				{"role":"user","content":[{"type":"text","text":"refreshed goal context"}]},
				{"role":"user","content":"continuation"}
			]`,
			wantRole:  "user",
			wantTexts: []string{"goal context", "visible request", "refreshed goal context", "continuation"},
		},
		{
			name: "assistant blocks combine",
			messages: `[
				{"role":"assistant","content":[{"type":"text","text":"working"}]},
				{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"inspect","input":{}}]}
			]`,
			wantRole:  "assistant",
			wantTexts: []string{"working"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := []byte(`{"messages":` + tt.messages + `}`)
			result := NormalizeClaudeConsecutiveTurns(input)
			messages := gjson.GetBytes(result, "messages").Array()
			if len(messages) != 1 {
				t.Fatalf("messages = %s, want one combined turn", gjson.GetBytes(result, "messages").Raw)
			}
			if got := messages[0].Get("role").String(); got != tt.wantRole {
				t.Fatalf("role = %q, want %q", got, tt.wantRole)
			}

			var gotTexts []string
			for _, part := range messages[0].Get("content").Array() {
				if part.Get("type").String() == "text" {
					gotTexts = append(gotTexts, part.Get("text").String())
				}
			}
			if !reflect.DeepEqual(gotTexts, tt.wantTexts) {
				t.Fatalf("text blocks = %#v, want %#v", gotTexts, tt.wantTexts)
			}
		})
	}
}

func TestNormalizeClaudeConsecutiveTurnsPreservesUnknownMessageExtensions(t *testing.T) {
	input := []byte(`{"messages":[
		{"role":"user","content":"one","extension":{"keep":true}},
		{"role":"user","content":"two"}
	]}`)
	result := NormalizeClaudeConsecutiveTurns(input)
	if got := gjson.GetBytes(result, "messages.#").Int(); got != 2 {
		t.Fatalf("messages count = %d, want unmerged records", got)
	}
	if !gjson.GetBytes(result, "messages.0.extension.keep").Bool() {
		t.Fatalf("unknown extension was lost: %s", result)
	}
}
