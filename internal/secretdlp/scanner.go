package secretdlp

import (
	"bytes"
	"context"
	"math"
	"regexp"
	"sort"
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
		seen[secret] = Finding{
			Secret:     secret,
			RuleID:     result.Finding.RuleID,
			Source:     "betterleaks",
			Confidence: betterLeaksConfidence(result.Finding.RuleID, s.confidence),
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
	knownProviderKeyPattern = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{36,255}|github_pat_[A-Za-z0-9_]{22,255}|glpat-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|npm_[A-Za-z0-9]{36}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|r8_[A-Za-z0-9]{30,}|figd_[A-Za-z0-9_-]{20,}|whsec_[A-Za-z0-9]{24,}|shp(?:at|pa|ss)_[a-fA-F0-9]{32})\b`)
	// bare-credential shapes: context-free by design (real tokens are pasted bare),
	// but shape-strict so identifiers cannot satisfy them. Entropy/structure are
	// internal validators here (isBarePrefixedCredential / isBareHexPairCredential),
	// not a generic entropy rule; SECRET_DLP_HIGH_ENTROPY does not gate these.
	barePrefixedTokenPattern = regexp.MustCompile(`\b[a-z][a-z0-9]{1,5}[-_][A-Za-z0-9]{30,64}\b`)
	bareHexPairTokenPattern  = regexp.MustCompile(`\b[0-9a-f]{24,64}\.[A-Za-z0-9_-]{12,64}\b`)
	bareTokenSeparator       = regexp.MustCompile(`[-_]`)
	distinctSKPattern        = regexp.MustCompile(`\b(?:sk-ant-[A-Za-z0-9_-]{20,}|sk-proj-[A-Za-z0-9_-]{20,}|sk-or-v1-[A-Za-z0-9_-]{20,}|sk_live_[A-Za-z0-9_-]{20,})\b`)
	dsnUserInfoPattern       = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:([^@\s/?#]+)@[^/\s]+`)
	bearerPattern            = regexp.MustCompile(`(?i)\bBearer\s+([A-Za-z0-9._~+/=-]{16,})`)
	jwtPattern               = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	pemPrivateKeyPattern     = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]+?-----END [A-Z ]*PRIVATE KEY-----`)
	highEntropyPattern       = regexp.MustCompile(`\b[A-Za-z0-9._~+/=-]{24,}\b`)
	commitSHAPattern         = regexp.MustCompile(`^[a-f0-9]{40}$`)
	uuidPattern              = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
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

	for _, seg := range segs {
		text := scanTextForSegment(seg)
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
	case "pem-private-key":
		return 1.0
	case "sk-distinct":
		return 0.98
	case "openai-sk", "openai-style-key", "known-provider-key":
		return 0.95
	case "bearer-token", "jwt", "dsn-userinfo":
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
		if secret == "" || looksBenignCandidate(secret) || ids.containsOrEmbeds(secret) {
			continue
		}
		f.Secret = secret
		f.Confidence = confidenceForFinding(f)
		if f.Confidence >= cfg.RedactThreshold {
			accepted = append(accepted, f)
		} else {
			shadow = append(shadow, f)
		}
	}
	return findingsFromMap(findingsBySecret(accepted)), findingsFromMap(findingsBySecret(shadow))
}

func findingsBySecret(findings []Finding) map[string]Finding {
	out := make(map[string]Finding, len(findings))
	for _, f := range findings {
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
	hasDSN := bytes.Contains(body, []byte("://"))
	hasKnown := hasKnownProviderAnchor(body)
	if !hasSK && !hasJWT && !hasPEM && !hasDSN && !hasKnown {
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
	return findingsFromMap(seen)
}

var knownProviderAnchors = [][]byte{
	[]byte("ghp_"), []byte("gho_"), []byte("ghu_"), []byte("ghs_"), []byte("ghr_"),
	[]byte("github_pat_"), []byte("glpat-"), []byte("xoxb-"), []byte("xoxa-"),
	[]byte("xoxp-"), []byte("xoxr-"), []byte("xoxs-"), []byte("npm_"),
	[]byte("AKIA"), []byte("AIza"), []byte("r8_"), []byte("figd_"),
	[]byte("whsec_"), []byte("shpat_"), []byte("shppa_"), []byte("shpss_"),
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

	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
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
		jwtPattern.MatchString(s) ||
		pemPrivateKeyPattern.MatchString(s) ||
		isBareCredentialShape(s)
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
