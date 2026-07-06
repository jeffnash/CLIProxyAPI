package secretdlp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	blconfig "github.com/betterleaks/betterleaks/config"
	bldetect "github.com/betterleaks/betterleaks/detect"
	blsources "github.com/betterleaks/betterleaks/sources"
)

type Finding struct {
	Secret     string
	RuleID     string
	Source     string
	Confidence float64
}

type Scanner interface {
	Scan(ctx context.Context, segs []Segment) ([]Finding, error)
}

type CompositeScanner struct {
	scanners []Scanner
	max      int
	minLen   int
}

func NewScanner(cfg Config) (Scanner, error) {
	cfg = normalizeConfig(cfg)
	var scanners []Scanner

	if strings.EqualFold(cfg.Scanner, "betterleaks") {
		bl, err := NewBetterLeaksScannerWithConfidence(cfg.MaxFindings, cfg.MinValueLength, cfg.BetterLeaksConfidence)
		if err != nil {
			return nil, err
		}
		scanners = append(scanners, bl)
	}

	scanners = append(scanners, NewBuiltinScannerWithOptions(cfg.MaxFindings, cfg.MinValueLength, cfg.HighEntropy))

	return &CompositeScanner{
		scanners: scanners,
		max:      cfg.MaxFindings,
		minLen:   cfg.MinValueLength,
	}, nil
}

func (s *CompositeScanner) Scan(ctx context.Context, segs []Segment) ([]Finding, error) {
	seen := make(map[string]Finding)
	for _, scanner := range s.scanners {
		findings, err := scanner.Scan(ctx, segs)
		if err != nil {
			return nil, err
		}
		for _, f := range findings {
			secret := strings.TrimSpace(f.Secret)
			if len(secret) < s.minLen || looksBenignCandidate(secret) {
				continue
			}
			confidence := confidenceForFinding(f)
			if existing, ok := seen[secret]; ok && existing.Confidence >= confidence {
				continue
			}
			seen[secret] = Finding{
				Secret:     secret,
				RuleID:     f.RuleID,
				Source:     f.Source,
				Confidence: confidence,
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
	}
	return findingsFromMap(seen), nil
}

type BetterLeaksScanner struct {
	mu          sync.Mutex
	cfg         *blconfig.Config
	maxFindings int
	minLen      int
	confidence  map[string]float64
}

func NewBetterLeaksScanner(maxFindings, minLen int) (*BetterLeaksScanner, error) {
	return NewBetterLeaksScannerWithConfidence(maxFindings, minLen, nil)
}

func NewBetterLeaksScannerWithConfidence(maxFindings, minLen int, confidence map[string]float64) (*BetterLeaksScanner, error) {
	cfg, err := blconfig.Default()
	if err != nil {
		return nil, err
	}
	return &BetterLeaksScanner{
		cfg:         cfg,
		maxFindings: maxFindings,
		minLen:      minLen,
		confidence:  cloneConfidenceMap(confidence),
	}, nil
}

func (s *BetterLeaksScanner) Scan(ctx context.Context, segs []Segment) ([]Finding, error) {
	body := virtualSegmentDocument(segs)
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, nil
	}

	// BetterLeaks lazily compiles expressions into detector/config state. Keep v1
	// conservative and serialize scans. Remove this once config immutability is guaranteed.
	s.mu.Lock()
	defer s.mu.Unlock()

	detector := bldetect.NewDetectorContext(ctx, s.cfg, bldetect.ValidationOptions{
		Enabled: false,
	})
	detector.SkipFindingAppend = true

	source := &blsources.Stdin{
		Content: bytes.NewReader(body),
		Attributes: map[string]string{
			blsources.AttrPath: "cliproxy-request.json",
		},
	}

	seen := make(map[string]Finding)
	for result := range detector.Run(ctx, source) {
		if result.Err != nil {
			return nil, result.Err
		}
		secret := strings.TrimSpace(result.Finding.Secret)
		if len(secret) < s.minLen || looksBenignCandidate(secret) {
			continue
		}
		confidence := betterLeaksConfidence(result.Finding.RuleID, s.confidence)
		if existing, ok := seen[secret]; ok && existing.Confidence >= confidence {
			continue
		}
		seen[secret] = Finding{
			Secret:     secret,
			RuleID:     result.Finding.RuleID,
			Source:     "betterleaks",
			Confidence: confidence,
		}
		if len(seen) >= s.maxFindings {
			break
		}
	}

	return findingsFromMap(seen), nil
}

type BuiltinScanner struct {
	max         int
	minLen      int
	highEntropy bool
}

func NewBuiltinScanner(maxFindings, minLen int) *BuiltinScanner {
	return &BuiltinScanner{max: maxFindings, minLen: minLen}
}

func NewBuiltinScannerWithOptions(maxFindings, minLen int, highEntropy bool) *BuiltinScanner {
	return &BuiltinScanner{max: maxFindings, minLen: minLen, highEntropy: highEntropy}
}

var (
	assignmentSecretPattern = regexp.MustCompile(`(?i)["']?(api[_-]?key|token|secret|password|passwd|pwd|database_url|db_url|auth|credential|webhook|signing[_-]?secret)["']?\s*[:=]\s*["']?([A-Za-z0-9._~:/+=,@%{}$!#?&-]{8,})`)
	openAIStyleKeyPattern   = regexp.MustCompile(`\bsk-[A-Za-z0-9]{28,64}\b`)
	// knownProviderKeyPattern covers distinctive, fixed provider prefixes that are
	// structurally impossible as identifiers. Safe bare, safe in the raw fallback.
	knownProviderKeyPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255}|glpat-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|npm_[A-Za-z0-9]{36}|pypi-[A-Za-z0-9_-]{50,}|rubygems_[A-Za-z0-9]{40,}|dckr_pat_[A-Za-z0-9_-]{20,}|atlasv1\.[A-Za-z0-9_-]{50,}|AKIA[0-9A-Z]{16}|ASIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|ya29\.[A-Za-z0-9_-]{20,}|1//[A-Za-z0-9_-]{20,}|hf_[A-Za-z0-9]{34,}|gsk_[A-Za-z0-9]{32,}|pplx-[A-Za-z0-9]{32,}|tgp_v1_[A-Za-z0-9_-]{20,}|co_[A-Za-z0-9]{32,}|r8_[A-Za-z0-9]{30,}|figd_[A-Za-z0-9_-]{20,}|dop_v1_[A-Za-z0-9]{40,}|whsec_[A-Za-z0-9]{24,}|rk_live_[A-Za-z0-9]{20,}|sq0(?:atp|csp)-[A-Za-z0-9_-]{20,}|access-(?:sandbox|development|production)-[A-Za-z0-9-]{20,}|SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|[0-9a-f]{32}-us\d{1,2}|SK[0-9a-fA-F]{32}|shp(?:at|pa|ss)_[a-fA-F0-9]{32})\b`)
	knownProviderURLPattern = regexp.MustCompile(`(?i)\bhttps://(?:hooks\.slack\.com/services/T[A-Z0-9]+/B[A-Z0-9]+/[A-Za-z0-9]+|(?:discord(?:app)?\.com)/api/webhooks/\d+/[\w-]+)\b`)
	discordBotTokenPattern  = regexp.MustCompile(`\b(?:mfa\.[A-Za-z0-9_-]{20,}|[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{27,})\b`)
	// bare-credential shapes: context-free by design (real tokens are pasted bare),
	// but shape-strict so identifiers cannot satisfy them. Entropy/structure are
	// internal validators here (isBarePrefixedCredential / isBareHexPairCredential),
	// not a generic entropy rule; SECRET_DLP_HIGH_ENTROPY does not gate these.
	barePrefixedTokenPattern      = regexp.MustCompile(`\b[a-z][a-z0-9]{1,5}[-_][A-Za-z0-9]{30,64}\b`)
	bareHexPairTokenPattern       = regexp.MustCompile(`\b[0-9a-f]{24,64}\.[A-Za-z0-9_-]{12,64}\b`)
	bareTokenSeparator            = regexp.MustCompile(`[-_]`)
	solanaKeypairPattern          = regexp.MustCompile(`\b[1-9A-HJ-NP-Za-km-z]{86,88}\b`)
	evmPrivateKeyPattern          = regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{64}\b`)
	ed25519JSONArrayPattern       = regexp.MustCompile(`\[(?:\s*\d{1,3}\s*,){63}\s*\d{1,3}\s*\]`)
	bip39WordRun                  = regexp.MustCompile(`(?:\b[a-z]+\b[ \t]+){11,23}\b[a-z]+\b`)
	awsSecretAccessKeyPattern     = regexp.MustCompile(`\b[A-Za-z0-9/+=]{40}\b`)
	azureStorageAccountKeyPattern = regexp.MustCompile(`(?i)\bAccountKey=([A-Za-z0-9+/]{80,}={0,2})`)
	basicAuthPattern              = regexp.MustCompile(`(?i)\bBasic\s+([A-Za-z0-9+/]{12,}={0,2})`)
	pgpPrivateKeyPattern          = regexp.MustCompile(`-----BEGIN PGP PRIVATE KEY BLOCK-----[\s\S]+?-----END PGP PRIVATE KEY BLOCK-----`)
	// puttyPrivateKeyPattern must span the whole PPK block through Private-MAC so
	// the private key lines (which follow the Private-Lines: header) are inside
	// the redacted span; anchoring on Private-Lines:/Private-MAC: alone stops
	// before the private base64 and lets it egress after the placeholder.
	puttyPrivateKeyPattern = regexp.MustCompile(`(?m)^PuTTY-User-Key-File-[23]: [^\n]+[\s\S]+?^Private-MAC: [^\n]+`)
	ageSecretKeyPattern    = regexp.MustCompile(`\bAGE-SECRET-KEY-1[0-9A-Z]{40,}\b`)
	distinctSKPattern      = regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_-]{20,}|sk-proj-[A-Za-z0-9_-]{20,}|sk-or-v1-[A-Za-z0-9_-]{20,}|sk_live_[A-Za-z0-9_-]{20,})\b`)
	dsnUserInfoPattern     = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:([^@\s/?#]+)@[^/\s]+`)
	bearerPattern          = regexp.MustCompile(`(?i)\bBearer\s+([A-Za-z0-9._~+/=-]{16,})`)
	jwtPattern             = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	pemPrivateKeyPattern   = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]+?-----END [A-Z ]*PRIVATE KEY-----`)
	highEntropyPattern     = regexp.MustCompile(`\b[A-Za-z0-9._~+/=-]{24,}\b`)
	commitSHAPattern       = regexp.MustCompile(`^[a-f0-9]{40}$`)
	uuidPattern            = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	// Full-string shapes for the values addRaw stores for the validator/context
	// gated raw credential rules. They let isExplicitSecretShape exempt an
	// already-validated detection from the generic benign heuristics.
	awsSecretAccessKeyValuePattern     = regexp.MustCompile(`^[A-Za-z0-9/+=]{40}$`)
	azureStorageAccountKeyValuePattern = regexp.MustCompile(`^[A-Za-z0-9+/]{80,}={0,2}$`)
	basicAuthValuePattern              = regexp.MustCompile(`^[A-Za-z0-9+/]{12,}={0,2}$`)
)

func (s *BuiltinScanner) Scan(ctx context.Context, segs []Segment) ([]Finding, error) {
	_ = ctx
	seen := make(map[string]Finding)

	add := func(secret, rule string, confidence float64) {
		secret = strings.Trim(secret, `"'`)
		secret = strings.TrimSpace(secret)
		if len(secret) < s.minLen || looksBenignCandidate(secret) {
			return
		}
		// A value can match several rules (e.g. ghp_... hits both
		// known-provider-key and bare-credential-prefixed); keep the
		// highest-confidence attribution regardless of loop order.
		if existing, ok := seen[secret]; ok && existing.Confidence >= confidence {
			return
		}
		seen[secret] = Finding{Secret: secret, RuleID: rule, Source: "builtin", Confidence: confidence}
	}
	addRaw := func(secret, rule string, confidence float64) {
		secret = strings.TrimSpace(secret)
		if len(secret) < s.minLen {
			return
		}
		if existing, ok := seen[secret]; ok && existing.Confidence >= confidence {
			return
		}
		seen[secret] = Finding{Secret: secret, RuleID: rule, Source: "builtin", Confidence: confidence}
	}

	// Accumulate cross-segment structured-object groups (GCP service account,
	// Ethereum keystore) during this single pass instead of a second traversal.
	gcpGroups := make(map[string]gcpSegmentGroup)
	ethGroups := make(map[string]ethSegmentGroup)
	for _, seg := range segs {
		text := scanTextForSegment(seg)
		accumulateStructuredSegment(seg, gcpGroups, ethGroups)
		// The whole-object detectors key on literal JSON field names; skip the
		// JSON decode entirely when neither field can possibly be present.
		if strings.Contains(text, "private_key") || strings.Contains(text, "ciphertext") {
			for _, f := range structuredObjectFindingsFromText(seg, text) {
				addRaw(f.Secret, f.RuleID, f.Confidence)
				if len(seen) >= s.max {
					return findingsFromMap(seen), nil
				}
			}
		}
		for _, m := range assignmentSecretPattern.FindAllStringSubmatch(text, -1) {
			if len(m) >= 3 {
				add(m[2], "assignment-context", 0.85)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range bearerPattern.FindAllStringSubmatch(text, -1) {
			if len(m) >= 2 {
				add(m[1], "bearer-token", 0.90)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range dsnUserInfoPattern.FindAllStringSubmatch(text, -1) {
			if len(m) >= 2 {
				add(m[1], "dsn-userinfo", 0.90)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range distinctSKPattern.FindAllString(text, -1) {
			add(m, "sk-distinct", 0.98)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range openAIStyleKeyPattern.FindAllString(text, -1) {
			add(m, "openai-sk", 0.95)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range knownProviderKeyPattern.FindAllString(text, -1) {
			add(m, "known-provider-key", 0.95)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range knownProviderURLPattern.FindAllString(text, -1) {
			addRaw(m, "known-provider-key", 0.95)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range discordBotTokenPattern.FindAllString(text, -1) {
			if isDiscordBotToken(m) {
				add(m, "known-provider-key", 0.95)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range barePrefixedTokenPattern.FindAllString(text, -1) {
			if isBarePrefixedCredential(m) {
				add(m, "bare-credential-prefixed", 0.85)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range bareHexPairTokenPattern.FindAllString(text, -1) {
			if isBareHexPairCredential(m) {
				add(m, "bare-credential-hexpair", 0.85)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range basicAuthPattern.FindAllStringSubmatch(text, -1) {
			if len(m) >= 2 && isBasicAuthCredential(m[1]) {
				addRaw(m[1], "basic-auth", 0.90)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, loc := range awsSecretAccessKeyPattern.FindAllStringIndex(text, -1) {
			if hasAWSSecretKeywordNear(text, loc[0], loc[1]) {
				addRaw(text[loc[0]:loc[1]], "aws-secret-access-key", 0.90)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range azureStorageAccountKeyPattern.FindAllStringSubmatch(text, -1) {
			if len(m) >= 2 {
				addRaw(m[1], "azure-storage-account-key", 0.90)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		// A base58 secret key is an 86-88 char run; skip the regex and the
		// big.Int decode length check when the segment is shorter than that.
		if len(text) >= 86 {
			for _, m := range solanaKeypairPattern.FindAllString(text, -1) {
				if isBase58SecretKey(m) {
					addRaw(m, "solana-keypair-base58", 1.0)
				}
				if len(seen) >= s.max {
					return findingsFromMap(seen), nil
				}
			}
		}
		for _, loc := range evmPrivateKeyPattern.FindAllStringIndex(text, -1) {
			if hasKeyMaterialKeywordNear(text, loc[0], loc[1]) {
				addRaw(text[loc[0]:loc[1]], "evm-private-key-hex", 0.90)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range ed25519JSONArrayPattern.FindAllString(text, -1) {
			if isEd25519SecretArray(m) {
				addRaw(m, "ed25519-keypair-json-array", 0.98)
			}
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		// A mnemonic needs >=12 whitespace-separated words; skip the run regex
		// and per-candidate checksum work when the segment cannot hold one.
		if atLeastWordSeparators(text, 11) {
			for _, m := range findBIP39Mnemonics(text) {
				addRaw(m, "bip39-mnemonic", 0.98)
				if len(seen) >= s.max {
					return findingsFromMap(seen), nil
				}
			}
		}
		for _, m := range jwtPattern.FindAllString(text, -1) {
			add(m, "jwt", 0.90)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range pemPrivateKeyPattern.FindAllString(text, -1) {
			add(m, "pem-private-key", 1.0)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range pgpPrivateKeyPattern.FindAllString(text, -1) {
			addRaw(m, "pem-private-key", 1.0)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range puttyPrivateKeyPattern.FindAllString(text, -1) {
			addRaw(m, "pem-private-key", 1.0)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		for _, m := range ageSecretKeyPattern.FindAllString(text, -1) {
			addRaw(m, "age-secret-key", 1.0)
			if len(seen) >= s.max {
				return findingsFromMap(seen), nil
			}
		}
		if s.highEntropy {
			for _, loc := range highEntropyPattern.FindAllStringIndex(text, -1) {
				m := text[loc[0]:loc[1]]
				if isHighEntropyCandidate(seg, text, loc[0], loc[1], m) {
					add(m, "high-entropy", 0.70)
				}
				if len(seen) >= s.max {
					break
				}
			}
		}
	}
	for _, f := range resolveStructuredSegmentGroups(gcpGroups, ethGroups) {
		addRaw(f.Secret, f.RuleID, f.Confidence)
		if len(seen) >= s.max {
			return findingsFromMap(seen), nil
		}
	}

	return findingsFromMap(seen), nil
}

func scanTextForSegment(seg Segment) string {
	key := segmentSecretContextKey(seg)
	if key == "" {
		return seg.Value
	}
	return key + "=" + seg.Value
}

func segmentSecretContextKey(seg Segment) string {
	if seg.Kind != ToolArgs && seg.Kind != SecretField {
		return ""
	}
	key := jsonPointerLast(seg.Path)
	if !isSecretDLPContextKey(key) {
		return ""
	}
	return key
}

func jsonPointerLast(pointer string) string {
	if pointer == "" {
		return ""
	}
	idx := strings.LastIndex(pointer, "/")
	if idx >= 0 {
		pointer = pointer[idx+1:]
	}
	pointer = strings.ReplaceAll(pointer, "~1", "/")
	pointer = strings.ReplaceAll(pointer, "~0", "~")
	return pointer
}

func isSecretDLPContextKey(key string) bool {
	key = normalizeJSONPathKey(key)
	compact := strings.ReplaceAll(key, "_", "")
	return strings.Contains(key, "api_key") ||
		strings.Contains(compact, "apikey") ||
		strings.Contains(key, "token") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "password") ||
		strings.Contains(key, "passwd") ||
		key == "pwd" ||
		strings.Contains(key, "credential") ||
		strings.Contains(key, "webhook") ||
		strings.Contains(key, "database_url") ||
		strings.Contains(key, "db_url")
}

func virtualSegmentDocument(segs []Segment) []byte {
	if len(segs) == 0 {
		return nil
	}
	var b strings.Builder
	for _, seg := range segs {
		text := scanTextForSegment(seg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	return []byte(b.String())
}

func confidenceForFinding(f Finding) float64 {
	if f.Confidence > 0 {
		return f.Confidence
	}
	return confidenceForRule(f.RuleID)
}

func confidenceForRule(ruleID string) float64 {
	switch strings.ToLower(ruleID) {
	case "pem-private-key", "age-secret-key":
		return 1.0
	case "solana-keypair-base58":
		return 1.0
	case "sk-distinct", "ed25519-keypair-json-array", "bip39-mnemonic", "gcp-service-account-json", "ethereum-keystore-json":
		return 0.98
	case "openai-sk", "openai-style-key", "known-provider-key":
		return 0.95
	case "bearer-token", "jwt", "dsn-userinfo", "basic-auth", "aws-secret-access-key", "azure-storage-account-key", "evm-private-key-hex":
		return 0.90
	case "assignment-context", "bare-credential-prefixed", "bare-credential-hexpair":
		return 0.85
	case "high-entropy":
		return 0.70
	default:
		return 0.40
	}
}

func cloneConfidenceMap(in map[string]float64) map[string]float64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]float64, len(in))
	for ruleID, confidence := range in {
		if ruleID == "" || confidence <= 0 || confidence > 1 {
			continue
		}
		out[ruleID] = confidence
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func betterLeaksConfidence(ruleID string, confidence map[string]float64) float64 {
	if configured, ok := confidence[ruleID]; ok {
		return configured
	}
	lower := strings.ToLower(ruleID)
	if strings.Contains(lower, "entropy") || strings.Contains(lower, "generic") {
		return 0.70
	}
	return 0.40
}

func filterFindings(findings []Finding, ids IdentifierSet, cfg Config) (accepted []Finding, shadow []Finding) {
	cfg = normalizeConfig(cfg)
	for _, f := range findings {
		secret := strings.TrimSpace(f.Secret)
		if secret == "" || looksBenignCandidate(secret) {
			continue
		}
		f.Secret = secret
		f.Confidence = confidenceForFinding(f)
		if ids.containsOrEmbeds(secret) && !credentialFindingOverridesIdentifier(f) {
			continue
		}
		if f.Confidence >= cfg.RedactThreshold {
			accepted = append(accepted, f)
		} else {
			shadow = append(shadow, f)
		}
	}
	return findingsFromMap(findingsBySecret(accepted)), findingsFromMap(findingsBySecret(shadow))
}

func credentialFindingOverridesIdentifier(f Finding) bool {
	switch strings.ToLower(f.RuleID) {
	case "pem-private-key",
		"age-secret-key",
		"sk-distinct",
		"openai-sk",
		"openai-style-key",
		"known-provider-key",
		"bearer-token",
		"jwt",
		"dsn-userinfo",
		"basic-auth",
		"aws-secret-access-key",
		"azure-storage-account-key",
		"solana-keypair-base58",
		"ed25519-keypair-json-array",
		"evm-private-key-hex",
		"bip39-mnemonic",
		"gcp-service-account-json",
		"ethereum-keystore-json",
		"assignment-context",
		"bare-credential-prefixed",
		"bare-credential-hexpair":
		return f.Confidence >= 0.80
	default:
		return false
	}
}

func findingsBySecret(findings []Finding) map[string]Finding {
	out := make(map[string]Finding, len(findings))
	for _, f := range findings {
		if existing, ok := out[f.Secret]; ok && existing.Confidence >= f.Confidence {
			continue
		}
		out[f.Secret] = f
	}
	return out
}

func explicitRawFindings(body []byte, minLen int, maxFindings int) []Finding {
	if len(body) == 0 || maxFindings <= 0 {
		return nil
	}
	hasSK := bytes.Contains(body, []byte("sk-")) || bytes.Contains(body, []byte("sk_"))
	hasJWT := bytes.Contains(body, []byte("eyJ"))
	hasPEM := bytes.Contains(body, []byte("-----BEGIN "))
	// PuTTY .ppk files carry no -----BEGIN marker, so the raw fallback needs a
	// dedicated anchor or it skips PuTTY keys on raw-only / malformed-JSON paths.
	hasPuTTY := bytes.Contains(body, []byte("PuTTY-User-Key-File-"))
	hasAge := bytes.Contains(body, []byte("AGE-SECRET-KEY-1"))
	hasDSN := bytes.Contains(body, []byte("://"))
	hasKnown := hasKnownProviderAnchor(body)
	hasBasic := bytes.Contains(body, []byte("Basic ")) || bytes.Contains(body, []byte("basic "))
	if !hasSK && !hasJWT && !hasPEM && !hasPuTTY && !hasAge && !hasDSN && !hasKnown && !hasBasic {
		return nil
	}

	text := string(body)
	seen := make(map[string]Finding)
	remaining := func() int {
		n := maxFindings - len(seen)
		if n < 0 {
			return 0
		}
		return n
	}
	add := func(secret, rule string, confidence float64) {
		secret = strings.TrimSpace(strings.Trim(secret, `"'`))
		if len(secret) < minLen || looksBenignCandidate(secret) {
			return
		}
		if existing, ok := seen[secret]; ok && existing.Confidence >= confidence {
			return
		}
		seen[secret] = Finding{Secret: secret, RuleID: rule, Source: "builtin", Confidence: confidence}
	}
	if hasSK {
		for _, m := range distinctSKPattern.FindAllString(text, remaining()) {
			add(m, "sk-distinct", 0.98)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
		for _, m := range openAIStyleKeyPattern.FindAllString(text, remaining()) {
			add(m, "openai-sk", 0.95)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
	}
	if hasKnown {
		for _, m := range knownProviderKeyPattern.FindAllString(text, remaining()) {
			add(m, "known-provider-key", 0.95)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
		for _, m := range knownProviderURLPattern.FindAllString(text, remaining()) {
			add(m, "known-provider-key", 0.95)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
		for _, m := range discordBotTokenPattern.FindAllString(text, remaining()) {
			if isDiscordBotToken(m) {
				add(m, "known-provider-key", 0.95)
			}
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
	}
	// bare-credential-prefixed / bare-credential-hexpair intentionally do NOT
	// run here: the raw fallback scans control-plane bytes, and those shapes
	// are the only ones loose enough to conceivably collide with an unusual
	// tool or resource name. They run on extracted segments only.
	if hasJWT {
		for _, m := range jwtPattern.FindAllString(text, remaining()) {
			add(m, "jwt", 0.90)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
	}
	if hasPEM {
		for _, m := range pemPrivateKeyPattern.FindAllString(text, remaining()) {
			add(m, "pem-private-key", 1.0)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
		for _, m := range pgpPrivateKeyPattern.FindAllString(text, remaining()) {
			add(m, "pem-private-key", 1.0)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
	}
	if hasPuTTY {
		for _, m := range puttyPrivateKeyPattern.FindAllString(text, remaining()) {
			add(m, "pem-private-key", 1.0)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
	}
	if hasAge {
		for _, m := range ageSecretKeyPattern.FindAllString(text, remaining()) {
			add(m, "age-secret-key", 1.0)
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
	}
	if hasDSN {
		for _, m := range dsnUserInfoPattern.FindAllStringSubmatch(text, remaining()) {
			if len(m) >= 2 {
				add(m[1], "dsn-userinfo", 0.90)
			}
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
	}
	if hasBasic {
		for _, m := range basicAuthPattern.FindAllStringSubmatch(text, remaining()) {
			if len(m) >= 2 && isBasicAuthCredential(m[1]) {
				add(m[1], "basic-auth", 0.90)
			}
			if len(seen) >= maxFindings {
				return findingsFromMap(seen)
			}
		}
	}
	return findingsFromMap(seen)
}

var knownProviderAnchors = [][]byte{
	[]byte("ghp_"), []byte("gho_"), []byte("ghu_"), []byte("ghs_"), []byte("ghr_"),
	[]byte("github_pat_"), []byte("glpat-"), []byte("xoxb-"), []byte("xoxa-"),
	[]byte("xoxp-"), []byte("xoxr-"), []byte("xoxs-"), []byte("npm_"),
	[]byte("pypi-"), []byte("rubygems_"), []byte("dckr_pat_"), []byte("atlasv1."),
	[]byte("AKIA"), []byte("ASIA"), []byte("AIza"), []byte("ya29."), []byte("1//"),
	[]byte("hf_"), []byte("gsk_"), []byte("pplx-"), []byte("tgp_v1_"), []byte("co_"),
	[]byte("r8_"), []byte("figd_"), []byte("dop_v1_"), []byte("whsec_"), []byte("rk_live_"),
	[]byte("sq0atp-"), []byte("sq0csp-"), []byte("access-sandbox-"), []byte("access-development-"),
	[]byte("access-production-"), []byte("SG."), []byte("AGE-SECRET-KEY-1"),
	[]byte("shpat_"), []byte("shppa_"), []byte("shpss_"),
	[]byte("hooks.slack.com/services/"), []byte("discord.com/api/webhooks/"), []byte("discordapp.com/api/webhooks/"),
	[]byte("mfa."), []byte("PuTTY-User-Key-File-"),
}

func hasKnownProviderAnchor(body []byte) bool {
	for _, anchor := range knownProviderAnchors {
		if bytes.Contains(body, anchor) {
			return true
		}
	}
	return false
}

func isHighEntropyCandidate(seg Segment, text string, start, end int, value string) bool {
	if len(value) < 32 || shannonEntropy(value) < 3.7 {
		return false
	}
	if wordlikeRatio(value) >= 0.5 {
		return false
	}
	if separatorPartCount(value) > 2 && !jwtPattern.MatchString(value) {
		return false
	}
	if seg.Kind != SecretField && !hasSecretKeywordNear(text, start, end) {
		return false
	}
	return true
}

func hasSecretKeywordNear(text string, start, end int) bool {
	left := start - 64
	if left < 0 {
		left = 0
	}
	right := end + 64
	if right > len(text) {
		right = len(text)
	}
	window := strings.ToLower(text[left:right])
	for _, keyword := range []string{"api_key", "apikey", "token", "secret", "password", "passwd", "pwd", "database_url", "db_url", "auth", "credential", "webhook", "signing_secret"} {
		if strings.Contains(window, keyword) {
			return true
		}
	}
	return false
}

// hashAlgoPrefixes guards against SRI/digest strings like "sha256-<alnum run>"
// matching the bare prefixed-token shape.
var hashAlgoPrefixes = map[string]struct{}{
	"sha1": {}, "sha224": {}, "sha256": {}, "sha384": {}, "sha512": {},
	"md5": {}, "crc32": {}, "xxh64": {}, "blake3": {},
}

// isBarePrefixedCredential validates a barePrefixedTokenPattern match. The
// pattern supplies the credential shape (short lowercase tag + separator +
// long separator-free alnum body); the validators require the body to look
// machine-generated: letters AND >=4 digits, >=5 letter<->digit transitions
// (kills word concatenations and bunched timestamps), and entropy as a
// supporting signal only. Cloud resource IDs (subnet-, ami-, pod hashes) are
// excluded by the 30-char body floor, not by these checks.
func isBarePrefixedCredential(m string) bool {
	loc := bareTokenSeparator.FindStringIndex(m)
	if loc == nil {
		return false
	}
	prefix := strings.ToLower(m[:loc[0]])
	body := m[loc[1]:]
	if _, ok := hashAlgoPrefixes[prefix]; ok {
		return false
	}
	letters, digits := 0, 0
	for _, r := range body {
		switch {
		case r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z':
			letters++
		case r >= '0' && r <= '9':
			digits++
		}
	}
	if letters < 1 || digits < 4 {
		return false
	}
	if letterDigitTransitions(body) < 5 {
		return false
	}
	return shannonEntropy(body) >= 3.2
}

// isBareHexPairCredential validates a bareHexPairTokenPattern match
// (<hex id>.<secret tail>). The head must be true mixed hex (kills long
// decimals and letter runs); the tail must contain a digit or uppercase
// (kills "<hash>.configuration"-style word tails). Filename suffixes like
// .tar/.min/.js are already excluded by the 12-char tail floor.
func isBareHexPairCredential(m string) bool {
	idx := strings.IndexByte(m, '.')
	if idx <= 0 || idx+1 >= len(m) {
		return false
	}
	head, tail := m[:idx], m[idx+1:]
	headLetters, headDigits := 0, 0
	for _, r := range head {
		switch {
		case r >= 'a' && r <= 'f':
			headLetters++
		case r >= '0' && r <= '9':
			headDigits++
		}
	}
	if headLetters < 1 || headDigits < 1 {
		return false
	}
	for _, r := range tail {
		if r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' {
			return true
		}
	}
	return false
}

func letterDigitTransitions(s string) int {
	t := 0
	prevIsLetter := false
	prevSet := false
	for _, r := range s {
		isLetter := r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
		isDigit := r >= '0' && r <= '9'
		if !isLetter && !isDigit {
			prevSet = false
			continue
		}
		if prevSet && prevIsLetter != isLetter {
			t++
		}
		prevIsLetter = isLetter
		prevSet = true
	}
	return t
}

type structuredSecretFinding struct {
	Secret     string
	RuleID     string
	Confidence float64
}

func structuredObjectFindingsFromText(seg Segment, text string) []structuredSecretFinding {
	if !structuredObjectSegmentKind(seg.Kind) {
		return nil
	}
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "{") {
		return nil
	}
	var value any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return structuredObjectFindingsFromMap(obj)
}

func structuredObjectSegmentKind(kind SegKind) bool {
	return kind == ContentText || kind == ToolArgs || kind == ToolResult || kind == SecretField
}

func structuredObjectFindingsFromMap(obj map[string]any) []structuredSecretFinding {
	var out []structuredSecretFinding
	if privateKey, ok := gcpServiceAccountPrivateKey(obj); ok {
		out = appendStructuredSecretFinding(out, privateKey, "gcp-service-account-json", 0.98)
	}
	if ciphertext, ok := ethereumKeystoreCiphertext(obj); ok {
		out = appendStructuredSecretFinding(out, ciphertext, "ethereum-keystore-json", 0.98)
	}
	return out
}

func appendStructuredSecretFinding(out []structuredSecretFinding, secret, rule string, confidence float64) []structuredSecretFinding {
	out = append(out, structuredSecretFinding{
		Secret:     secret,
		RuleID:     rule,
		Confidence: confidence,
	})
	if encoded, ok := jsonStringContent(secret); ok && encoded != secret {
		out = append(out, structuredSecretFinding{
			Secret:     encoded,
			RuleID:     rule,
			Confidence: confidence,
		})
	}
	return out
}

func jsonStringContent(value string) (string, bool) {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) < 2 {
		return "", false
	}
	return string(encoded[1 : len(encoded)-1]), true
}

func gcpServiceAccountPrivateKey(obj map[string]any) (string, bool) {
	if typ, _ := obj["type"].(string); typ != "service_account" {
		return "", false
	}
	privateKey, _ := obj["private_key"].(string)
	if !strings.Contains(privateKey, "-----BEGIN") {
		return "", false
	}
	return privateKey, true
}

func ethereumKeystoreCiphertext(obj map[string]any) (string, bool) {
	cryptoObj, _ := obj["crypto"].(map[string]any)
	if cryptoObj == nil {
		cryptoObj, _ = obj["Crypto"].(map[string]any)
	}
	if cryptoObj == nil {
		return "", false
	}
	ciphertext, _ := cryptoObj["ciphertext"].(string)
	kdf, _ := cryptoObj["kdf"].(string)
	if len(ciphertext) < 32 || kdf == "" {
		return "", false
	}
	return ciphertext, true
}

type gcpSegmentGroup struct {
	hasServiceAccount bool
	privateKey        string
}

type ethSegmentGroup struct {
	ciphertext string
	kdf        string
}

// accumulateStructuredSegment folds a single segment into the cross-segment
// GCP/Ethereum groups so whole-object detection can share the main scan pass
// instead of requiring a second traversal of every segment.
func accumulateStructuredSegment(seg Segment, gcp map[string]gcpSegmentGroup, eth map[string]ethSegmentGroup) {
	if !structuredObjectSegmentKind(seg.Kind) {
		return
	}
	key := normalizeJSONPathKey(jsonPointerLast(seg.Path))
	parent := jsonPointerParent(seg.Path)
	switch key {
	case "type":
		if seg.Value == "service_account" {
			group := gcp[parent]
			group.hasServiceAccount = true
			gcp[parent] = group
		}
	case "private_key":
		if strings.Contains(seg.Value, "-----BEGIN") {
			group := gcp[parent]
			group.privateKey = seg.Value
			gcp[parent] = group
		}
	case "ciphertext":
		if strings.HasSuffix(normalizeJSONPointer(parent), "/crypto") {
			group := eth[jsonPointerParent(parent)]
			group.ciphertext = seg.Value
			eth[jsonPointerParent(parent)] = group
		}
	case "kdf":
		if strings.HasSuffix(normalizeJSONPointer(parent), "/crypto") {
			group := eth[jsonPointerParent(parent)]
			group.kdf = seg.Value
			eth[jsonPointerParent(parent)] = group
		}
	}
}

func resolveStructuredSegmentGroups(gcp map[string]gcpSegmentGroup, eth map[string]ethSegmentGroup) []structuredSecretFinding {
	var out []structuredSecretFinding
	for _, group := range gcp {
		if group.hasServiceAccount && group.privateKey != "" {
			out = append(out, structuredSecretFinding{
				Secret:     group.privateKey,
				RuleID:     "gcp-service-account-json",
				Confidence: 0.98,
			})
		}
	}
	for _, group := range eth {
		if len(group.ciphertext) >= 32 && group.kdf != "" {
			out = append(out, structuredSecretFinding{
				Secret:     group.ciphertext,
				RuleID:     "ethereum-keystore-json",
				Confidence: 0.98,
			})
		}
	}
	return out
}

// atLeastWordSeparators reports whether text holds at least n space/tab
// separators, a cheap necessary condition for an n+1 word BIP39 mnemonic run.
func atLeastWordSeparators(text string, n int) bool {
	count := 0
	for i := 0; i < len(text); i++ {
		if text[i] == ' ' || text[i] == '\t' {
			count++
			if count >= n {
				return true
			}
		}
	}
	return false
}

func jsonPointerParent(pointer string) string {
	if pointer == "" {
		return ""
	}
	idx := strings.LastIndex(pointer, "/")
	if idx <= 0 {
		return ""
	}
	return pointer[:idx]
}

func normalizeJSONPointer(pointer string) string {
	if pointer == "" {
		return ""
	}
	parts := strings.Split(pointer, "/")
	for i := range parts {
		parts[i] = normalizeJSONPathKey(parts[i])
	}
	return strings.Join(parts, "/")
}

func isDiscordBotToken(token string) bool {
	if strings.HasPrefix(token, "mfa.") {
		return true
	}
	if strings.HasPrefix(token, "eyJ") {
		return false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(decoded) == 0 {
		return false
	}
	for _, b := range decoded {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

func isBasicAuthCredential(encoded string) bool {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	return len(parts) == 2 && strings.TrimSpace(parts[0]) != "" && len(strings.TrimSpace(parts[1])) >= 8
}

func hasAWSSecretKeywordNear(text string, start, end int) bool {
	window := nearbyLower(text, start, end, 64)
	return strings.Contains(window, "aws_secret_access_key") ||
		strings.Contains(window, "secretaccesskey") ||
		strings.Contains(window, "secret_access_key") ||
		strings.Contains(window, "aws secret access key")
}

func hasKeyMaterialKeywordNear(text string, start, end int) bool {
	window := nearbyLower(text, start, end, 64)
	for _, kw := range keyMaterialKeywords {
		if strings.Contains(window, kw) {
			return true
		}
	}
	return false
}

func nearbyLower(text string, start, end, width int) string {
	left := start - width
	if left < 0 {
		left = 0
	}
	right := end + width
	if right > len(text) {
		right = len(text)
	}
	return strings.ToLower(text[left:right])
}

var keyMaterialKeywords = []string{
	"private_key", "privatekey", "privkey", "priv_key", "private key",
	"secret_key", "secretkey", "mnemonic", "seed_phrase", "seedphrase",
	"seed phrase", "recovery phrase", "wallet", "keypair", "signer",
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var base58Index = func() [128]int8 {
	var idx [128]int8
	for i := range idx {
		idx[i] = -1
	}
	for i, c := range base58Alphabet {
		idx[c] = int8(i)
	}
	return idx
}()

func base58DecodedLen(s string) int {
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	num := new(big.Int)
	fiftyEight := big.NewInt(58)
	for i := zeros; i < len(s); i++ {
		c := s[i]
		if c >= 128 || base58Index[c] < 0 {
			return -1
		}
		num.Mul(num, fiftyEight)
		num.Add(num, big.NewInt(int64(base58Index[c])))
	}
	return zeros + len(num.Bytes())
}

func isBase58SecretKey(s string) bool {
	return base58DecodedLen(s) == 64
}

func isEd25519SecretArray(s string) bool {
	count := 0
	for _, f := range strings.Split(strings.Trim(s, "[]"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n < 0 || n > 255 {
			return false
		}
		count++
	}
	return count == 64
}

func findBIP39Mnemonics(text string) []string {
	if len(bip39Wordlist) == 0 {
		return nil
	}
	var out []string
	for _, cand := range bip39WordRun.FindAllString(text, -1) {
		words := strings.Fields(cand)
		if isBIP39MnemonicWords(words) {
			out = append(out, strings.Join(words, " "))
			continue
		}
		out = append(out, validMnemonicSubruns(words)...)
	}
	return out
}

func validMnemonicSubruns(words []string) []string {
	var out []string
	for _, length := range []int{24, 21, 18, 15, 12} {
		for i := 0; i+length <= len(words); i++ {
			if isBIP39MnemonicWords(words[i : i+length]) {
				out = append(out, strings.Join(words[i:i+length], " "))
				i += length - 1
			}
		}
	}
	return out
}

func isBIP39Mnemonic(s string) bool {
	return isBIP39MnemonicWords(strings.Fields(s))
}

func isBIP39MnemonicWords(words []string) bool {
	switch len(words) {
	case 12, 15, 18, 21, 24:
	default:
		return false
	}
	// Each word encodes 11 bits: the concatenation is ENT entropy bits followed
	// by CS checksum bits, where CS = ENT/32 and ENT+CS = 11*len(words). Rebuild
	// the entropy, then require the trailing CS bits to equal the leading CS bits
	// of SHA-256(entropy). A list of in-wordlist words with a bad checksum (e.g.
	// ordinary prose) is rejected here.
	totalBits := len(words) * 11
	csBits := totalBits / 33
	entBits := totalBits - csBits
	entropy := make([]byte, entBits/8)
	var checksum uint16
	for wi, w := range words {
		idx, ok := bip39Wordlist[w]
		if !ok {
			return false
		}
		for b := 0; b < 11; b++ {
			bit := (idx >> uint(10-b)) & 1
			if pos := wi*11 + b; pos < entBits {
				if bit == 1 {
					entropy[pos/8] |= 1 << uint(7-(pos%8))
				}
			} else {
				checksum = checksum<<1 | uint16(bit)
			}
		}
	}
	sum := sha256.Sum256(entropy)
	return checksum == uint16(sum[0]>>uint(8-csBits))
}

func findingsFromMap(m map[string]Finding) []Finding {
	out := make([]Finding, 0, len(m))
	for _, f := range m {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		return len(out[i].Secret) > len(out[j].Secret)
	})
	return out
}

func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	counts := make(map[rune]int)
	for _, r := range s {
		counts[r]++
	}
	var entropy float64
	length := float64(len([]rune(s)))
	for _, count := range counts {
		p := float64(count) / length
		entropy -= p * math.Log2(p)
	}
	return entropy
}

func looksBenignCandidate(s string) bool {
	if strings.HasPrefix(s, "__CPA_DLP_v1_") {
		return true
	}

	lower := strings.ToLower(s)

	if (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) && !knownProviderURLPattern.MatchString(s) {
		return true
	}

	if strings.Contains(lower, "node_modules/") ||
		strings.Contains(lower, "package-lock") ||
		strings.Contains(lower, "pnpm-lock") ||
		strings.Contains(lower, "yarn.lock") {
		return true
	}

	if commitSHAPattern.MatchString(lower) {
		return true
	}

	if uuidPattern.MatchString(lower) {
		return true
	}

	if !isExplicitSecretShape(s) {
		if wordlikeRatio(s) >= 0.6 {
			return true
		}
		if separatorPartCount(s) >= 3 && !jwtPattern.MatchString(s) {
			return true
		}
	}

	return false
}

func isExplicitSecretShape(s string) bool {
	return openAIStyleKeyPattern.MatchString(s) ||
		distinctSKPattern.MatchString(s) ||
		knownProviderKeyPattern.MatchString(s) ||
		knownProviderURLPattern.MatchString(s) ||
		discordBotTokenPattern.MatchString(s) && isDiscordBotToken(s) ||
		jwtPattern.MatchString(s) ||
		pemPrivateKeyPattern.MatchString(s) ||
		pgpPrivateKeyPattern.MatchString(s) ||
		puttyPrivateKeyPattern.MatchString(s) ||
		ageSecretKeyPattern.MatchString(s) ||
		solanaKeypairPattern.MatchString(s) && isBase58SecretKey(s) ||
		ed25519JSONArrayPattern.MatchString(s) && isEd25519SecretArray(s) ||
		isBIP39Mnemonic(s) ||
		isBareCredentialShape(s) ||
		isValidatedRawCredentialShape(s)
}

// isValidatedRawCredentialShape recognizes the exact values BuiltinScanner.Scan
// stores via addRaw for the validator/context-gated raw credential rules
// (aws-secret-access-key, azure-storage-account-key, basic-auth). Those
// detections already cleared a strict gate at scan time (nearby keyword or a
// decode validator), so exempting their shape here stops the generic
// wordlike/separator benign heuristics — which fire on base64 '/' runs and
// letter-heavy bodies — from silently discarding a real secret downstream.
func isValidatedRawCredentialShape(s string) bool {
	if isAWSSecretAccessKeyShape(s) {
		return true
	}
	if azureStorageAccountKeyValuePattern.MatchString(s) {
		return true
	}
	if basicAuthValuePattern.MatchString(s) && isBasicAuthCredential(s) {
		return true
	}
	return false
}

// isAWSSecretAccessKeyShape matches the 40-char base64 body stored for an
// aws-secret-access-key finding while requiring a non-alphabetic character so a
// 40-letter identifier word is not mistaken for a key.
func isAWSSecretAccessKeyShape(s string) bool {
	if !awsSecretAccessKeyValuePattern.MatchString(s) {
		return false
	}
	return strings.ContainsAny(s, "0123456789+/=")
}

// isBareCredentialShape is deliberately full-string and validator-gated:
// substring matching here would let any long value CONTAINING a
// credential-shaped run bypass the wordlike/separator benign heuristics,
// weakening the identifier false-positive protections.
func isBareCredentialShape(s string) bool {
	if m := barePrefixedTokenPattern.FindString(s); m == s && isBarePrefixedCredential(s) {
		return true
	}
	if m := bareHexPairTokenPattern.FindString(s); m == s && isBareHexPairCredential(s) {
		return true
	}
	return false
}

func wordlikeRatio(s string) float64 {
	tokens := splitCandidateSubtokens(s)
	if len(tokens) == 0 {
		return 0
	}
	wordlike := 0
	for _, token := range tokens {
		if isWordlikeSubtoken(token) {
			wordlike++
		}
	}
	return float64(wordlike) / float64(len(tokens))
}

func isWordlikeSubtoken(token string) bool {
	token = strings.TrimSpace(token)
	if len(token) < 2 {
		return false
	}
	letters := 0
	digits := 0
	for _, r := range token {
		switch {
		case r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z':
			letters++
		case r >= '0' && r <= '9':
			digits++
		}
	}
	return letters >= 2 && digits == 0
}

func separatorPartCount(s string) int {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/'
	})
	count := 0
	for _, part := range parts {
		if part != "" {
			count++
		}
	}
	return count
}
