package executor

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func promptCacheFallbackTestContext(apiKey string) context.Context {
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	if apiKey != "" {
		ginCtx.Set("userApiKey", apiKey)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func promptCacheFallbackTestExecutor() *OpenAICompatExecutor {
	return NewOpenAICompatExecutor("openai-compatibility", &config.Config{
		OpenAICompatibility: []config.OpenAICompatibility{{
			Name:                  "compat",
			SupportPromptCacheKey: true,
		}},
	})
}

func promptCacheFallbackTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		Provider: "openai-compatibility",
		Attributes: map[string]string{
			"compat_name":  "compat",
			"provider_key": "compat",
		},
	}
}

func TestOpenAICompatExecutorApplyPromptCacheKey_StatelessFallbackStableAcrossTurns(t *testing.T) {
	executor := promptCacheFallbackTestExecutor()
	auth := promptCacheFallbackTestAuth()
	ctx := promptCacheFallbackTestContext("downstream-caller")
	// Append-only stateless resend, as OMP openai-responses clients behave:
	// every turn repeats the full history without prompt_cache_key or
	// previous_response_id.
	turn1 := []byte(`{"model":"muse-spark-1.3-contributor","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
	turn2 := []byte(`{"model":"muse-spark-1.3-contributor","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)

	keys := make([]string, 0, 2)
	for _, payload := range [][]byte{turn1, turn2} {
		translated, errApply := executor.applyPromptCacheKey(
			ctx,
			auth,
			sdktranslator.FromString("openai-response"),
			"muse-spark-1.3-contributor",
			cliproxyexecutor.Request{Model: "muse-spark-1.3-contributor", Payload: payload},
			cliproxyexecutor.Options{},
			[]byte(`{"model":"muse-spark-1.3-contributor","messages":[]}`),
		)
		if errApply != nil {
			t.Fatalf("applyPromptCacheKey error: %v", errApply)
		}
		keys = append(keys, gjson.GetBytes(translated, "prompt_cache_key").String())
	}
	if keys[0] == "" {
		t.Fatalf("stateless responses turn produced no prompt_cache_key")
	}
	if keys[0] != keys[1] {
		t.Fatalf("stateless prompt_cache_key unstable across turns: %q vs %q", keys[0], keys[1])
	}
	if _, errParse := uuid.Parse(keys[0]); errParse != nil {
		t.Fatalf("stateless prompt_cache_key %q is not a UUID: %v", keys[0], errParse)
	}

	otherPayload := []byte(`{"model":"muse-spark-1.3-contributor","instructions":"be concise","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"different conversation"}]}]}`)
	translated, errApply := executor.applyPromptCacheKey(
		ctx,
		auth,
		sdktranslator.FromString("openai-response"),
		"muse-spark-1.3-contributor",
		cliproxyexecutor.Request{Model: "muse-spark-1.3-contributor", Payload: otherPayload},
		cliproxyexecutor.Options{},
		[]byte(`{"model":"muse-spark-1.3-contributor","messages":[]}`),
	)
	if errApply != nil {
		t.Fatalf("applyPromptCacheKey error: %v", errApply)
	}
	if got := gjson.GetBytes(translated, "prompt_cache_key").String(); got == "" || got == keys[0] {
		t.Fatalf("distinct conversation prompt_cache_key = %q, want a distinct non-empty key", got)
	}
}

func TestOpenAICompatExecutorApplyPromptCacheKey_StatelessFallbackRequiresIdentity(t *testing.T) {
	executor := promptCacheFallbackTestExecutor()
	auth := promptCacheFallbackTestAuth()
	payload := []byte(`{"model":"muse-spark-1.3-contributor","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)

	translated, errApply := executor.applyPromptCacheKey(
		promptCacheFallbackTestContext(""),
		auth,
		sdktranslator.FromString("openai-response"),
		"muse-spark-1.3-contributor",
		cliproxyexecutor.Request{Model: "muse-spark-1.3-contributor", Payload: payload},
		cliproxyexecutor.Options{},
		[]byte(`{"model":"muse-spark-1.3-contributor","messages":[]}`),
	)
	if errApply != nil {
		t.Fatalf("applyPromptCacheKey error: %v", errApply)
	}
	if got := gjson.GetBytes(translated, "prompt_cache_key").String(); got != "" {
		t.Fatalf("identity-less request must not gain a prompt_cache_key, got %q", got)
	}
}
