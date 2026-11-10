package thinking

import "testing"

// TestForkProviderFamiliesKnown guards the fork-only provider families in
// isKnownProviderFamily. Without them, cross-family thinking configs for these
// providers take the unknown-family path instead of being clamped like the
// upstream families (e.g. CodeBuddy effort levels stop clamping to high).
// The fork delta here is a single case line, which makes it easy to lose in
// an upstream merge of validate.go.
func TestForkProviderFamiliesKnown(t *testing.T) {
	for _, provider := range []string{"chutes", "codebuddy", "copilot", "cursor", "grok", "kiro", "meta"} {
		if !isKnownProviderFamily(provider) {
			t.Errorf("isKnownProviderFamily(%q) = false, want true; fork provider family appears dropped", provider)
		}
	}
}
