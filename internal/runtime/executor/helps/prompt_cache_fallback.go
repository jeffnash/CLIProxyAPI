package helps

import (
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// maxStatelessPromptCacheSignalBytes bounds each payload signal hashed into a
// stateless prompt_cache_key so huge prompts cost O(1) to fingerprint.
const maxStatelessPromptCacheSignalBytes = 4096

// StatelessPromptCacheKey derives a deterministic prompt_cache_key for
// stateless clients that send neither prompt_cache_key nor session identity
// (e.g. OMP openai-responses turns, which re-send the full history every
// turn). Stability across append-only turns is what lets upstream prefix
// caches (Meta Muse Spark, OpenAI) engage.
//
// identity must identify the downstream credential (APIKeyFromContext). An
// empty identity yields "" so anonymous payloads never gain a cache scope.
// User-controlled session-ish fields (metadata.user_id) are deliberately
// ignored so one user cannot scope another user's chats.
func StatelessPromptCacheKey(provider, modelName, sourceFormat string, payload []byte, identity string) string {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return ""
	}
	segments := []string{
		"cli-proxy-api:stateless-prompt-cache",
		strings.ToLower(strings.TrimSpace(provider)),
		strings.ToLower(strings.TrimSpace(modelName)),
		strings.ToLower(strings.TrimSpace(sourceFormat)),
		identity,
		statelessPromptCacheFingerprint(payload),
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join(segments, "\x00"))).String()
}

// statelessPromptCacheFingerprint extracts conversation-stable signals: an
// explicit chain id when present, else the prompt prefix (instructions,
// tools, first input item) that append-only clients repeat every turn.
func statelessPromptCacheFingerprint(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	root := gjson.ParseBytes(payload)
	if chain := firstNonBlankStatelessSignal(
		root.Get("previous_response_id").String(),
		root.Get("conversation_id").String(),
		root.Get("conversation.id").String(),
	); chain != "" {
		return "chain\x00" + truncateStatelessPromptCacheSignal(chain)
	}
	signals := make([]string, 0, 3)
	if v := strings.TrimSpace(root.Get("instructions").String()); v != "" {
		signals = append(signals, "instructions\x00"+truncateStatelessPromptCacheSignal(v))
	}
	if tools := root.Get("tools"); tools.Exists() {
		signals = append(signals, "tools\x00"+truncateStatelessPromptCacheSignal(tools.Raw))
	}
	if first := statelessPromptCacheFirstItem(root); first != "" {
		signals = append(signals, "first\x00"+first)
	}
	return strings.Join(signals, "\n")
}

// statelessPromptCacheFirstItem returns the first prompt item across the
// OpenAI-family shapes (responses input, chat messages, gemini contents).
func statelessPromptCacheFirstItem(root gjson.Result) string {
	for _, path := range []string{"input.0", "messages.0", "contents.0"} {
		if item := root.Get(path); item.Exists() {
			return truncateStatelessPromptCacheSignal(item.Raw)
		}
	}
	if input := root.Get("input"); input.Exists() && input.Type == gjson.String {
		return truncateStatelessPromptCacheSignal(input.String())
	}
	return ""
}

func truncateStatelessPromptCacheSignal(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxStatelessPromptCacheSignalBytes {
		return value[:maxStatelessPromptCacheSignalBytes]
	}
	return value
}

func firstNonBlankStatelessSignal(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
