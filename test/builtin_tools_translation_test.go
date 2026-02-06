package test

import (
	"bytes"
	"testing"

	_ "github.com/router-for-me/CLIProxyAPI/v6/internal/translator"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestOpenAIToCodex_PreservesBuiltinTools(t *testing.T) {
	in := []byte(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search","search_context_size":"high"}],
		"tool_choice":{"type":"web_search"}
	}`)

	out := sdktranslator.TranslateRequest(sdktranslator.FormatOpenAI, sdktranslator.FormatCodex, "gpt-5", in, false)

	if got := gjson.GetBytes(out, "tools.#").Int(); got != 1 {
		t.Fatalf("expected 1 tool, got %d: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "web_search" {
		t.Fatalf("expected tools[0].type=web_search, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tools.0.search_context_size").String(); got != "high" {
		t.Fatalf("expected tools[0].search_context_size=high, got %q: %s", got, string(out))
	}
	if got := gjson.GetBytes(out, "tool_choice.type").String(); got != "web_search" {
		t.Fatalf("expected tool_choice.type=web_search, got %q: %s", got, string(out))
	}
}

func TestOpenAIResponsesToOpenAI_IgnoresBuiltinTools(t *testing.T) {
	in := []byte(`{
		"model":"gpt-5",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"web_search","search_context_size":"low"}]
	}`)

	out := sdktranslator.TranslateRequest(sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI, "gpt-5", in, false)

	if got := gjson.GetBytes(out, "tools.#").Int(); got != 0 {
		t.Fatalf("expected 0 tools (builtin tools not supported in Chat Completions), got %d: %s", got, string(out))
	}
}

func TestGeminiToCodex_DeterministicCallIDs(t *testing.T) {
	in := []byte(`{
		"system_instruction": {"parts": [{"text": "sys"}]},
		"contents": [
			{"role": "model", "parts": [{"functionCall": {"name": "tool_a", "args": {"x": 1}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "tool_a", "response": {"result": "ok"}}}]}
		]
	}`)

	out1 := sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatCodex, "gpt-5", in, false)
	out2 := sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatCodex, "gpt-5", in, false)

	if !bytes.Equal(out1, out2) {
		t.Fatalf("expected deterministic translation; outputs differ\n1=%s\n2=%s", string(out1), string(out2))
	}

	callID := gjson.GetBytes(out1, `input.#(type=="function_call").call_id`).String()
	outID := gjson.GetBytes(out1, `input.#(type=="function_call_output").call_id`).String()
	if callID == "" {
		t.Fatalf("expected function_call.call_id to be set: %s", string(out1))
	}
	if outID == "" {
		t.Fatalf("expected function_call_output.call_id to be set: %s", string(out1))
	}
	if outID != callID {
		t.Fatalf("expected function_call_output.call_id to match function_call.call_id; got %q vs %q", outID, callID)
	}
}

func TestGeminiToCodex_DeterministicCallIDs_KeyOrderNormalization(t *testing.T) {
	// Same args with different key order should produce the same call_id
	inKeysAB := []byte(`{
		"contents": [
			{"role": "model", "parts": [{"functionCall": {"name": "tool_x", "args": {"a": 1, "b": 2}}}]}
		]
	}`)
	inKeysBA := []byte(`{
		"contents": [
			{"role": "model", "parts": [{"functionCall": {"name": "tool_x", "args": {"b": 2, "a": 1}}}]}
		]
	}`)

	outAB := sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatCodex, "gpt-5", inKeysAB, false)
	outBA := sdktranslator.TranslateRequest(sdktranslator.FormatGemini, sdktranslator.FormatCodex, "gpt-5", inKeysBA, false)

	callIDAB := gjson.GetBytes(outAB, `input.#(type=="function_call").call_id`).String()
	callIDBA := gjson.GetBytes(outBA, `input.#(type=="function_call").call_id`).String()

	if callIDAB == "" || callIDBA == "" {
		t.Fatalf("expected call_id to be set; AB=%q BA=%q", callIDAB, callIDBA)
	}
	if callIDAB != callIDBA {
		t.Fatalf("expected same call_id regardless of JSON key order; AB=%q BA=%q\noutAB=%s\noutBA=%s",
			callIDAB, callIDBA, string(outAB), string(outBA))
	}
}
