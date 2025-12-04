package responses

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIResponsesRequestToOpenAIChatCompletions_StringContent(t *testing.T) {
	payload := []byte(`{
		"model": "copilot-gpt-4.1",
		"input": [
			{"role":"system","content":"stay concise"},
			{"role":"user","content":"hello there"}
		]
	}`)

	out := ConvertOpenAIResponsesRequestToOpenAIChatCompletions("copilot-gpt-4.1", payload, false)

	msgs := gjson.GetBytes(out, "messages")
	if !msgs.IsArray() {
		t.Fatalf("messages not array: %s", msgs.Raw)
	}

	system := msgs.Array()[0]
	if got := system.Get("role").String(); got != "system" {
		t.Fatalf("system role = %q, want system", got)
	}
	if got := system.Get("content").String(); got != "stay concise" {
		t.Fatalf("system content = %q, want stay concise", got)
	}

	user := msgs.Array()[1]
	if got := user.Get("role").String(); got != "user" {
		t.Fatalf("user role = %q, want user", got)
	}
	if got := user.Get("content").String(); got != "hello there" {
		t.Fatalf("user content = %q, want hello there", got)
	}
}
