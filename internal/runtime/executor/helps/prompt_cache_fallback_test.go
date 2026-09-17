package helps

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStatelessPromptCacheKey_RequiresIdentity(t *testing.T) {
	payload := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":"hello"}]}`)
	for _, identity := range []string{"", "   "} {
		if got := StatelessPromptCacheKey("meta", "m", "openai-response", payload, identity); got != "" {
			t.Fatalf("identity %q produced key %q, want empty", identity, got)
		}
	}
}

func TestStatelessPromptCacheKey_StableAcrossAppendOnlyTurns(t *testing.T) {
	turn1 := []byte(`{"model":"m","instructions":"sys","tools":[{"type":"function","name":"read"}],"input":[{"role":"user","content":"hello"}]}`)
	turn2 := []byte(`{"model":"m","instructions":"sys","tools":[{"type":"function","name":"read"}],"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"next"}]}`)

	first := StatelessPromptCacheKey("meta", "m", "openai-response", turn1, "caller")
	second := StatelessPromptCacheKey("meta", "m", "openai-response", turn2, "caller")
	if first == "" {
		t.Fatalf("expected non-empty key for stateless turn")
	}
	if first != second {
		t.Fatalf("key unstable across append-only turns: %q vs %q", first, second)
	}
	if _, errParse := uuid.Parse(first); errParse != nil {
		t.Fatalf("key %q is not a UUID: %v", first, errParse)
	}
}

func TestStatelessPromptCacheKey_SeparatesScopes(t *testing.T) {
	base := []byte(`{"model":"m","instructions":"sys","input":[{"role":"user","content":"hello"}]}`)
	baseline := StatelessPromptCacheKey("meta", "m", "openai-response", base, "caller")

	variants := map[string]string{
		"provider": StatelessPromptCacheKey("other", "m", "openai-response", base, "caller"),
		"model":    StatelessPromptCacheKey("meta", "other", "openai-response", base, "caller"),
		"format":   StatelessPromptCacheKey("meta", "m", "openai", base, "caller"),
		"identity": StatelessPromptCacheKey("meta", "m", "openai-response", base, "other-caller"),
		"content":  StatelessPromptCacheKey("meta", "m", "openai-response", []byte(`{"model":"m","instructions":"sys","input":[{"role":"user","content":"bye"}]}`), "caller"),
	}
	for name, got := range variants {
		if got == "" {
			t.Fatalf("%s variant produced empty key", name)
		}
		if got == baseline {
			t.Fatalf("%s variant shares key %q with baseline", name, got)
		}
	}
}

func TestStatelessPromptCacheKey_PrefersExplicitChainID(t *testing.T) {
	chained := []byte(`{"model":"m","previous_response_id":"resp_1","input":[{"role":"user","content":"hello"}]}`)
	unchained := []byte(`{"model":"m","input":[{"role":"user","content":"hello"}]}`)

	key := StatelessPromptCacheKey("meta", "m", "openai-response", chained, "caller")
	other := StatelessPromptCacheKey("meta", "m", "openai-response", unchained, "caller")
	if key == "" || key == other {
		t.Fatalf("chained key = %q, unchained = %q, want distinct non-empty", key, other)
	}
	repeat := StatelessPromptCacheKey("meta", "m", "openai-response", chained, "caller")
	if repeat != key {
		t.Fatalf("chained key unstable: %q vs %q", key, repeat)
	}
}

func TestStatelessPromptCacheKey_BoundedSignals(t *testing.T) {
	huge := []byte(`{"model":"m","instructions":"` + strings.Repeat("x", 65536) + `","input":[{"role":"user","content":"hello"}]}`)
	key := StatelessPromptCacheKey("meta", "m", "openai-response", huge, "caller")
	if key == "" {
		t.Fatalf("expected non-empty key for huge payload")
	}
	if _, errParse := uuid.Parse(key); errParse != nil {
		t.Fatalf("key %q is not a UUID: %v", key, errParse)
	}
}
