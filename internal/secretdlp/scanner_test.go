package secretdlp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strconv"
	"strings"
	"testing"
)

func testGenBase58Secret(t *testing.T) string {
	t.Helper()
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read(): %v", err)
	}
	if b[0] == 0 {
		b[0] = 1
	}
	n := new(big.Int).SetBytes(b)
	fiftyEight := big.NewInt(58)
	mod := new(big.Int)
	var sb strings.Builder
	for n.Sign() > 0 {
		n.DivMod(n, fiftyEight, mod)
		sb.WriteByte(base58Alphabet[mod.Int64()])
	}
	r := []byte(sb.String())
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	out := string(r)
	if base58DecodedLen(out) != 64 {
		t.Fatalf("generated base58 decodes to %d bytes, want 64", base58DecodedLen(out))
	}
	return out
}

func joinRepeatedInt(value, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = strconv.Itoa(value)
	}
	return strings.Join(parts, ",")
}

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

func TestExplicitRawFindingsFindsKnownProviderWebhookURL(t *testing.T) {
	// Split literal so this synthetic webhook fixture does not trip provider-side
	// push-protection scanners; the runtime value is unchanged.
	webhook := "https://hooks.slack.com/services/T00000000/B00000000/" + "AbCdEfGhIjKlMnOpQrStUvWx"
	findings := explicitRawFindings([]byte(`{"url":"`+webhook+`"}`), 12, 10)
	assertFinding(t, findings, webhook)
	assertRuleAndConfidence(t, findings, webhook, "known-provider-key", 0.95)

	if findings := explicitRawFindings([]byte(`{"url":"https://slack.com/help/articles/000000"}`), 12, 10); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none for non-webhook Slack URL", findings)
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

func TestFilterFindingsKeepsBareTokenEqualToToolName(t *testing.T) {
	// Protocol bytes are protected by segment scoping, but a credential-shaped
	// value in content must not be allowed to egress raw solely because the same
	// value appears as an identifier.
	token := "tp-s8lnnc4nf0a0s296fb63ya9vqzvctz0ohk26q1ewrks0252f"
	ids := make(IdentifierSet)
	ids.add(token)
	accepted, shadow := filterFindings([]Finding{{
		Secret:     token,
		RuleID:     "bare-credential-prefixed",
		Source:     "builtin",
		Confidence: 0.85,
	}}, ids, Config{RedactThreshold: 0.80})
	if len(accepted) != 1 || accepted[0].Secret != token || len(shadow) != 0 {
		t.Fatalf("filterFindings() accepted=%v shadow=%v, want explicit credential accepted", accepted, shadow)
	}
}

func TestFilterFindingsStillSubtractsGenericTokenEqualToToolName(t *testing.T) {
	token := "mcp__codebase-memory-mcp__manage_adr"
	ids := make(IdentifierSet)
	ids.add(token)
	accepted, shadow := filterFindings([]Finding{{
		Secret:     token,
		RuleID:     "betterleaks-generic",
		Source:     "betterleaks",
		Confidence: 0.90,
	}}, ids, Config{RedactThreshold: 0.80})
	if len(accepted) != 0 || len(shadow) != 0 {
		t.Fatalf("filterFindings() accepted=%v shadow=%v, want generic identifier subtraction", accepted, shadow)
	}
}

func TestFindingsBySecretKeepsHighestConfidence(t *testing.T) {
	token := "ghp_A1b2C3d4E5f6A1b2C3d4E5f6A1b2C3d4E5f6"
	findings := findingsBySecret([]Finding{
		{Secret: token, RuleID: "known-provider-key", Confidence: 0.95},
		{Secret: token, RuleID: "bare-credential-prefixed", Confidence: 0.85},
	})
	got := findings[token]
	if got.RuleID != "known-provider-key" || got.Confidence != 0.95 {
		t.Fatalf("findingsBySecret()[%q] = %+v, want highest confidence", token, got)
	}
}

func TestBIP39WordlistEmbedded(t *testing.T) {
	if len(bip39Wordlist) != 2048 {
		t.Fatalf("len(bip39Wordlist) = %d, want 2048", len(bip39Wordlist))
	}
	sum := sha256.Sum256([]byte(bip39WordlistRaw))
	if got, want := hex.EncodeToString(sum[:]), "2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda"; got != want {
		t.Fatalf("bip39_english.txt sha256 = %s, want %s", got, want)
	}
}

func TestBuiltinScannerFindsExpandedKnownProviderKeys(t *testing.T) {
	scanner := NewBuiltinScanner(50, 12)
	for name, token := range map[string]string{
		"slack webhook":     "https://hooks.slack.com/services/T00000000/B00000000/" + "AbCdEfGhIjKlMnOpQrStUvWx",
		"discord webhook":   "https://discord.com/api/webhooks/123456789012345678/AbCdEfGhIjKlMnOpQrStUvWxYz-123",
		"google access":     "ya29.A0AfH6SMDLPFixtureToken1234567890abcdef",
		"google refresh":    "1//0gDLPFixtureRefreshToken1234567890abcdef",
		"huggingface":       "hf_abcdefghijklmnopqrstuvwxyzABCDEFGH1234",
		"groq":              "gsk_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN123456",
		"perplexity":        "pplx-abcdefghijklmnopqrstuvwxyz1234567890ABCD",
		"together":          "tgp_v1_abcdefghijklmnopqrstuvwxyz1234567890ABCD",
		"cohere":            "co_abcdefghijklmnopqrstuvwxyz123456",
		"digitalocean":      "dop_v1_abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890",
		"pypi":              "pypi-abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890",
		"docker hub":        "dckr_pat_abcdefghijklmnopqrstuvwxyz1234567890ABCD",
		"terraform cloud":   "atlasv1.abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890",
		"stripe restricted": "rk_live_" + "abcdefghijklmnopqrstuvwxyz123456",
		"square":            "sq0atp-abcdefghijklmnopqrstuvwxyz1234567890",
		"plaid":             "access-production-abcdefghijklmnopqrstuvwxyz-123456",
		"sendgrid":          "SG.abcdefghijklmnopqrstuvwxyz.ABCDEFGHIJKLMNOPQRSTUVWXYZ123456",
		"mailchimp":         "0123456789abcdef0123456789abcdef" + "-us20",
		// Split literal so this synthetic fixture does not trip provider-side
		// push-protection secret scanners; the runtime value is unchanged.
		"twilio": "SK" + "0123456789abcdef0123456789ABCDEF",
		"gitlab": "glpat-abcdefghijklmnopqrstuvwxyz123456",
	} {
		findings, err := scanner.Scan(context.Background(), testSegments("credential: "+token, ContentText))
		if err != nil {
			t.Fatalf("%s Scan() error = %v", name, err)
		}
		assertFinding(t, findings, token)
		assertRuleAndConfidence(t, findings, token, "known-provider-key", 0.95)
	}
}

func TestExpandedKnownProviderKeysIgnoreFalsePositives(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	for _, s := range []string{
		"https://slack.com/help/articles/000000",
		"https://discord.com/channels/123456789012345678/987654321098765432",
		"ya29.not-long-enough",
		"hf_short",
		"co_short",
		"pplx-project-docs-and-routes",
		"SG.this.is.documentation",
		"SKnothexadecimalnothexadecimalnothex",
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(s, ContentText))
		if err != nil {
			t.Fatalf("Scan(%q) error = %v", s, err)
		}
		if len(findings) != 0 {
			t.Fatalf("Scan(%q) = %+v, want no findings", s, findings)
		}
	}
}

func TestBuiltinScannerFindsContextGatedCloudAndAuthKeys(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	awsSecret := "abcdEFGH1234ijklMNOP5678qrstUVWX9012yzAB"
	azureKey := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+/abcdefghijklmnopqrstuvwxyzABCD=="
	basic := base64.StdEncoding.EncodeToString([]byte("cliproxy:" + "correct-horse-token"))
	for name, tc := range map[string]struct {
		text       string
		secret     string
		rule       string
		confidence float64
	}{
		"aws secret": {
			text:       "aws_secret_access_key=" + awsSecret,
			secret:     awsSecret,
			rule:       "aws-secret-access-key",
			confidence: 0.90,
		},
		"azure account key": {
			text:       "DefaultEndpointsProtocol=https;AccountName=acct;AccountKey=" + azureKey + ";EndpointSuffix=core.windows.net",
			secret:     azureKey,
			rule:       "azure-storage-account-key",
			confidence: 0.90,
		},
		"basic auth": {
			text:       "Authorization: Basic " + basic,
			secret:     basic,
			rule:       "basic-auth",
			confidence: 0.90,
		},
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(tc.text, ContentText))
		if err != nil {
			t.Fatalf("%s Scan() error = %v", name, err)
		}
		assertFinding(t, findings, tc.secret)
		assertRuleAndConfidence(t, findings, tc.secret, tc.rule, tc.confidence)
	}
}

func TestContextGatedCloudAndAuthKeysIgnoreFalsePositives(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	for _, s := range []string{
		"abcdEFGH1234ijklMNOP5678qrstUVWX9012yzAB",
		"sha256:0d17b565c37bcbd895e9d92315a05c1c3c9a29f762b011a10c54a66cd53c9b31",
		"DefaultEndpointsProtocol=https;AccountName=acct;EndpointSuffix=core.windows.net",
		"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("not-a-secret")),
		"Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("user:short")),
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(s, ContentText))
		if err != nil {
			t.Fatalf("Scan(%q) error = %v", s, err)
		}
		if len(findings) != 0 {
			t.Fatalf("Scan(%q) = %+v, want no findings", s, findings)
		}
	}
}

func TestBuiltinScannerFindsPrivateKeyMaterial(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	openSSH := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU=\n-----END OPENSSH PRIVATE KEY-----"
	pgp := "-----BEGIN PGP PRIVATE KEY BLOCK-----\nVersion: Test\n\nabc123\n-----END PGP PRIVATE KEY BLOCK-----"
	age := "AGE-SECRET-KEY-1ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCDEFGH"
	for name, tc := range map[string]struct {
		text       string
		secret     string
		rule       string
		confidence float64
	}{
		"openssh": {text: openSSH, secret: openSSH, rule: "pem-private-key", confidence: 1.0},
		"pgp":     {text: pgp, secret: pgp, rule: "pem-private-key", confidence: 1.0},
		"age":     {text: age, secret: age, rule: "age-secret-key", confidence: 1.0},
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(tc.text, ContentText))
		if err != nil {
			t.Fatalf("%s Scan() error = %v", name, err)
		}
		assertFinding(t, findings, tc.secret)
		assertRuleAndConfidence(t, findings, tc.secret, tc.rule, tc.confidence)
	}
}

func TestPrivateKeyMaterialIgnoresPublicKeyFalsePositives(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	for _, s := range []string{
		"-----BEGIN PUBLIC KEY-----\nabc123\n-----END PUBLIC KEY-----",
		"age1qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq",
		"-----BEGIN PGP PUBLIC KEY BLOCK-----\nabc123\n-----END PGP PUBLIC KEY BLOCK-----",
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(s, ContentText))
		if err != nil {
			t.Fatalf("Scan(%q) error = %v", s, err)
		}
		if len(findings) != 0 {
			t.Fatalf("Scan(%q) = %+v, want no findings", s, findings)
		}
	}
}

// puttyKeyFixture returns a structurally valid PPK-style body plus one of its
// private base64 lines, so tests can assert the redaction span covers it.
func puttyKeyFixture() (body, privateLine string) {
	privateLine = "PRIVATEsecretbase64line0000000000000000000000000000000000"
	body = "PuTTY-User-Key-File-2: ssh-rsa\n" +
		"Encryption: none\n" +
		"Comment: rsa-key-20260101\n" +
		"Public-Lines: 1\n" +
		"AAAAB3NzaC1yc2EApublicline00000000000000000000000000000000\n" +
		"Private-Lines: 2\n" +
		privateLine + "\n" +
		"PRIVATEsecretbase64line1111111111111111111111111111111111\n" +
		"Private-MAC: 1a2b3c4d5e6f78901a2b3c4d5e6f78901a2b3c4d"
	return body, privateLine
}

func TestBuiltinScannerRedactsFullPuTTYPrivateKeyBlock(t *testing.T) {
	scanner := NewBuiltinScanner(10, 12)
	ppk, privateLine := puttyKeyFixture()
	findings, err := scanner.Scan(context.Background(), testSegments(ppk, ContentText))
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	// The finding must span through Private-MAC (not stop at the Private-Lines
	// header), so redacting the matched substring removes the private base64.
	var got string
	for _, f := range findings {
		if strings.Contains(f.Secret, "PuTTY-User-Key-File-") {
			got = f.Secret
		}
	}
	if got == "" {
		t.Fatalf("findings = %+v, want PuTTY private key block", findings)
	}
	if !strings.Contains(got, privateLine) {
		t.Fatalf("PuTTY finding does not span private key lines: %q", got)
	}
}

func TestExplicitRawFindingsRedactsStandalonePuTTYKey(t *testing.T) {
	ppk, privateLine := puttyKeyFixture()
	// A .ppk body carries no -----BEGIN marker; the raw fallback must still find
	// it via its dedicated PuTTY anchor.
	findings := explicitRawFindings([]byte(ppk), 12, 10)
	var got string
	for _, f := range findings {
		if strings.Contains(f.Secret, "PuTTY-User-Key-File-") {
			got = f.Secret
		}
	}
	if got == "" {
		t.Fatalf("findings = %+v, want standalone .ppk body found on raw fallback", findings)
	}
	if !strings.Contains(got, privateLine) {
		t.Fatalf("raw PuTTY finding does not span private key lines: %q", got)
	}
}

func TestCompositeScannerKeepsValidatedRawCredentialWithBenignShape(t *testing.T) {
	scanner, err := NewScanner(Config{Scanner: "builtin", MaxFindings: 10, MinValueLength: 12})
	if err != nil {
		t.Fatalf("NewScanner(): %v", err)
	}
	// Letter-heavy Base64 values with '/' separators trip both the wordlike and
	// separator benign heuristics; a validator/context-gated finding must still
	// survive the composite re-check instead of being reclassified benign.
	awsSecret := "abcdEFGH/ijklMNOP/qrstUVWX/yzABcdef/ghij" // 40 base64 chars
	azureKey := strings.Repeat("abcd/EFGH", 10) + "=="      // 90 base64 chars + padding
	for name, tc := range map[string]struct {
		text   string
		secret string
	}{
		"aws slash body":   {text: "aws_secret_access_key=" + awsSecret, secret: awsSecret},
		"azure slash body": {text: "AccountKey=" + azureKey + ";EndpointSuffix=core.windows.net", secret: azureKey},
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(tc.text, ContentText))
		if err != nil {
			t.Fatalf("%s Scan() error = %v", name, err)
		}
		assertFinding(t, findings, tc.secret)
	}
}

func TestBuiltinScannerFindsCryptoWalletMaterial(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	solana := testGenBase58Secret(t)
	evm := "4c0883a69102937d6231471b5dbb6204fe512961708279e4f4c0883a69102937"
	ed25519 := "[" + joinRepeatedInt(7, 64) + "]"
	// Canonical BIP39 test vector: 128-bit all-zero entropy, checksum word "about".
	mnemonic := "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"
	for name, tc := range map[string]struct {
		text       string
		secret     string
		rule       string
		confidence float64
	}{
		"solana base58": {text: "wallet keypair " + solana, secret: solana, rule: "solana-keypair-base58", confidence: 1.0},
		"evm hex":       {text: "private_key=" + evm, secret: evm, rule: "evm-private-key-hex", confidence: 0.90},
		"ed25519 array": {text: ed25519, secret: ed25519, rule: "ed25519-keypair-json-array", confidence: 0.98},
		"bip39":         {text: mnemonic, secret: mnemonic, rule: "bip39-mnemonic", confidence: 0.98},
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(tc.text, ContentText))
		if err != nil {
			t.Fatalf("%s Scan() error = %v", name, err)
		}
		assertFinding(t, findings, tc.secret)
		assertRuleAndConfidence(t, findings, tc.secret, tc.rule, tc.confidence)
	}
}

func TestCryptoWalletMaterialIgnoresFalsePositives(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	for _, s := range []string{
		"4c0883a69102937d6231471b5dbb6204fe512961708279e4f4c0883a69102937",
		"commit 5318b4d5bcd28de64ee5559e671353e16f075ecae9f99c7a79a38af5f869aa46",
		"sha256:0d17b565c37bcbd895e9d92315a05c1c3c9a29f762b011a10c54a66cd53c9b31",
		"9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM",
		"[" + joinRepeatedInt(300, 64) + "]",
		"[" + joinRepeatedInt(5, 32) + "]",
		"the quick brown fox jumps over the lazy sleeping dog again today",
		// 12 valid BIP39 words but an invalid checksum: ordinary prose must not
		// be redacted as a wallet mnemonic.
		"abandon ability able about above absent absorb abstract absurd abuse access accident",
	} {
		findings, err := scanner.Scan(context.Background(), testSegments(s, ContentText))
		if err != nil {
			t.Fatalf("Scan(%q) error = %v", s, err)
		}
		if len(findings) != 0 {
			t.Fatalf("Scan(%q) = %+v, want no findings", s, findings)
		}
	}
}

func TestBuiltinScannerFindsWholeObjectSecrets(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	gcpPrivateKeyEscaped := "-----BEGIN PRIVATE KEY-----\\nabc123fixture"
	gcpPrivateKeyDecoded := "-----BEGIN PRIVATE KEY-----\nabc123fixture"
	gcpJSON := `{"type":"service_account","project_id":"p","private_key":"` + gcpPrivateKeyEscaped + `"}`
	ethCiphertext := "7b2274657374223a22646c70227d00112233445566778899aabbccddeeff"
	ethJSON := `{"version":3,"crypto":{"ciphertext":"` + ethCiphertext + `","kdf":"scrypt"}}`
	for name, tc := range map[string]struct {
		segment    Segment
		secret     string
		rule       string
		confidence float64
	}{
		"gcp json string": {
			segment:    Segment{Path: "/messages/0/tool_calls/0/function/arguments", Value: gcpJSON, Kind: ToolArgs},
			secret:     gcpPrivateKeyDecoded,
			rule:       "gcp-service-account-json",
			confidence: 0.98,
		},
		"ethereum json string": {
			segment:    Segment{Path: "/messages/0/content", Value: ethJSON, Kind: ContentText},
			secret:     ethCiphertext,
			rule:       "ethereum-keystore-json",
			confidence: 0.98,
		},
		"gcp structured leaves": {
			segment:    Segment{Path: "/messages/0/content/0/input/private_key", Value: gcpPrivateKeyEscaped, Kind: ToolArgs},
			secret:     gcpPrivateKeyEscaped,
			rule:       "gcp-service-account-json",
			confidence: 0.98,
		},
	} {
		segs := []Segment{tc.segment}
		if name == "gcp structured leaves" {
			segs = append(segs, Segment{Path: "/messages/0/content/0/input/type", Value: "service_account", Kind: ToolArgs})
		}
		findings, err := scanner.Scan(context.Background(), segs)
		if err != nil {
			t.Fatalf("%s Scan() error = %v", name, err)
		}
		assertFinding(t, findings, tc.secret)
		assertRuleAndConfidence(t, findings, tc.secret, tc.rule, tc.confidence)
	}
}

func TestWholeObjectSecretsIgnoreFalsePositives(t *testing.T) {
	scanner := NewBuiltinScanner(20, 12)
	for _, segs := range [][]Segment{
		testSegments(`{"type":"service_account","private_key_id":"abc123"}`, ContentText),
		testSegments(`{"version":3,"crypto":{"ciphertext":"abc123","mac":"def456"}}`, ContentText),
		{{Path: "/tools/0/input_schema/private_key", Value: "-----BEGIN PRIVATE KEY-----\\nabc", Kind: ContentText}},
	} {
		findings, err := scanner.Scan(context.Background(), segs)
		if err != nil {
			t.Fatalf("Scan(%+v) error = %v", segs, err)
		}
		if len(findings) != 0 {
			t.Fatalf("Scan(%+v) = %+v, want no findings", segs, findings)
		}
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
