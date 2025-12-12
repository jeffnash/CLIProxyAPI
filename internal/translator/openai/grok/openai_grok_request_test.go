package grok

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	grokauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/grok"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
)

type mockHTTPClient struct {
	resp *http.Response
	err  error
}

func (m *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return m.resp, m.err
}

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
	}{
		{
			name:       "Tool result message",
			messages:   json.RawMessage(`[{"role":"tool","tool_call_id":"call_123","content":"result"}]`),
			wantPrefix: "tool_result: [call_id=call_123] result\n",
		},
		{
			name:       "Assistant with tool_calls",
			messages:   json.RawMessage(`[{"role":"assistant","tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"London\"}"}}]}]`),
			wantPrefix: "assistant: <tool_call>{\"tool_name\":\"get_weather\",\"arguments\":{\"location\":\"London\"}}</tool_call>\n",
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
			if !strings.Contains(msg, tt.wantPrefix) {
				t.Errorf("message missing expected prefix, got %v", msg)
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
			var msg struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			}
			json.Unmarshal(tt.messageJSON, &msg)

			_, _, images := extractOpenAIContent(payload) // payload is []byte, matches function signature

			if !strings.Contains(message, tt.wantText) && tt.wantText != "" {
				t.Errorf("text = %v, want containing %v", message, tt.wantText)
			}
			if len(images) != tt.wantImages {
				t.Errorf("images len = %d, want %d", len(images), tt.wantImages)
			}
		})
	}
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
		cases := []struct {
			cfg  *config.Config
			want bool
		}{
			{nil, true},
			{&config.Config{}, false},
			{&config.Config{Grok: config.GrokConfig{Temporary: true}}, true},
		}
		for i, c := range cases {
			if got := effectiveGrokTemporary(c.cfg); got != c.want {
				t.Errorf("case %d: got %v, want %v", i, got, c.want)
			}
		}
	})
}

func TestBuildGrokVideoPayload(t *testing.T) {
	// BuildGrokVideoPayload is not tested here (requires real HTTP client)
	// Left as placeholder for future expansion

	t.Run("video model with single image - success", func(t *testing.T) {
		// mock upload response
		uploadBody := `{"data":{"asset_id":"abc123","url":"abc123/image.jpg"}}`
		mockClient.On("Do", mock.MatchedBy(func(req *http.Request) bool {
			return req.URL.Path == "/v1/upload" && req.Method == "POST"
		})).Return(&http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(uploadBody)),
		}, nil)

		// mock post creation response
		postBody := `{"data":{"id":"post_456"}}`
		mockClient.On("Do", mock.MatchedBy(func(req *http.Request) bool {
			return strings.Contains(req.URL.Path, "/posts") && req.Method == "POST"
		})).Return(&http.Response{
			StatusCode: 201,
			Body:       io.NopCloser(strings.NewReader(postBody)),
		}, nil)

		input := map[string]json.RawMessage{
			"model":    json.RawMessage(`"grok-imagine-0.9"`),
			"messages": json.RawMessage(`[{"role":"user","content":[{"type":"text","text":"cat"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/4AAQSkZJRg=="}}]}]`),
		}
		payload, _ := json.Marshal(input)

		got, err := ConvertOpenAIRequestToGrok(context.Background(), payload, mockClient, &config.Config{})
		if err != nil {
			t.Fatal(err)
		}

		msg := gjson.GetBytes(got, "message").String()
		if !strings.Contains(msg, "https://grok.com/imagine/post_456") {
			t.Error("missing imagine URL in message")
		}
		if !strings.Contains(msg, "cat") {
			t.Error("missing original text")
		}
		if !strings.Contains(msg, "--mode=custom") {
			t.Error("missing custom mode flag")
		}
		if !gjson.GetBytes(got, "toolOverrides.videoGen").Bool() {
			t.Error("videoGen not enabled")
		}
	})

	t.Run("video model with image - upload fails - fallback", func(t *testing.T) {
		mockClient.On("Do", mock.Anything).Return(&http.Response{StatusCode: 500}, nil)

		input := map[string]json.RawMessage{
			"model":    json.RawMessage(`"grok-imagine-0.9"`),
			"messages": json.RawMessage(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/4AAQSkZJRg=="}}]}]`),
		}
		payload, _ := json.Marshal(input)

		got, _ := ConvertOpenAIRequestToGrok(context.Background(), payload, mockClient, &config.Config{})

		msg := gjson.GetBytes(got, "message").String()
		if !strings.Contains(msg, "data:image/jpeg;base64,") {
			t.Error("fallback to base64 failed")
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
	defaults := map[string]interface{}{
		"disableSearch":             false,
		"enableImageGeneration":     true,
		"returnImageBytes":          false,
		"enableImageStreaming":      true,
		"imageGenerationCount":      2,
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

	for key, want := range defaults {
		if gotVal := gjson.GetBytes(got, key); gotVal.Value() != want {
			t.Errorf("toggle %s = %v, want %v", key, gotVal.Value(), want)
		}
	}
}

func TestConvertOpenAIRequestToGrok_EdgeCases(t *testing.T) {
	t.Run("nil payload", func(t *testing.T) {
		_, err := ConvertOpenAIRequestToGrok(context.Background(), nil, nil, &config.Config{})
		if err != nil {
			t.Error("nil payload should be handled gracefully")
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
