package secretdlp

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	placeholderPrefix              = "__CPA_DLP_v1_"
	minPlaceholderLength           = len(placeholderPrefix) + 12 + 1 + 16 + 1 + 3 + 1 + 11 + 2
	defaultStreamPlaceholderWindow = 128
)

var placeholderPattern = regexp.MustCompile(`__CPA_DLP_v1_[A-Za-z0-9_-]{12}_[A-Za-z0-9_-]{16}_\d{3,}_[A-Za-z0-9_-]{11}__`)

var (
	secretDLPExcludedTopLevelJSONPathKeys = map[string]bool{
		"functions":       true,
		"metadata":        true,
		"response_format": true,
		"tool_choice":     true,
		"tools":           true,
	}
	secretDLPSchemaJSONPathKeys = map[string]bool{
		"$defs":        true,
		"allof":        true,
		"anyof":        true,
		"definitions":  true,
		"enum":         true,
		"input_schema": true,
		"items":        true,
		"oneof":        true,
		"parameters":   true,
		"properties":   true,
		"required":     true,
		"schema":       true,
	}
	secretDLPProtocolJSONStringKeys = map[string]bool{
		"call_id":              true,
		"finish_reason":        true,
		"id":                   true,
		"model":                true,
		"name":                 true,
		"object":               true,
		"previous_response_id": true,
		"provider":             true,
		"role":                 true,
		"service_tier":         true,
		"status":               true,
		"tool_call_id":         true,
		"type":                 true,
	}
)

type PlaceholderResolver func(placeholder string) ([]byte, bool)

type Mapping struct {
	Placeholder string
	Secret      []byte
}

type Session struct {
	ID        string
	ClientID  string
	Nonce     string
	ExpiresAt time.Time
	Mode      Mode
	CreatedAt time.Time

	placeholderPrefix string

	mu                  sync.RWMutex
	secretToPlaceholder map[string]string
	placeholderToSecret map[string][]byte
	streamTail          []byte
	maxPlaceholderLen   int
}

func NewSession(masterKey []byte, clientCredential string, ttl time.Duration, mode Mode) *Session {
	clientID := hmacShort(masterKey, clientCredential, 12)
	nonce := randomToken(12)
	now := time.Now()

	return &Session{
		ID:                  randomToken(18),
		ClientID:            clientID,
		Nonce:               nonce,
		ExpiresAt:           now.Add(ttl),
		Mode:                mode,
		CreatedAt:           now,
		placeholderPrefix:   placeholderPrefix,
		secretToPlaceholder: make(map[string]string),
		placeholderToSecret: make(map[string][]byte),
		maxPlaceholderLen:   defaultStreamPlaceholderWindow,
	}
}

func (s *Session) Expired(now time.Time) bool {
	if s == nil {
		return true
	}
	return !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
}

func (s *Session) HasMappings() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.placeholderToSecret) > 0
}

func (s *Session) RedactRawWithMappings(body []byte, findings []Finding) ([]byte, []Mapping) {
	if s == nil || len(body) == 0 || len(findings) == 0 {
		return bytes.Clone(body), nil
	}
	return s.redactRawWithMappings(body, findings)
}

func (s *Session) RedactSegments(body []byte, segments []Segment, findings []Finding) ([]byte, []Mapping) {
	if s == nil || len(body) == 0 || len(segments) == 0 || len(findings) == 0 {
		return bytes.Clone(body), nil
	}

	findings = sortedFindingsBySecretLength(findings)
	segments = sortedSegmentsByAscendingStart(segments)

	out := make([]byte, 0, len(body))
	mappingsByPlaceholder := make(map[string][]byte)
	cursor := 0

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, seg := range segments {
		if seg.TokSpan[0] < 0 || seg.TokSpan[1] > len(body) || seg.TokSpan[0] >= seg.TokSpan[1] {
			continue
		}
		if seg.TokSpan[0] < cursor {
			continue
		}
		redacted, changed := s.redactSegmentValueLocked(seg, findings, mappingsByPlaceholder)
		if !changed {
			continue
		}
		encoded, err := json.Marshal(redacted)
		if err != nil {
			continue
		}
		out = append(out, body[cursor:seg.TokSpan[0]]...)
		out = append(out, encoded...)
		cursor = seg.TokSpan[1]
	}
	out = append(out, body[cursor:]...)

	return out, mappingsFromMap(mappingsByPlaceholder)
}

func (s *Session) redactSegmentValueLocked(seg Segment, findings []Finding, mappingsByPlaceholder map[string][]byte) (string, bool) {
	if seg.Kind == ToolArgs {
		if redacted, changed, ok := s.redactNestedJSONStringValuesLocked(seg.Value, findings, mappingsByPlaceholder); ok {
			return redacted, changed
		}
	}

	redacted := seg.Value
	for _, f := range findings {
		secret := strings.TrimSpace(f.Secret)
		if secret == "" || !strings.Contains(redacted, secret) {
			continue
		}
		placeholder := s.placeholderForSecretLocked(secret)
		mappingsByPlaceholder[placeholder] = []byte(secret)
		redacted = strings.ReplaceAll(redacted, secret, placeholder)
	}
	return redacted, redacted != seg.Value
}

func (s *Session) redactNestedJSONStringValuesLocked(value string, findings []Finding, mappingsByPlaceholder map[string][]byte) (string, bool, bool) {
	doc, err := tokenizeJSON([]byte(value))
	if err != nil {
		return value, false, false
	}
	segments := make([]Segment, 0, len(doc.Spans))
	for _, span := range doc.Spans {
		segments = append(segments, Segment{
			Path:    span.Pointer,
			Value:   span.Value,
			Kind:    ContentText,
			TokSpan: span.TokSpan,
		})
	}

	var out []byte
	cursor := 0
	changed := false
	for _, nested := range sortedSegmentsByAscendingStart(segments) {
		if nested.TokSpan[0] < cursor {
			continue
		}
		redacted := nested.Value
		for _, f := range findings {
			secret := strings.TrimSpace(f.Secret)
			if secret == "" || !strings.Contains(redacted, secret) {
				continue
			}
			placeholder := s.placeholderForSecretLocked(secret)
			mappingsByPlaceholder[placeholder] = []byte(secret)
			redacted = strings.ReplaceAll(redacted, secret, placeholder)
		}
		if redacted == nested.Value {
			continue
		}
		encoded, err := json.Marshal(redacted)
		if err != nil {
			continue
		}
		if out == nil {
			out = make([]byte, 0, len(value))
		}
		out = append(out, value[cursor:nested.TokSpan[0]]...)
		out = append(out, encoded...)
		cursor = nested.TokSpan[1]
		changed = true
	}
	if !changed {
		return value, false, true
	}
	out = append(out, value[cursor:]...)
	return string(out), true, true
}

func (s *Session) redactRawWithMappings(body []byte, findings []Finding) ([]byte, []Mapping) {
	out := bytes.Clone(body)
	findings = sortedFindingsBySecretLength(findings)

	s.mu.Lock()
	defer s.mu.Unlock()

	mappingsByPlaceholder := make(map[string][]byte)
	for _, f := range findings {
		secret := strings.TrimSpace(f.Secret)
		if secret == "" {
			continue
		}
		secretBytes := []byte(secret)
		if !hasSecretValueOccurrence(out, secretBytes) {
			continue
		}
		placeholder := s.placeholderForSecretLocked(secret)
		var count int
		out, count = replaceAllSecretValueCount(out, secretBytes, []byte(placeholder))
		if count > 0 {
			mappingsByPlaceholder[placeholder] = []byte(secret)
		}
	}

	return out, mappingsFromMap(mappingsByPlaceholder)
}

func sortedSegmentsByAscendingStart(segments []Segment) []Segment {
	out := append([]Segment(nil), segments...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].TokSpan[0] < out[j].TokSpan[0]
	})
	return out
}

func sortedFindingsBySecretLength(findings []Finding) []Finding {
	out := append([]Finding(nil), findings...)
	sort.Slice(out, func(i, j int) bool {
		return len(out[i].Secret) > len(out[j].Secret)
	})
	return out
}

func (s *Session) placeholderForSecretLocked(secret string) string {
	placeholder, ok := s.secretToPlaceholder[secret]
	if !ok {
		placeholder = s.newPlaceholderLocked(len(s.secretToPlaceholder) + 1)
		s.secretToPlaceholder[secret] = placeholder
		s.placeholderToSecret[placeholder] = []byte(secret)
		if len(placeholder) > s.maxPlaceholderLen {
			s.maxPlaceholderLen = len(placeholder)
		}
	}
	return placeholder
}

func mappingsFromMap(mappingsByPlaceholder map[string][]byte) []Mapping {
	mappings := make([]Mapping, 0, len(mappingsByPlaceholder))
	for placeholder, secret := range mappingsByPlaceholder {
		mappings = append(mappings, Mapping{
			Placeholder: placeholder,
			Secret:      cloneBytes(secret),
		})
	}
	sort.Slice(mappings, func(i, j int) bool {
		return mappings[i].Placeholder < mappings[j].Placeholder
	})

	return mappings
}

func isSecretDLPBlockedJSONPath(path []string) bool {
	if len(path) == 0 {
		return true
	}

	if secretDLPExcludedTopLevelJSONPathKeys[normalizeJSONPathKey(path[0])] {
		return true
	}

	for _, key := range path {
		if secretDLPSchemaJSONPathKeys[normalizeJSONPathKey(key)] {
			return true
		}
	}

	last := normalizeJSONPathKey(path[len(path)-1])
	return secretDLPProtocolJSONStringKeys[last]
}

func isSecretDLPRestorableJSONPath(path []string) bool {
	return !isSecretDLPBlockedJSONPath(path)
}

func normalizeJSONPathKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "_")
	return key
}

func (s *Session) restoreJSONValueLocked(value any, path []string, resolver PlaceholderResolver) (any, int) {
	switch v := value.(type) {
	case map[string]any:
		restored := 0
		for key, child := range v {
			next, count := s.restoreJSONValueLocked(child, append(path, key), resolver)
			if count > 0 {
				v[key] = next
				restored += count
			}
		}
		return v, restored
	case []any:
		restored := 0
		for i, child := range v {
			next, count := s.restoreJSONValueLocked(child, path, resolver)
			if count > 0 {
				v[i] = next
				restored += count
			}
		}
		return v, restored
	case string:
		if !isSecretDLPRestorableJSONPath(path) {
			return v, 0
		}
		return s.restoreJSONStringLocked(v, resolver)
	default:
		return value, 0
	}
}

func (s *Session) restoreJSONStringLocked(value string, resolver PlaceholderResolver) (string, int) {
	if !strings.Contains(value, placeholderPrefix) {
		return value, 0
	}

	restored := 0
	out := value
	for placeholder, secret := range s.placeholderToSecret {
		var count int
		out, count = replaceAllStringCount(out, placeholder, string(secret))
		restored += count
	}
	if resolver == nil {
		return out, restored
	}

	matches := placeholderPattern.FindAllString(out, -1)
	seen := make(map[string]struct{}, len(matches))
	for _, placeholder := range matches {
		if _, ok := seen[placeholder]; ok {
			continue
		}
		seen[placeholder] = struct{}{}
		if _, ok := s.placeholderToSecret[placeholder]; ok {
			continue
		}
		secret, ok := resolver(placeholder)
		if !ok || len(secret) == 0 {
			continue
		}
		secret = cloneBytes(secret)
		s.placeholderToSecret[placeholder] = secret
		if len(placeholder) > s.maxPlaceholderLen {
			s.maxPlaceholderLen = len(placeholder)
		}
		var count int
		out, count = replaceAllStringCount(out, placeholder, string(secret))
		restored += count
	}
	return out, restored
}

func replaceAllStringCount(s, old, new string) (string, int) {
	if old == "" {
		return s, 0
	}
	count := strings.Count(s, old)
	if count == 0 {
		return s, 0
	}
	return strings.ReplaceAll(s, old, new), count
}

func decodeSecretDLPJSON(body []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func (s *Session) restoreJSONBodyLocked(body []byte, resolver PlaceholderResolver) ([]byte, int, bool) {
	var root any
	if err := decodeSecretDLPJSON(body, &root); err != nil {
		return nil, 0, false
	}
	restored, count := s.restoreJSONValueLocked(root, nil, resolver)
	if count == 0 {
		return bytes.Clone(body), 0, true
	}
	out, err := json.Marshal(restored)
	if err != nil {
		return nil, 0, false
	}
	return out, count, true
}

func (s *Session) restoreRawJSONLocked(body []byte, resolver PlaceholderResolver) ([]byte, int) {
	out := bytes.Clone(body)
	restored := 0
	for placeholder, secret := range s.placeholderToSecret {
		replacement := jsonEscapedStringContent(secret)
		var count int
		out, count = replaceAllCount(out, []byte(placeholder), replacement)
		restored += count
	}
	if resolver == nil {
		return out, restored
	}

	matches := placeholderPattern.FindAll(out, -1)
	seen := make(map[string]struct{}, len(matches))
	for _, match := range matches {
		placeholder := string(match)
		if _, ok := seen[placeholder]; ok {
			continue
		}
		seen[placeholder] = struct{}{}
		if _, ok := s.placeholderToSecret[placeholder]; ok {
			continue
		}
		secret, ok := resolver(placeholder)
		if !ok || len(secret) == 0 {
			continue
		}
		secret = cloneBytes(secret)
		s.placeholderToSecret[placeholder] = secret
		if len(placeholder) > s.maxPlaceholderLen {
			s.maxPlaceholderLen = len(placeholder)
		}
		replacement := jsonEscapedStringContent(secret)
		var count int
		out, count = replaceAllCount(out, []byte(placeholder), replacement)
		restored += count
	}
	return out, restored
}

func (s *Session) restoreJSONLocked(body []byte, resolver PlaceholderResolver) ([]byte, int) {
	if out, restored, ok := s.restoreJSONBodyLocked(body, resolver); ok {
		return out, restored
	}
	return s.restoreRawJSONLocked(body, resolver)
}

func (s *Session) restoreStreamJSONLocked(body []byte, resolver PlaceholderResolver) ([]byte, int) {
	// Streaming deltas can arrive as SSE fragments or partial JSON strings, so the
	// structured path-blocking restore used for complete JSON bodies is not reliable
	// here. Placeholders are high-entropy protocol tokens, and raw replacement keeps
	// split-token streaming correct while non-streaming responses preserve blocked
	// fields via restoreJSONBodyLocked.
	return s.restoreRawJSONLocked(body, resolver)
}

func (s *Session) RestoreJSON(body []byte) []byte {
	return s.RestoreJSONWithResolver(body, nil)
}

func (s *Session) RestoreJSONWithResolver(body []byte, resolver PlaceholderResolver) []byte {
	out, _ := s.RestoreJSONWithResolverStats(body, resolver)
	return out
}

func (s *Session) RestoreJSONWithResolverStats(body []byte, resolver PlaceholderResolver) ([]byte, int) {
	if s == nil || len(body) == 0 {
		return bytes.Clone(body), 0
	}

	if resolver == nil {
		s.mu.RLock()
		out, restored := s.restoreJSONLocked(body, resolver)
		s.mu.RUnlock()
		return out, restored
	}
	s.mu.Lock()
	out, restored := s.restoreJSONLocked(body, resolver)
	s.mu.Unlock()
	return out, restored
}

func (s *Session) RestoreStreamJSONChunk(chunk []byte) []byte {
	return s.RestoreStreamJSONChunkWithResolver(chunk, nil)
}

func (s *Session) RestoreStreamJSONChunkWithResolver(chunk []byte, resolver PlaceholderResolver) []byte {
	out, _ := s.RestoreStreamJSONChunkWithResolverStats(chunk, resolver)
	return out
}

func (s *Session) RestoreStreamJSONChunkWithResolverStats(chunk []byte, resolver PlaceholderResolver) ([]byte, int) {
	if s == nil || len(chunk) == 0 {
		return bytes.Clone(chunk), 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	combined := append(bytes.Clone(s.streamTail), chunk...)
	out, restored := s.restoreStreamJSONLocked(combined, resolver)
	hold := placeholderHoldbackLen(out, s.maxPlaceholderLen)
	if hold > 0 && len(out) <= hold {
		s.streamTail = out
		return nil, restored
	}

	var safe []byte
	if hold > 0 {
		safe = out[:len(out)-hold]
		s.streamTail = bytes.Clone(out[len(out)-hold:])
	} else {
		safe = out
		s.streamTail = nil
	}

	return bytes.Clone(safe), restored
}

func placeholderHoldbackLen(body []byte, maxPlaceholderLen int) int {
	if len(body) == 0 {
		return 0
	}
	if maxPlaceholderLen < defaultStreamPlaceholderWindow {
		maxPlaceholderLen = defaultStreamPlaceholderWindow
	}
	maxHold := maxPlaceholderLen - 1
	if maxHold > len(body) {
		maxHold = len(body)
	}
	for n := maxHold; n > 0; n-- {
		if isPotentialPlaceholderPrefix(string(body[len(body)-n:])) {
			return n
		}
	}
	return 0
}

func isPotentialPlaceholderPrefix(s string) bool {
	if s == "" {
		return false
	}
	prefix := placeholderPrefix
	if strings.HasPrefix(prefix, s) {
		return true
	}
	if !strings.HasPrefix(s, prefix) {
		return false
	}
	for _, r := range s[len(prefix):] {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (s *Session) FlushStreamJSONTail() []byte {
	return s.FlushStreamJSONTailWithResolver(nil)
}

func (s *Session) FlushStreamJSONTailWithResolver(resolver PlaceholderResolver) []byte {
	out, _ := s.FlushStreamJSONTailWithResolverStats(resolver)
	return out
}

func (s *Session) FlushStreamJSONTailWithResolverStats(resolver PlaceholderResolver) ([]byte, int) {
	if s == nil {
		return nil, 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.streamTail) == 0 {
		return nil, 0
	}

	out, restored := s.restoreStreamJSONLocked(s.streamTail, resolver)
	s.streamTail = nil
	return out, restored
}

func (s *Session) RedactForLog(body []byte) []byte {
	out, _ := s.RedactForLogStats(body)
	return out
}

func (s *Session) RedactForLogStats(body []byte) ([]byte, int) {
	if s == nil || len(body) == 0 {
		return bytes.Clone(body), 0
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := bytes.Clone(body)
	redacted := 0

	type replacement struct {
		raw         []byte
		jsonEscaped []byte
		placeholder []byte
	}

	replacements := make([]replacement, 0, len(s.placeholderToSecret))
	for placeholder, secret := range s.placeholderToSecret {
		replacements = append(replacements, replacement{
			raw:         bytes.Clone(secret),
			jsonEscaped: jsonEscapedStringContent(secret),
			placeholder: []byte(placeholder),
		})
	}

	sort.Slice(replacements, func(i, j int) bool {
		return len(replacements[i].raw) > len(replacements[j].raw)
	})

	for _, r := range replacements {
		var count int
		out, count = replaceAllSecretValueCount(out, r.jsonEscaped, r.placeholder)
		redacted += count
		out, count = replaceAllSecretValueCount(out, r.raw, r.placeholder)
		redacted += count
	}

	return out, redacted
}

func (s *Session) newPlaceholderLocked(counter int) string {
	return fmt.Sprintf("%s%s_%s_%03d_%s__", s.placeholderPrefix, s.ClientID, s.Nonce, counter, randomToken(8))
}

func hmacShort(key []byte, value string, n int) string {
	if len(key) == 0 {
		key = []byte("cliproxy-secret-dlp-boot-key-missing")
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	sum := mac.Sum(nil)
	encoded := base64.RawURLEncoding.EncodeToString(sum)
	if n > 0 && len(encoded) > n {
		return encoded[:n]
	}
	return encoded
}

func jsonEscapedStringContent(raw []byte) []byte {
	encoded, err := json.Marshal(string(raw))
	if err != nil || len(encoded) < 2 {
		return raw
	}
	return encoded[1 : len(encoded)-1]
}

func replaceAllCount(body, old, replacement []byte) ([]byte, int) {
	if len(body) == 0 || len(old) == 0 {
		return body, 0
	}
	count := bytes.Count(body, old)
	if count == 0 {
		return body, 0
	}
	return bytes.ReplaceAll(body, old, replacement), count
}

func replaceAllSecretValueCount(body, secret, replacement []byte) ([]byte, int) {
	if len(body) == 0 || len(secret) == 0 {
		return body, 0
	}

	out := make([]byte, 0, len(body))
	cursor := 0
	replaced := 0
	for {
		idx := bytes.Index(body[cursor:], secret)
		if idx < 0 {
			break
		}
		start := cursor + idx
		end := start + len(secret)
		if isSecretValueBoundary(body, start-1) && isSecretValueBoundary(body, end) {
			out = append(out, body[cursor:start]...)
			out = append(out, replacement...)
			cursor = end
			replaced++
			continue
		}
		out = append(out, body[cursor:end]...)
		cursor = end
	}
	if replaced == 0 {
		return body, 0
	}
	out = append(out, body[cursor:]...)
	return out, replaced
}

func hasSecretValueOccurrence(body, secret []byte) bool {
	if len(body) == 0 || len(secret) == 0 {
		return false
	}
	cursor := 0
	for {
		idx := bytes.Index(body[cursor:], secret)
		if idx < 0 {
			return false
		}
		start := cursor + idx
		end := start + len(secret)
		if isSecretValueBoundary(body, start-1) && isSecretValueBoundary(body, end) {
			return true
		}
		cursor = end
	}
}

func isSecretValueBoundary(body []byte, idx int) bool {
	if idx < 0 || idx >= len(body) {
		return true
	}
	b := body[idx]
	return !(b >= 'A' && b <= 'Z' ||
		b >= 'a' && b <= 'z' ||
		b >= '0' && b <= '9' ||
		b == '.' ||
		b == '_' ||
		b == '~' ||
		b == '+' ||
		b == '/' ||
		b == '=' ||
		b == '-')
}
