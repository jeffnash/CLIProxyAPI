package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/tidwall/gjson"
)

func TestV1Models_IncludesLimits_WhenPresent(t *testing.T) {
	server := newTestServer(t)

	reg := registry.GetGlobalRegistry()
	clientID := "http-test-client-limits-present"
	modelID := "http-test-model-limits-present"
	created := time.Now().Unix()

	reg.RegisterClient(clientID, "openai", []*registry.ModelInfo{{
		ID:                  modelID,
		Object:              "model",
		Created:             created,
		OwnedBy:             "test-provider",
		ContextLength:       123456,
		MaxCompletionTokens: 7890,
	}})
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	root := gjson.ParseBytes(rr.Body.Bytes())
	model := root.Get(`data.#(id=="` + modelID + `")`)
	if !model.Exists() {
		t.Fatalf("expected model %q in /v1/models response: %s", modelID, rr.Body.String())
	}

	// Regression: ensure base OpenAI fields are always present.
	if got := model.Get("id").String(); got != modelID {
		t.Fatalf("expected id %q, got %q", modelID, got)
	}
	if got := model.Get("object").String(); got != "model" {
		t.Fatalf("expected object 'model', got %q", got)
	}
	if !model.Get("created").Exists() {
		t.Fatalf("expected created field to be present")
	}
	if got := model.Get("owned_by").String(); got != "test-provider" {
		t.Fatalf("expected owned_by %q, got %q", "test-provider", got)
	}

	if got := model.Get("context_length"); !got.Exists() {
		t.Fatalf("expected context_length to be present")
	} else if got.Int() != 123456 {
		t.Fatalf("expected context_length 123456, got %d", got.Int())
	}
	if got := model.Get("max_completion_tokens"); !got.Exists() {
		t.Fatalf("expected max_completion_tokens to be present")
	} else if got.Int() != 7890 {
		t.Fatalf("expected max_completion_tokens 7890, got %d", got.Int())
	}
}

func TestV1Models_FallbacksFromGeminiTokenLimits(t *testing.T) {
	server := newTestServer(t)

	reg := registry.GetGlobalRegistry()
	clientID := "http-test-client-gemini-fallback"
	modelID := "http-test-model-gemini-fallback"
	created := time.Now().Unix()

	// Simulate a Gemini-style model where provider-native input/output token limits are
	// known, but OpenAI-style ContextLength/MaxCompletionTokens are unset.
	reg.RegisterClient(clientID, "gemini", []*registry.ModelInfo{{
		ID:               modelID,
		Object:           "model",
		Created:          created,
		OwnedBy:          "google",
		InputTokenLimit:  999999,
		OutputTokenLimit: 4242,
	}})
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	root := gjson.ParseBytes(rr.Body.Bytes())
	model := root.Get(`data.#(id=="` + modelID + `")`)
	if !model.Exists() {
		t.Fatalf("expected model %q in /v1/models response: %s", modelID, rr.Body.String())
	}

	if got := model.Get("context_length"); !got.Exists() {
		t.Fatalf("expected context_length to be present via fallback mapping")
	} else if got.Int() != 999999 {
		t.Fatalf("expected context_length 999999, got %d", got.Int())
	}
	if got := model.Get("max_completion_tokens"); !got.Exists() {
		t.Fatalf("expected max_completion_tokens to be present via fallback mapping")
	} else if got.Int() != 4242 {
		t.Fatalf("expected max_completion_tokens 4242, got %d", got.Int())
	}
}

func TestV1Models_PassthruModelLimitsAppear(t *testing.T) {
	server := newTestServer(t)

	reg := registry.GetGlobalRegistry()
	clientID := "http-test-client-passthru"
	modelID := "http-test-model-passthru"
	created := time.Now().Unix()

	// Simulate a passthru model registration (as done by sdk/cliproxy/service.go
	// from config passthru routes).
	reg.RegisterClient(clientID, "openai-compatibility", []*registry.ModelInfo{{
		ID:                  modelID,
		Object:              "model",
		Created:             created,
		OwnedBy:             "passthru",
		ContextLength:       8000,
		MaxCompletionTokens: 2000,
	}})
	t.Cleanup(func() { reg.UnregisterClient(clientID) })

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-key")

	rr := httptest.NewRecorder()
	server.engine.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d; body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}

	root := gjson.ParseBytes(rr.Body.Bytes())
	model := root.Get(`data.#(id=="` + modelID + `")`)
	if !model.Exists() {
		t.Fatalf("expected model %q in /v1/models response: %s", modelID, rr.Body.String())
	}

	if got := model.Get("owned_by").String(); got != "passthru" {
		t.Fatalf("expected owned_by 'passthru', got %q", got)
	}
	if got := model.Get("context_length"); !got.Exists() || got.Int() != 8000 {
		t.Fatalf("expected context_length 8000, got %v", got.Value())
	}
	if got := model.Get("max_completion_tokens"); !got.Exists() || got.Int() != 2000 {
		t.Fatalf("expected max_completion_tokens 2000, got %v", got.Value())
	}
}
