package grok

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

func TestConvertOpenAIRequestToGrok_BasicConversions(t *testing.T) {
	tests := []struct {
		name      string
		input     map[string]json.RawMessage
		wantModel string
		wantMsg   string
		wantTemp  bool
	}{
		{
			name: "Basic text message",
			input: map[string]json.RawMessage{
				"model":    json.RawMessage(`"grok-3-fast"`),
				"messages": json.RawMessage(`[{"role":"user","content":"hello"}]`),
			},
			wantModel: "grok-3",
			wantMsg:   "user: hello\n",
			wantTemp:  true,
		},
		{
			name: "Multi-turn conversation",
			input: map[string]json.RawMessage{
				"model":    json.RawMessage(`"grok-4-heavy"`),
				"messages": json.RawMessage(`[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]`),
			},
			wantModel: "grok-4-heavy",
			wantMsg:   "system: You are helpful\nuser: hi\nassistant: hello\n",
			wantTemp:  true,
		},
		{
			name: "Empty messages array",
			input: map[string]json.RawMessage{
				"model":    json.RawMessage(`"grok-3"`),
				"messages": json.RawMessage(`[]`),
			},
			wantModel: "grok-3",
			wantMsg:   "user: Hello\n",
			wantTemp:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload, _ := json.Marshal(tt.input)
			modelName := gjson.GetBytes(payload, "model").String()
			got := ConvertOpenAIRequestToGrok(modelName, payload, false)

			if gjson.GetBytes(got, "modelName").String() != tt.wantModel {
				t.Errorf("modelName = %v, want %v", gjson.GetBytes(got, "modelName"), tt.wantModel)
			}
			if !strings.Contains(gjson.GetBytes(got, "message").String(), tt.wantMsg) {
				t.Errorf("message missing expected content, got %v", gjson.GetBytes(got, "message").String())
			}
			if gjson.GetBytes(got, "temporary").Bool() != tt.wantTemp {
				t.Errorf("temporary = %v, want %v", gjson.GetBytes(got, "temporary"), tt.wantTemp)
			}
		})
	}
}

func TestConvertOpenAIRequestToGrok_ImageAttachments(t *testing.T) {
	tests := []struct {
		name          string
		messages      json.RawMessage
		wantImagesLen int
	}{
		{
			name:          "Base64 image URL",
			messages:      json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]`),
			wantImagesLen: 1,
		},
		{
			name:          "HTTP image URL",
			messages:      json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/image.jpg"}}]}]`),
			wantImagesLen: 1,
		},
		{
			name:          "Multiple images",
			messages:      json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}},{"type":"image_url","image_url":{"url":"https://example.com/2.jpg"}}]}]`),
			wantImagesLen: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := map[string]json.RawMessage{
				"model":    json.RawMessage(`"grok-3"`),
				"messages": tt.messages,
			}
			payload, _ := json.Marshal(input)
			modelName := gjson.GetBytes(payload, "model").String()
			got := ConvertOpenAIRequestToGrok(modelName, payload, false)

			images := gjson.GetBytes(got, "imageAttachments").Array()
			if len(images) != tt.wantImagesLen {
				t.Errorf("imageAttachments len = %d, want %d", len(images), tt.wantImagesLen)
			}
		})
	}
}

func TestConvertOpenAIRequestToGrok_ToolHandling(t *testing.T) {
	tests := []struct {
		name       string
		messages   json.RawMessage
		wantPrefix string
		wantParts  []string
	}{
		{
			name:       "Tool result message",
			messages:   json.RawMessage(`[{"role":"tool","tool_call_id":"call_123","content":"result"}]`),
			wantPrefix: "tool_result: [call_id=call_123] result\n",
		},
		{
			name:       "Assistant with tool_calls",
			messages:   json.RawMessage(`[{"role":"assistant","tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"London\"}"}}]}]`),
			wantPrefix: "assistant: <tool_call>{\"tool_name\":\"get_weather\",\"call_id\":\"call_abc\",\"arguments\":{\"location\":\"London\"}}</tool_call>\n",
		},
		{
			name:      "Assistant with multiple tool_calls",
			messages:  json.RawMessage(`[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"alpha","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"beta","arguments":"{\"x\":1}"}}]}]`),
			wantParts: []string{`<tool_call>{"tool_name":"alpha","call_id":"call_1","arguments":{}}</tool_call>`, `<tool_call>{"tool_name":"beta","call_id":"call_2","arguments":{"x":1}}</tool_call>`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := map[string]json.RawMessage{
				"model":    json.RawMessage(`"grok-3"`),
				"messages": tt.messages,
			}
			payload, _ := json.Marshal(input)
			modelName := gjson.GetBytes(payload, "model").String()
			got := ConvertOpenAIRequestToGrok(modelName, payload, false)

			msg := gjson.GetBytes(got, "message").String()
			if tt.wantPrefix != "" && !strings.Contains(msg, tt.wantPrefix) {
				t.Errorf("message missing expected prefix, got %v", msg)
			}
			for _, part := range tt.wantParts {
				if !strings.Contains(msg, part) {
					t.Errorf("message missing expected tool call %q, got %v", part, msg)
				}
			}
		})
	}
}

func TestExtractOpenAIContent(t *testing.T) {
	tests := []struct {
		name        string
		messageJSON json.RawMessage
		wantText    string
		wantImages  int
	}{
		{
			name:        "String content",
			messageJSON: json.RawMessage(`{"role":"user","content":"hello"}`),
			wantText:    "user: hello\n",
		},
		{
			name:        "Array content with text and image",
			messageJSON: json.RawMessage(`{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}`),
			wantText:    "user: describe\n",
			wantImages:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(`{"messages":[` + string(tt.messageJSON) + `]}`)
			message, _, images := extractOpenAIContent(payload)

			if tt.wantText != "" && !strings.Contains(message, tt.wantText) {
				t.Errorf("text = %v, want containing %v", message, tt.wantText)
			}
			if len(images) != tt.wantImages {
				t.Errorf("images len = %d, want %d", len(images), tt.wantImages)
			}
		})
	}
}

func TestToolFormatting(t *testing.T) {
	t.Run("tool result without call_id", func(t *testing.T) {
		msg := formatToolResult("", gjson.Parse(`"ok"`))
		if msg != "tool_result: ok" {
			t.Errorf("unexpected tool result: %q", msg)
		}
		if strings.Contains(msg, "[]") || strings.Contains(msg, "  ") {
			t.Errorf("tool result should not include empty call_id or double spaces: %q", msg)
		}
	})

	t.Run("tool result non-string content", func(t *testing.T) {
		msg := formatToolResult("call_9", gjson.Parse(`{"value":1}`))
		want := `tool_result: [call_id=call_9] {"value":1}`
		if msg != want {
			t.Errorf("tool result = %q, want %q", msg, want)
		}
	})

	t.Run("tool call empty arguments", func(t *testing.T) {
		msg := formatToolCall("fn", "call_x", "   ")
		want := `<tool_call>{"tool_name":"fn","call_id":"call_x","arguments":{}}</tool_call>`
		if msg != want {
			t.Errorf("tool call = %q, want %q", msg, want)
		}
	})
}

func TestVideoPayloadHelpers(t *testing.T) {
	t.Run("normalizeImageContentType", func(t *testing.T) {
		cases := []struct {
			in, want string
		}{
			{"image/jpeg", "image/jpeg"},
			{"image/png; charset=utf-8", "image/png"},
			{"", "image/jpeg"},
			{"text/html", "image/jpeg"},
		}
		for _, c := range cases {
			if got := normalizeImageContentType(c.in); got != c.want {
				t.Errorf("normalizeImageContentType(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	})

	t.Run("resolveUploadFileName", func(t *testing.T) {
		cases := []struct {
			ct, want string
		}{
			{"image/jpeg", "image.jpg"},
			{"image/png", "image.png"},
			{"image/gif", "image.gif"},
		}
		for _, c := range cases {
			if got := resolveUploadFileName(c.ct); got != c.want {
				t.Errorf("resolveUploadFileName(%q) = %q, want %q", c.ct, got, c.want)
			}
		}
	})

	t.Run("formatGrokAssetURL", func(t *testing.T) {
		cases := []struct {
			in, want string
		}{
			{"abc123/file.jpg", "https://assets.grok.com/abc123/file.jpg"},
			{"https://assets.grok.com/abc/file.jpg", "https://assets.grok.com/abc/file.jpg"},
		}
		for _, c := range cases {
			if got := formatGrokAssetURL(c.in); got != c.want {
				t.Errorf("formatGrokAssetURL(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	})

	t.Run("formatGrokPostAssetURL", func(t *testing.T) {
		cases := []struct {
			in, want string
		}{
			{"post/abc123/file.jpg", "https://assets.grok.com/post/abc123/file.jpg"},
		}
		for _, c := range cases {
			if got := formatGrokPostAssetURL(c.in); got != c.want {
				t.Errorf("formatGrokPostAssetURL(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	})

	t.Run("effectiveGrokTemporary", func(t *testing.T) {
		falseValue := false
		trueValue := true
		cases := []struct {
			cfg  *config.Config
			want bool
		}{
			{nil, true},
			{&config.Config{}, true},
			{&config.Config{Grok: config.GrokConfig{Temporary: &falseValue}}, false},
			{&config.Config{Grok: config.GrokConfig{Temporary: &trueValue}}, true},
		}
		for i, c := range cases {
			if got := effectiveGrokTemporary(c.cfg); got != c.want {
				t.Errorf("case %d: got %v, want %v", i, got, c.want)
			}
		}
	})
}

func TestBuildGrokVideoPayload(t *testing.T) {
	t.Run("video model with no images - fallback to text payload", func(t *testing.T) {
		input := map[string]json.RawMessage{
			"model":    json.RawMessage(`"grok-imagine-0.9"`),
			"messages": json.RawMessage(`[{"role":"user","content":"cat"}]`),
		}
		payload, _ := json.Marshal(input)

		got, referer, err := BuildGrokVideoPayload(context.Background(), nil, &config.Config{}, "", "", "grok-imagine-0.9", payload)
		if err != nil {
			t.Fatal(err)
		}
		if referer != "" {
			t.Errorf("expected empty referer, got %q", referer)
		}

		msg := gjson.GetBytes(got, "message").String()
		if !strings.Contains(msg, "user: cat") {
			t.Error("missing original text in message")
		}
		if gjson.GetBytes(got, "toolOverrides.videoGen").Exists() {
			t.Error("unexpected videoGen override in fallback payload")
		}
	})
}

func TestConvertOpenAIRequestToGrok_Toggles(t *testing.T) {
	input := map[string]json.RawMessage{
		"model":    json.RawMessage(`"grok-3"`),
		"messages": json.RawMessage(`[{"role":"user","content":"hi"}]`),
	}
	payload, _ := json.Marshal(input)
	modelName := gjson.GetBytes(payload, "model").String()
	got := ConvertOpenAIRequestToGrok(modelName, payload, false)

	// Verify all default toggles as per plan
	boolDefaults := map[string]bool{
		"disableSearch":             false,
		"enableImageGeneration":     true,
		"returnImageBytes":          false,
		"enableImageStreaming":      true,
		"forceConcise":              false,
		"enableSideBySide":          true,
		"sendFinalMetadata":         true,
		"isReasoning":               false,
		"disableTextFollowUps":      true,
		"disableMemory":             false,
		"isAsyncChat":               false,
		"returnRawGrokInXaiRequest": false,
		"forceSideBySide":           false,
	}

	for key, want := range boolDefaults {
		if gotVal := gjson.GetBytes(got, key).Bool(); gotVal != want {
			t.Errorf("toggle %s = %v, want %v", key, gotVal, want)
		}
	}

	if gotVal := gjson.GetBytes(got, "imageGenerationCount").Int(); gotVal != 2 {
		t.Errorf("toggle imageGenerationCount = %v, want %v", gotVal, 2)
	}
}

func TestConvertOpenAIRequestToGrok_EdgeCases(t *testing.T) {
	t.Run("nil payload", func(t *testing.T) {
		got := ConvertOpenAIRequestToGrok("grok-3", nil, false)
		if msg := gjson.GetBytes(got, "message").String(); msg == "" {
			t.Error("nil payload should still produce a message")
		}
	})

	t.Run("very long message", func(t *testing.T) {
		longText := strings.Repeat("a", 15000)
		input := map[string]json.RawMessage{
			"model":    json.RawMessage(`"grok-3"`),
			"messages": json.RawMessage(`[{"role":"user","content":"` + longText + `"}]`),
		}
		payload, _ := json.Marshal(input)
		modelName := gjson.GetBytes(payload, "model").String()
		got := ConvertOpenAIRequestToGrok(modelName, payload, false)

		if len(gjson.GetBytes(got, "message").String()) <= 1000 {
			t.Error("long message was truncated")
		}
	})
}
