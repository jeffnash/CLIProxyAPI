package secretdlp

import (
	"context"
	"testing"
)

func TestBuiltinScannerFindsAssignmentSecret(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	findings, err := scanner.Scan(context.Background(), testSegments(`api_key=sk-testdlpfixture0000000000000000000000000000`, SecretField))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertFinding(t, findings, "sk-testdlpfixture0000000000000000000000000000")
}

func TestBuiltinScannerUsesToolInputKeyContext(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	secret := "aB9xK2mN7pQ5rT8z"
	findings, err := scanner.Scan(context.Background(), []Segment{{
		Path:    "/messages/0/content/0/input/password",
		Value:   secret,
		Kind:    ToolArgs,
		TokSpan: [2]int{0, len(secret)},
	}})
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertFinding(t, findings, secret)
}

func TestVirtualSegmentDocumentIncludesToolInputKeyContext(t *testing.T) {
	doc := string(virtualSegmentDocument([]Segment{{
		Path:  "/messages/0/content/0/input/password",
		Value: "aB9xK2mN7pQ5rT8z",
		Kind:  ToolArgs,
	}}))
	if doc != "password=aB9xK2mN7pQ5rT8z" {
		t.Fatalf("virtualSegmentDocument() = %q, want tool input key context", doc)
	}
}

func TestBuiltinScannerFindsBearerToken(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	findings, err := scanner.Scan(context.Background(), testSegments(`Authorization: Bearer abcdefghijklmnopqrstuvwxyz123456`, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertFinding(t, findings, "abcdefghijklmnopqrstuvwxyz123456")
}

func TestBuiltinScannerFindsJWT(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	token := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.signaturepart"
	findings, err := scanner.Scan(context.Background(), testSegments(token, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertFinding(t, findings, token)
}

func TestBuiltinScannerFindsOpenAIStyleKeyWithoutAssignmentContext(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	secret := "sk-testdlpfixture0000000000000000000000000000"
	findings, err := scanner.Scan(context.Background(), testSegments(secret, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertFinding(t, findings, secret)
}

func TestBuiltinScannerIgnoresCommitSHA(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	findings, err := scanner.Scan(context.Background(), testSegments(`383c34c6ac1537f769fb4380f178d4a5436a42b7`, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %+v, want none", findings)
	}
}

func TestDefaultScannerDoesNotFindToolNameAsSecret(t *testing.T) {
	scanner, err := NewScanner(Config{
		Scanner:        "betterleaks",
		MaxFindings:    10,
		MinValueLength: 12,
	})
	if err != nil {
		t.Fatalf("NewScanner(): %v", err)
	}
	toolName := "mcp__codebase-memory-mcp__manage_adr"
	findings, err := scanner.Scan(context.Background(), testSegments(toolName, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %+v, want none for tool name", findings)
	}
}

func TestBuiltinScannerDoesNotFindHighEntropyTokenByDefault(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	toolName := "mcp__codebase-memory-mcp__manage_adr"
	findings, err := scanner.Scan(context.Background(), testSegments(toolName, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %+v, want none for tool-like high entropy token", findings)
	}
}

func TestBuiltinScannerCanOptIntoHighEntropyToken(t *testing.T) {
	scanner := NewBuiltinScannerWithOptions(10, 12, true)
	secret := "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6"
	findings, err := scanner.Scan(context.Background(), testSegments(secret, SecretField))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertFinding(t, findings, secret)
}

func TestBuiltinScannerRejectsWordlikeGraphQLOperationName(t *testing.T) {
	scanner := NewBuiltinScannerWithOptions(10, 12, true)
	name := "GetUserProfileWithOrdersAndRecommendations"
	findings, err := scanner.Scan(context.Background(), testSegments(name, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %+v, want none for wordlike operation name", findings)
	}
}

func TestFilterFindingsShadowsHighEntropyBelowDefaultThreshold(t *testing.T) {
	findings := []Finding{{Secret: "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6", RuleID: "high-entropy", Source: "builtin", Confidence: 0.70}}
	accepted, shadow := filterFindings(findings, nil, Config{})
	if len(accepted) != 0 {
		t.Fatalf("accepted = %+v, want none", accepted)
	}
	if len(shadow) != 1 {
		t.Fatalf("shadow = %+v, want one shadow finding", shadow)
	}
}

func TestBetterLeaksConfidenceUsesConfiguredRuleMap(t *testing.T) {
	got := betterLeaksConfidence("betterleaks-custom", map[string]float64{"betterleaks-custom": 0.91})
	if got != 0.91 {
		t.Fatalf("betterLeaksConfidence() = %.2f, want 0.91", got)
	}
}

func TestBetterLeaksConfidenceDefaults(t *testing.T) {
	for _, ruleID := range []string{"generic-api-key", "high-entropy-token"} {
		if got := betterLeaksConfidence(ruleID, nil); got != 0.70 {
			t.Fatalf("betterLeaksConfidence(%q) = %.2f, want 0.70", ruleID, got)
		}
	}
	if got := betterLeaksConfidence("unmapped-provider-token", nil); got != 0.40 {
		t.Fatalf("betterLeaksConfidence(unmapped-provider-token) = %.2f, want 0.40", got)
	}
	if got := betterLeaksConfidence("jwt", nil); got != 0.40 {
		t.Fatalf("betterLeaksConfidence(jwt) = %.2f, want 0.40 for builtin-name collision", got)
	}
}

func TestFilterFindingsUsesBetterLeaksConfidencePolicy(t *testing.T) {
	secret := "sk-testdlpfixture0000000000000000000000000000"

	accepted, shadow := filterFindings([]Finding{{
		Secret:     secret,
		RuleID:     "betterleaks-custom",
		Source:     "betterleaks",
		Confidence: betterLeaksConfidence("betterleaks-custom", map[string]float64{"betterleaks-custom": 0.91}),
	}}, nil, Config{})
	if len(accepted) != 1 || len(shadow) != 0 {
		t.Fatalf("accepted=%+v shadow=%+v, want configured BetterLeaks finding accepted", accepted, shadow)
	}

	accepted, shadow = filterFindings([]Finding{{
		Secret:     secret,
		RuleID:     "jwt",
		Source:     "betterleaks",
		Confidence: betterLeaksConfidence("jwt", nil),
	}}, nil, Config{})
	if len(accepted) != 0 || len(shadow) != 1 {
		t.Fatalf("accepted=%+v shadow=%+v, want unmapped builtin-name collision shadowed", accepted, shadow)
	}
}

func TestExplicitRawFindingsSkipsUnmarkedBodiesAndHonorsMaxFindings(t *testing.T) {
	if findings := explicitRawFindings([]byte("large body without explicit secret markers"), 12, 10); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none for unmarked raw body", findings)
	}

	body := []byte("sk-AHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3A0 sk-BHL5JPzaa0VUlLJ7rZYrn78C6GHjrA2kDAPWnlCSWGFzy3B0")
	findings := explicitRawFindings(body, 12, 1)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one capped finding", findings)
	}
}

func TestCompositeScannerDedupesFindings(t *testing.T) {
	composite := &CompositeScanner{
		scanners: []Scanner{
			staticScanner{{Secret: "sk-testdlpfixture0000000000000000000000000000", RuleID: "a", Source: "one"}},
			staticScanner{{Secret: "sk-testdlpfixture0000000000000000000000000000", RuleID: "b", Source: "two"}},
		},
		max:    10,
		minLen: 12,
	}

	findings, err := composite.Scan(context.Background(), testSegments("ignored", ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want one deduped finding", findings)
	}
	assertFinding(t, findings, "sk-testdlpfixture0000000000000000000000000000")
}

type staticScanner []Finding

func (s staticScanner) Scan(context.Context, []Segment) ([]Finding, error) {
	return []Finding(s), nil
}

func TestBuiltinScannerFindsBarePrefixedTokenInContent(t *testing.T) {
	// Live-probe gap: bare unknown-prefix token in message content, no
	// assignment/Bearer context, high-entropy disabled.
	scanner := NewBuiltinScanner(10, 12)
	token := "tp-s8lnnc4nf0a0s296fb63ya9vqzvctz0ohk26q1ewrks0252f"
	findings, err := scanner.Scan(context.Background(), testSegments("my key is "+token+" thanks", ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertFinding(t, findings, token)
	assertRuleAndConfidence(t, findings, token, "bare-credential-prefixed", 0.85)
}

func TestBuiltinScannerFindsBareHexPairTokenInContent(t *testing.T) {
	// Live-probe gap: <32hex>.<16 mixed> bare in content.
	scanner := NewBuiltinScanner(10, 12)
	token := "d84582b16f9f4dbba70ae6d30a6a9762.yvF0iJYCy2eeQHab"
	findings, err := scanner.Scan(context.Background(), testSegments(token, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	assertFinding(t, findings, token)
	assertRuleAndConfidence(t, findings, token, "bare-credential-hexpair", 0.85)
}

func TestBuiltinScannerFindsKnownProviderKeysBare(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	for _, token := range []string{
		"ghp_" + "A1b2C3d4E5f6A1b2C3d4E5f6A1b2C3d4E5f6",
		// 4 separator parts: proves the isExplicitSecretShape exemption from
		// the separatorPartCount>=3 benign heuristic.
		"xoxb-" + "13653274088-4586090492389-Abc1Def2Ghi3Jkl4",
	} {
		findings, err := scanner.Scan(context.Background(), testSegments("use "+token, ContentText))
		if err != nil {
			t.Fatalf("Scan() error = %v", err)
		}
		assertFinding(t, findings, token)
		assertRuleAndConfidence(t, findings, token, "known-provider-key", 0.95)
	}
}

func TestBareCredentialIgnoresIdentifierShapes(t *testing.T) {
	// Regression corpus: none of these may produce any finding when bare in
	// content with default options (high-entropy off).
	scanner := NewBuiltinScanner(10, 12)
	for _, s := range []string{
		"mcp__codebase-memory-mcp__manage_adr",
		"GetUserProfileWithOrdersAndRecommendations",
		"550e8400-e29b-41d4-a716-446655440000",
		"nginx-deployment-66b6c48dd5-abcde",
		"subnet-0a1b2c3d4e5f67890",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // traceparent
		"backup-202601011200000000000000000001",                   // bunched digits, no letters in body
		"intl-internationalizationconfigurations",                 // word concat, no digits
		"sha256-mB5c9kQ3xW7vT2rY8pL4nJ6hG1fD0sA9zX3cV5bN7mQ",      // SRI-style digest
		"d84582b16f9f4dbba70ae6d30a6a9762.tar.gz",
		"d84582b16f9f4dbba70ae6d30a6a9762.snapshot",
		"the file d84582b16f9f4dbba70ae6d30a6a9762.min.js loaded",
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(s, ContentText))
		if err != nil {
			t.Fatalf("Scan(%q) error = %v", s, err)
		}
		if len(findings) != 0 {
			t.Fatalf("Scan(%q) = %v, want no findings", s, findings)
		}
	}
}

func TestFilterFindingsSubtractsBareTokenEqualToToolName(t *testing.T) {
	// A tools[].name that happens to equal a credential-shaped string must
	// suppress the finding everywhere, including content mentions.
	token := "tp-s8lnnc4nf0a0s296fb63ya9vqzvctz0ohk26q1ewrks0252f"
	ids := make(IdentifierSet)
	ids.add(token)
	accepted, shadow := filterFindings([]Finding{{
		Secret:     token,
		RuleID:     "bare-credential-prefixed",
		Source:     "builtin",
		Confidence: 0.85,
	}}, ids, Config{RedactThreshold: 0.80})
	if len(accepted) != 0 || len(shadow) != 0 {
		t.Fatalf("filterFindings() accepted=%v shadow=%v, want identifier subtraction", accepted, shadow)
	}
}

func assertRuleAndConfidence(t *testing.T, findings []Finding, secret, rule string, confidence float64) {
	t.Helper()
	for _, f := range findings {
		if f.Secret != secret {
			continue
		}
		if f.RuleID != rule {
			t.Fatalf("finding %q rule = %q, want %q", secret, f.RuleID, rule)
		}
		if f.Confidence != confidence {
			t.Fatalf("finding %q confidence = %v, want %v", secret, f.Confidence, confidence)
		}
		return
	}
	t.Fatalf("finding %q not present in %v", secret, findings)
}

func testSegments(value string, kind SegKind) []Segment {
	return []Segment{{Path: "/test", Value: value, Kind: kind, TokSpan: [2]int{0, len(value)}}}
}

func assertFinding(t *testing.T, findings []Finding, secret string) {
	t.Helper()
	for _, finding := range findings {
		if finding.Secret == secret {
			return
		}
	}
	t.Fatalf("findings = %+v, want secret %q", findings, secret)
}
