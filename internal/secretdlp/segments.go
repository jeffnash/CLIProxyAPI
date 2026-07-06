package secretdlp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type SegKind int

const (
	ContentText SegKind = iota
	ToolArgs
	ToolResult
	SecretField
)

type Segment struct {
	Path    string
	Value   string
	Kind    SegKind
	TokSpan [2]int
}

type jsonStringSpan struct {
	Path    []string
	Pointer string
	Value   string
	TokSpan [2]int
}

type jsonDocument struct {
	Root  any
	Spans []jsonStringSpan
}

func tokenizeJSON(body []byte) (*jsonDocument, error) {
	parser := &jsonBodyParser{body: body}
	root, err := parser.parseValue(nil)
	if err != nil {
		return nil, err
	}
	parser.skipWhitespace()
	if parser.pos != len(parser.body) {
		return nil, fmt.Errorf("unexpected trailing JSON at byte %d", parser.pos)
	}
	return &jsonDocument{Root: root, Spans: parser.spans}, nil
}

func extractSegments(doc *jsonDocument, pack PathPack) []Segment {
	if doc == nil {
		return nil
	}
	segments := make([]Segment, 0, len(doc.Spans))
	for _, span := range doc.Spans {
		kind, ok := pack.segmentKind(span.Path)
		if !ok {
			continue
		}
		segments = append(segments, Segment{
			Path:    span.Pointer,
			Value:   span.Value,
			Kind:    kind,
			TokSpan: span.TokSpan,
		})
	}
	return segments
}

func jsonPointer(path []string) string {
	if len(path) == 0 {
		return ""
	}
	var b strings.Builder
	for _, part := range path {
		b.WriteByte('/')
		part = strings.ReplaceAll(part, "~", "~0")
		part = strings.ReplaceAll(part, "/", "~1")
		b.WriteString(part)
	}
	return b.String()
}

type jsonBodyParser struct {
	body  []byte
	pos   int
	spans []jsonStringSpan
}

func (p *jsonBodyParser) parseValue(path []string) (any, error) {
	p.skipWhitespace()
	if p.pos >= len(p.body) {
		return nil, fmt.Errorf("unexpected end of JSON")
	}

	switch p.body[p.pos] {
	case '{':
		return p.parseObject(path)
	case '[':
		return p.parseArray(path)
	case '"':
		start := p.pos
		value, end, err := p.parseStringToken()
		if err != nil {
			return nil, err
		}
		p.spans = append(p.spans, jsonStringSpan{
			Path:    append([]string(nil), path...),
			Pointer: jsonPointer(path),
			Value:   value,
			TokSpan: [2]int{start, end},
		})
		return value, nil
	case 't':
		if p.consumeLiteral("true") {
			return true, nil
		}
	case 'f':
		if p.consumeLiteral("false") {
			return false, nil
		}
	case 'n':
		if p.consumeLiteral("null") {
			return nil, nil
		}
	default:
		if p.body[p.pos] == '-' || isDigit(p.body[p.pos]) {
			return p.parseNumber()
		}
	}
	return nil, fmt.Errorf("unexpected JSON byte %q at byte %d", p.body[p.pos], p.pos)
}

func (p *jsonBodyParser) parseObject(path []string) (map[string]any, error) {
	if p.body[p.pos] != '{' {
		return nil, fmt.Errorf("expected object at byte %d", p.pos)
	}
	p.pos++
	out := make(map[string]any)
	p.skipWhitespace()
	if p.consumeByte('}') {
		return out, nil
	}
	for {
		p.skipWhitespace()
		if p.pos >= len(p.body) || p.body[p.pos] != '"' {
			return nil, fmt.Errorf("expected object key at byte %d", p.pos)
		}
		key, _, err := p.parseStringToken()
		if err != nil {
			return nil, err
		}
		p.skipWhitespace()
		if !p.consumeByte(':') {
			return nil, fmt.Errorf("expected ':' after object key at byte %d", p.pos)
		}
		value, err := p.parseValue(append(path, key))
		if err != nil {
			return nil, err
		}
		out[key] = value
		p.skipWhitespace()
		if p.consumeByte('}') {
			return out, nil
		}
		if !p.consumeByte(',') {
			return nil, fmt.Errorf("expected ',' or '}' at byte %d", p.pos)
		}
	}
}

func (p *jsonBodyParser) parseArray(path []string) ([]any, error) {
	if p.body[p.pos] != '[' {
		return nil, fmt.Errorf("expected array at byte %d", p.pos)
	}
	p.pos++
	var out []any
	p.skipWhitespace()
	if p.consumeByte(']') {
		return out, nil
	}
	for i := 0; ; i++ {
		value, err := p.parseValue(append(path, strconv.Itoa(i)))
		if err != nil {
			return nil, err
		}
		out = append(out, value)
		p.skipWhitespace()
		if p.consumeByte(']') {
			return out, nil
		}
		if !p.consumeByte(',') {
			return nil, fmt.Errorf("expected ',' or ']' at byte %d", p.pos)
		}
	}
}

func (p *jsonBodyParser) parseStringToken() (string, int, error) {
	start := p.pos
	if p.pos >= len(p.body) || p.body[p.pos] != '"' {
		return "", p.pos, fmt.Errorf("expected string at byte %d", p.pos)
	}
	p.pos++
	for p.pos < len(p.body) {
		switch p.body[p.pos] {
		case '\\':
			if p.pos+1 >= len(p.body) {
				return "", p.pos, fmt.Errorf("unterminated escape sequence at byte %d", p.pos)
			}
			p.pos += 2
		case '"':
			p.pos++
			var out string
			if err := json.Unmarshal(p.body[start:p.pos], &out); err != nil {
				return "", p.pos, err
			}
			return out, p.pos, nil
		default:
			if p.body[p.pos] < 0x20 {
				return "", p.pos, fmt.Errorf("invalid control character in string at byte %d", p.pos)
			}
			p.pos++
		}
	}
	return "", p.pos, fmt.Errorf("unterminated string at byte %d", start)
}

func (p *jsonBodyParser) parseNumber() (json.Number, error) {
	start := p.pos
	if p.consumeByte('-') && p.pos >= len(p.body) {
		return "", fmt.Errorf("invalid number at byte %d", start)
	}
	if p.consumeByte('0') {
		if p.pos < len(p.body) && isDigit(p.body[p.pos]) {
			return "", fmt.Errorf("invalid leading zero at byte %d", start)
		}
	} else if p.pos < len(p.body) && isDigit1To9(p.body[p.pos]) {
		for p.pos < len(p.body) && isDigit(p.body[p.pos]) {
			p.pos++
		}
	} else {
		return "", fmt.Errorf("invalid number at byte %d", start)
	}
	if p.consumeByte('.') {
		if p.pos >= len(p.body) || !isDigit(p.body[p.pos]) {
			return "", fmt.Errorf("invalid number fraction at byte %d", start)
		}
		for p.pos < len(p.body) && isDigit(p.body[p.pos]) {
			p.pos++
		}
	}
	if p.pos < len(p.body) && (p.body[p.pos] == 'e' || p.body[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.body) && (p.body[p.pos] == '+' || p.body[p.pos] == '-') {
			p.pos++
		}
		if p.pos >= len(p.body) || !isDigit(p.body[p.pos]) {
			return "", fmt.Errorf("invalid number exponent at byte %d", start)
		}
		for p.pos < len(p.body) && isDigit(p.body[p.pos]) {
			p.pos++
		}
	}
	return json.Number(string(p.body[start:p.pos])), nil
}

func (p *jsonBodyParser) skipWhitespace() {
	for p.pos < len(p.body) {
		switch p.body[p.pos] {
		case ' ', '\n', '\r', '\t':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonBodyParser) consumeByte(b byte) bool {
	if p.pos < len(p.body) && p.body[p.pos] == b {
		p.pos++
		return true
	}
	return false
}

func (p *jsonBodyParser) consumeLiteral(lit string) bool {
	if bytes.HasPrefix(p.body[p.pos:], []byte(lit)) {
		p.pos += len(lit)
		return true
	}
	return false
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

func isDigit1To9(b byte) bool {
	return b >= '1' && b <= '9'
}

func splitCandidateSubtokens(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/' || r == ':'
	})
	var out []string
	for _, field := range fields {
		out = append(out, splitCamelSubtokens(field)...)
	}
	return out
}

func splitCamelSubtokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	runes := []rune(s)
	for i := 1; i < len(runes); i++ {
		prev := runes[i-1]
		cur := runes[i]
		nextLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if unicode.IsLower(prev) && unicode.IsUpper(cur) || unicode.IsUpper(prev) && unicode.IsUpper(cur) && nextLower {
			out = append(out, string(runes[start:i]))
			start = i
		}
	}
	out = append(out, string(runes[start:]))
	return out
}
