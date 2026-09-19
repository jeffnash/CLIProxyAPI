package helps

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	codebuddy "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codebuddy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func codeBuddyTestModel() codebuddy.Model {
	tools := true
	reasoning := true
	return codebuddy.Model{
		ID: "hy4-preview", MaxInputTokens: 1000000, MaxOutputTokens: 64000,
		SupportsTools: &tools, SupportsReasoning: &reasoning,
		Efforts: []string{"high"}, DefaultEffort: "high",
	}
}

func prepareCodeBuddy(t *testing.T, body string, model codebuddy.Model, realm codebuddy.Realm) []byte {
	t.Helper()
	original := []byte(body)
	out, err := PrepareCodeBuddyPayload(original, model, realm)
	if err != nil {
		t.Fatalf("PrepareCodeBuddyPayload: %v", err)
	}
	if !bytes.Equal(original, []byte(body)) {
		t.Fatal("input body mutated")
	}
	return out
}

func prepareCodeBuddyErr(t *testing.T, body string, model codebuddy.Model, realm codebuddy.Realm) *CodeBuddyError {
	t.Helper()
	original := []byte(body)
	_, err := PrepareCodeBuddyPayload(original, model, realm)
	if err == nil {
		t.Fatalf("expected error for %s", body)
	}
	if !bytes.Equal(original, []byte(body)) {
		t.Fatal("input body mutated on error path")
	}
	var typed *CodeBuddyError
	if !errors.As(err, &typed) {
		t.Fatalf("error type = %T", err)
	}
	if !typed.IsRequestScoped() {
		t.Fatalf("rejection not request-scoped: %v", err)
	}
	return typed
}

func TestPrepareCodeBuddyPayload(t *testing.T) {
	model := codeBuddyTestModel()
	t.Run("forces stream and model", func(t *testing.T) {
		out := prepareCodeBuddy(t, `{"model":"other","messages":[{"role":"user","content":"hi"}],"stream":false}`, model, codebuddy.RealmCN)
		if gjson.GetBytes(out, "model").String() != "hy4-preview" {
			t.Fatalf("model = %s", out)
		}
		if !gjson.GetBytes(out, "stream").Bool() || !gjson.GetBytes(out, "stream_options.include_usage").Bool() {
			t.Fatalf("stream = %s", out)
		}
	})
	t.Run("moves max completion tokens", func(t *testing.T) {
		out := prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":1024}`, model, codebuddy.RealmCN)
		if gjson.GetBytes(out, "max_tokens").Int() != 1024 || gjson.GetBytes(out, "max_completion_tokens").Exists() {
			t.Fatalf("max = %s", out)
		}
		out = prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":5,"max_completion_tokens":1024}`, model, codebuddy.RealmCN)
		if gjson.GetBytes(out, "max_tokens").Int() != 5 {
			t.Fatalf("max = %s", out)
		}
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":1.5}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":0}`, model, codebuddy.RealmCN)
	})
	t.Run("maps developer and rejects unknown roles", func(t *testing.T) {
		out := prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`, model, codebuddy.RealmCN)
		if gjson.GetBytes(out, "messages.0.role").String() != "system" {
			t.Fatalf("roles = %s", out)
		}
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"function","content":"x"}]}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"content":"x"}]}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m"}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{invalid}`, model, codebuddy.RealmCN)
	})
	t.Run("prepends system for international", func(t *testing.T) {
		out := prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, model, codebuddy.RealmGlobal)
		if gjson.GetBytes(out, "messages.0.role").String() != "system" || gjson.GetBytes(out, "messages.1.role").String() != "user" {
			t.Fatalf("messages = %s", out)
		}
		out = prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, model, codebuddy.RealmCN)
		if gjson.GetBytes(out, "messages.#").Int() != 1 {
			t.Fatalf("cn prepended: %s", out)
		}
		out = prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`, model, codebuddy.RealmGlobal)
		if gjson.GetBytes(out, "messages.#").Int() != 2 {
			t.Fatalf("duplicate system: %s", out)
		}
	})
	t.Run("tool choice none strips tools", func(t *testing.T) {
		unsupported := model
		noTools := false
		unsupported.SupportsTools = &noTools
		out := prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"none"}`, unsupported, codebuddy.RealmCN)
		if gjson.GetBytes(out, "tools").Exists() || gjson.GetBytes(out, "tool_choice").Exists() {
			t.Fatalf("stripped = %s", out)
		}
	})
	t.Run("tool choice auto required preserved", func(t *testing.T) {
		for _, choice := range []string{"auto", "required"} {
			out := prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"`+choice+`"}`, model, codebuddy.RealmCN)
			if gjson.GetBytes(out, "tool_choice").String() != choice {
				t.Fatalf("choice = %s", out)
			}
		}
	})
	t.Run("named choice converts to string", func(t *testing.T) {
		out := prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"echo_nonce"}}],"tool_choice":{"type":"function","function":{"name":"echo_nonce"}}}`, model, codebuddy.RealmCN)
		if gjson.GetBytes(out, "tool_choice").String() != "echo_nonce" {
			t.Fatalf("choice = %s", out)
		}
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"function","function":{"name":"missing"}}}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":{"type":"other"}}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"bogus"}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"function","function":{"name":"f"}}}`, model, codebuddy.RealmCN)
	})
	t.Run("tools need verified capability", func(t *testing.T) {
		unknown := model
		unknown.SupportsTools = nil
		err := prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`, unknown, codebuddy.RealmCN)
		if !strings.Contains(err.Message, "verified") {
			t.Fatalf("message = %q", err.Message)
		}
		unsupported := model
		noTools := false
		unsupported.SupportsTools = &noTools
		err = prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`, unsupported, codebuddy.RealmCN)
		if !strings.Contains(err.Message, "does not support") {
			t.Fatalf("message = %q", err.Message)
		}
	})
	t.Run("tool history round trip", func(t *testing.T) {
		valid := `{"model":"m","messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_b","type":"function","function":{"name":"g","arguments":"{}"}},{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_a","content":"1"},
			{"role":"tool","tool_call_id":"call_b","content":"2"}
		],"tools":[{"type":"function","function":{"name":"f"}},{"type":"function","function":{"name":"g"}}]}`
		prepareCodeBuddy(t, valid, model, codebuddy.RealmCN)
		orphan := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"b","content":"x"}]}`
		prepareCodeBuddyErr(t, orphan, model, codebuddy.RealmCN)
		duplicate := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"x"},{"role":"tool","tool_call_id":"a","content":"y"}]}`
		prepareCodeBuddyErr(t, duplicate, model, codebuddy.RealmCN)
		missing := `{"model":"m","messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`
		prepareCodeBuddyErr(t, missing, model, codebuddy.RealmCN)
		split := `{"model":"m","messages":[
			{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"a","content":"x"},
			{"role":"user","content":"interrupt"},
			{"role":"tool","tool_call_id":"b","content":"y"}]}`
		prepareCodeBuddyErr(t, split, model, codebuddy.RealmCN)
	})
	t.Run("sequential tool rounds", func(t *testing.T) {
		twoRounds := `{"model":"m","messages":[
			{"role":"user","content":"go"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"a","content":"one"},
			{"role":"assistant","content":"next","tool_calls":[{"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"b","content":"two"},
			{"role":"user","content":"again"}
		],"tools":[{"type":"function","function":{"name":"f"}},{"type":"function","function":{"name":"g"}}]}`
		prepareCodeBuddy(t, twoRounds, model, codebuddy.RealmCN)
		parallelRounds := `{"model":"m","messages":[
			{"role":"assistant","content":null,"tool_calls":[{"id":"a1","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"a2","type":"function","function":{"name":"g","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"a2","content":"2"},
			{"role":"tool","tool_call_id":"a1","content":"1"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"b1","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"b2","type":"function","function":{"name":"g","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"b1","content":"1"},
			{"role":"tool","tool_call_id":"b2","content":"2"}
		],"tools":[{"type":"function","function":{"name":"f"}},{"type":"function","function":{"name":"g"}}]}`
		prepareCodeBuddy(t, parallelRounds, model, codebuddy.RealmCN)
		unresolvedBeforeNextRound := `{"model":"m","messages":[
			{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]},
			{"role":"assistant","tool_calls":[{"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"a","content":"x"},
			{"role":"tool","tool_call_id":"b","content":"y"}]}`
		prepareCodeBuddyErr(t, unresolvedBeforeNextRound, model, codebuddy.RealmCN)
	})
	t.Run("cardinality and formats", func(t *testing.T) {
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`, model, codebuddy.RealmCN)
		prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"text"}}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x"}}]}]}`, model, codebuddy.RealmCN)
		prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, model, codebuddy.RealmCN)
	})
	t.Run("reasoning checked against snapshot", func(t *testing.T) {
		prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`, model, codebuddy.RealmCN)
		unknown := model
		unknown.SupportsReasoning = nil
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`, unknown, codebuddy.RealmCN)
		prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, unknown, codebuddy.RealmCN)
		out := prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, model, codebuddy.RealmCN)
		if gjson.GetBytes(out, "reasoning_effort").Exists() {
			t.Fatalf("effort forced: %s", out)
		}
	})
	t.Run("preserves parallel calls and stops", func(t *testing.T) {
		out := prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":false,"temperature":0.5,"stop":["x"]}`, model, codebuddy.RealmCN)
		if gjson.GetBytes(out, "parallel_tool_calls").Bool() || gjson.GetBytes(out, "temperature").Float() != 0.5 {
			t.Fatalf("params = %s", out)
		}
	})
	t.Run("tool definitions must be named functions", func(t *testing.T) {
		prepareCodeBuddy(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":"bogus"}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":["bogus"]}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"f"}]}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function"}]}`, model, codebuddy.RealmCN)
		prepareCodeBuddyErr(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":""}}]}`, model, codebuddy.RealmCN)
	})
}

func TestValidateCodeBuddySourceTools(t *testing.T) {
	cases := []struct {
		name    string
		format  sdktranslator.Format
		source  string
		wantErr string
	}{
		{"responses function accepted", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"function","name":"f"}]}`, ""},
		{"responses custom accepted", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"custom","name":"f"}]}`, ""},
		{"responses namespace accepted", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"function","name":"f"}]}]}`, ""},
		{"responses web search rejected", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"web_search_preview"}]}`, "web_search_preview"},
		{"responses file search rejected", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"file_search"}]}`, "file_search"},
		{"responses code interpreter rejected", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"code_interpreter"}]}`, "code_interpreter"},
		{"responses additional tools rejected", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":[{"type":"additional_tools","tools":[{"type":"web_search_preview"}]}]}`, "web_search_preview"},
		{"responses namespace child rejected", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"namespace","name":"ns","tools":[{"type":"mcp"}]}]}`, "mcp"},
		{"responses nameless function rejected", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":[{"type":"function"}]}`, "no name"},
		{"responses non-array rejected", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi","tools":{}}`, "must be an array"},
		{"claude custom accepted", sdktranslator.FormatClaude, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`, ""},
		{"claude web search rejected", sdktranslator.FormatClaude, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, "web_search_20250305"},
		{"claude computer rejected", sdktranslator.FormatClaude, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"computer_20250124","name":"computer"}]}`, "computer_20250124"},
		{"claude nameless rejected", sdktranslator.FormatClaude, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"description":"d"}]}`, "no name"},
		{"openai source skips prevalidation", sdktranslator.FormatOpenAI, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom"}]}`, ""},
		{"no tools accepted", sdktranslator.FormatOpenAIResponse, `{"model":"m","input":"hi"}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCodeBuddySourceTools([]byte(tc.source), tc.format)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q", tc.wantErr)
			}
			var typed *CodeBuddyError
			if !errors.As(err, &typed) || !typed.IsRequestScoped() {
				t.Fatalf("rejection not request-scoped: %v", err)
			}
			if !strings.Contains(typed.Message, tc.wantErr) {
				t.Fatalf("message = %q, want %q", typed.Message, tc.wantErr)
			}
		})
	}
}

func TestResolveCodeBuddyModelRejectsOtherAccountAndRealm(t *testing.T) {
	creds := codebuddy.Credentials{
		Realm: codebuddy.RealmGlobal, AccessToken: "A", RefreshToken: "R", UID: "u",
		Expired: time.Now().Add(time.Hour),
	}
	tools := true
	catalog := codebuddy.Catalog{Models: []codebuddy.Model{{ID: "hy4-preview", SupportsTools: &tools}}}
	auth := &cliproxyauth.Auth{ID: "a", Provider: "codebuddy", Metadata: codebuddy.Metadata(creds, catalog, time.Now())}
	got, err := ResolveCodeBuddyModel(auth, cliproxyexecutor.Request{Model: "codebuddy-global-hy4-preview"})
	if err != nil || got.ID != "hy4-preview" {
		t.Fatalf("resolve = %+v %v", got, err)
	}
	for _, requested := range []string{"codebuddy-cn-hy4-preview", "codebuddy-global-hy4-preview-f", "hy4-preview", "other"} {
		if _, err := ResolveCodeBuddyModel(auth, cliproxyexecutor.Request{Model: requested}); err == nil {
			t.Fatalf("%q resolved", requested)
		}
	}
	// Resolved registry info for an unentitled upstream ID is rejected.
	req := cliproxyexecutor.Request{Model: "codebuddy-global-hy4-preview", Metadata: map[string]any{
		"cliproxy.resolved_api_key_model_info": &registry.ModelInfo{ID: "codebuddy-global-hy4-preview", UpstreamID: "hy4-preview-f"},
	}}
	if _, err := ResolveCodeBuddyModel(auth, req); err == nil {
		t.Fatal("unentitled resolved info accepted")
	}
	// Resolved info for an entitled ID resolves even through an alias name.
	req.Metadata["cliproxy.resolved_api_key_model_info"] = &registry.ModelInfo{ID: "hy4", UpstreamID: "hy4-preview"}
	if _, err := ResolveCodeBuddyModel(auth, req); err != nil {
		t.Fatalf("alias resolve: %v", err)
	}
}
