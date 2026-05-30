package executor

import (
	"encoding/json"
	"regexp"
	"strings"
)

// composerToolCall is a parsed inline tool-call recovered from Composer-2.5's
// streamed text. The model emits tool calls as marker-bracketed blocks in the
// visible text stream:
//
//	<|tool_calls_begin|>
//	  <|tool_call_begin|>
//	    NAME
//	    <|tool_sep|>
//	    KEY1
//	    VALUE1
//	    <|tool_sep|>
//	    KEY2
//	    VALUE2
//	  <|tool_call_end|>
//	  <|tool_call_begin|>
//	    ...JSON or key/value or inline...
//	  <|tool_call_end|>
//	<|tool_calls_end|>
//
// Both ASCII (`<|...|>`) and full-width (`<｜...▁...｜>`) marker variants are
// accepted. Body variants:
//   - JSON: `{"name":"foo","arguments":{...}}` (or `{name, args, input,
//     parameters}`)
//   - Key/value: `NAME<|tool_sep|>key\nvalue<|tool_sep|>...`
//   - Inline: `name(key=value,...)` or `name[k=v]`
//
// Ported from /home/jmn/composer-api/worker/cursor.ts (ComposerToolCallFilter
// + parseComposerToolCalls and friends, lines 666-900).
type composerToolCall struct {
	Name      string
	Arguments map[string]any
}

// composerToolFilterEvent is the discriminated union produced by composerToolCallFilter.Push.
type composerToolFilterEvent struct {
	Kind     string // "text" | "tool_call"
	Text     string
	ToolCall composerToolCall
}

// composerToolCallFilter buffers streamed text and extracts inline tool-call
// blocks. Plain text segments and parsed tool calls are returned in order.
// Markers may be split across chunks; the filter holds back the longest
// candidate-marker suffix of the buffer until either the block completes or
// non-marker content arrives.
type composerToolCallFilter struct {
	buf strings.Builder
}

func newComposerToolCallFilter() *composerToolCallFilter {
	return &composerToolCallFilter{}
}

// Push appends a chunk and returns whatever events can be emitted from the
// current buffer. Flush must be called at end of stream to drain residual
// bytes that never completed a marker block.
func (f *composerToolCallFilter) Push(delta string) []composerToolFilterEvent {
	f.buf.WriteString(delta)
	return f.drain(false)
}

// Flush drains the buffer at end-of-stream. Any unfinished tool-call block is
// returned as plain text (the model didn't complete it — better to show than
// to drop).
func (f *composerToolCallFilter) Flush() []composerToolFilterEvent {
	return f.drain(true)
}

func (f *composerToolCallFilter) drain(force bool) []composerToolFilterEvent {
	var events []composerToolFilterEvent

	for {
		current := f.buf.String()
		begin := findComposerToolMarker(current, "tool_calls_begin")
		if begin == nil {
			// No begin-marker in the buffer.
			if strings.TrimSpace(current) == "" {
				if force {
					f.buf.Reset()
				}
				break
			}
			if force {
				if current != "" {
					events = append(events, composerToolFilterEvent{Kind: "text", Text: current})
				}
				f.buf.Reset()
				break
			}
			// Hold back the longest suffix that could be the start of a marker.
			prefixIdx := composerToolMarkerPrefixIndex(current)
			if prefixIdx >= 0 {
				visible := current[:prefixIdx]
				if strings.TrimSpace(visible) != "" {
					events = append(events, composerToolFilterEvent{Kind: "text", Text: visible})
				}
				f.buf.Reset()
				f.buf.WriteString(current[prefixIdx:])
				break
			}
			// No marker, no candidate prefix — emit and clear.
			events = append(events, composerToolFilterEvent{Kind: "text", Text: current})
			f.buf.Reset()
			break
		}

		// There's text before the begin-marker — emit it first.
		if begin.Index > 0 {
			before := current[:begin.Index]
			if strings.TrimSpace(before) != "" {
				events = append(events, composerToolFilterEvent{Kind: "text", Text: before})
			}
			f.buf.Reset()
			f.buf.WriteString(current[begin.Index:])
			continue
		}

		// Buffer starts at the begin-marker. Look for the matching end-marker.
		after := current[begin.Length:]
		end := findComposerToolMarker(after, "tool_calls_end")
		if end == nil {
			// Block not closed yet. At stream end, emit raw buffer as text.
			if force {
				events = append(events, composerToolFilterEvent{Kind: "text", Text: current})
				f.buf.Reset()
			}
			break
		}

		blockEnd := begin.Length + end.Index + end.Length
		block := current[:blockEnd]
		for _, tc := range parseComposerToolCalls(block) {
			events = append(events, composerToolFilterEvent{Kind: "tool_call", ToolCall: tc})
		}
		remainder := strings.TrimLeft(current[blockEnd:], " \t\r\n")
		f.buf.Reset()
		f.buf.WriteString(remainder)
	}

	return events
}

// composerToolMarkerCandidates is the full list of marker tokens used for
// prefix detection (both ASCII and full-width variants).
var composerToolMarkerCandidates = []string{
	"<|tool_calls_begin|>", "<|tool_calls_end|>",
	"<|tool_call_begin|>", "<|tool_call_end|>",
	"<|tool_sep|>",
	"<｜tool▁calls▁begin｜>", "<｜tool▁calls▁end｜>",
	"<｜tool▁call▁begin｜>", "<｜tool▁call▁end｜>",
	"<｜tool▁sep｜>",
}

// composerToolMarkerPrefixIndex returns the index where a marker-like suffix
// starts, or -1 if no suffix of `value` could be the start of any marker.
// Used to hold back partial markers across chunk boundaries.
func composerToolMarkerPrefixIndex(value string) int {
	max := 0
	for _, c := range composerToolMarkerCandidates {
		if len(c) > max {
			max = len(c)
		}
	}
	if max > len(value) {
		max = len(value)
	}
	for length := max; length >= 1; length-- {
		if length > len(value) {
			continue
		}
		suffix := value[len(value)-length:]
		for _, c := range composerToolMarkerCandidates {
			if strings.HasPrefix(c, suffix) {
				return len(value) - length
			}
		}
	}
	return -1
}

// composerMarker is a regex match result for a tool marker.
type composerMarker struct {
	Index  int
	Length int
}

// composerMarkerPatternBuilder builds the regex source for a given marker name.
// Accepts ASCII `_` or full-width `▁` between words, `|` or `｜` as the side
// pipe, optional whitespace inside the bracket.
func composerMarkerPatternBuilder(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	pattern := strings.Join(parts, `[_\x{2581}]`)
	return `<\s*[|\x{FF5C}]\s*` + pattern + `\s*[|\x{FF5C}]\s*>`
}

var composerToolMarkerRegexCache = map[string]*regexp.Regexp{}

func composerToolMarkerRegex(name string) *regexp.Regexp {
	if re, ok := composerToolMarkerRegexCache[name]; ok {
		return re
	}
	re := regexp.MustCompile(composerMarkerPatternBuilder(name))
	composerToolMarkerRegexCache[name] = re
	return re
}

// findComposerToolMarker locates the first occurrence of the named marker in
// `value` (or nil if absent).
func findComposerToolMarker(value, name string) *composerMarker {
	re := composerToolMarkerRegex(name)
	loc := re.FindStringIndex(value)
	if loc == nil {
		return nil
	}
	return &composerMarker{Index: loc[0], Length: loc[1] - loc[0]}
}

// canonicalizeComposerToolMarkers normalizes full-width pipe / underscore
// variants to the ASCII `<|name|>` form so downstream string-splitters work
// uniformly.
var composerCanonicalRegex = regexp.MustCompile(
	`<\s*[|\x{FF5C}]\s*(tool[_\x{2581}]calls[_\x{2581}]begin|tool[_\x{2581}]calls[_\x{2581}]end|tool[_\x{2581}]call[_\x{2581}]begin|tool[_\x{2581}]call[_\x{2581}]end|tool[_\x{2581}]sep)\s*[|\x{FF5C}]\s*>`,
)

func canonicalizeComposerToolMarkers(value string) string {
	return composerCanonicalRegex.ReplaceAllStringFunc(value, func(match string) string {
		// Extract the marker name from the match, normalize underscores.
		inner := composerCanonicalRegex.FindStringSubmatch(match)[1]
		inner = strings.ReplaceAll(inner, "▁", "_")
		return "<|" + inner + "|>"
	})
}

// parseComposerToolCalls extracts every tool call from a marker block. The
// input is the full `<|tool_calls_begin|>...<|tool_calls_end|>` segment.
// Supports parallel tool calls (multiple <|tool_call_begin|> entries).
func parseComposerToolCalls(value string) []composerToolCall {
	const (
		toolCallsBegin = "<|tool_calls_begin|>"
		toolCallsEnd   = "<|tool_calls_end|>"
		toolCallBegin  = "<|tool_call_begin|>"
		toolCallEnd    = "<|tool_call_end|>"
	)
	normalized := canonicalizeComposerToolMarkers(value)
	beginIdx := strings.Index(normalized, toolCallsBegin)
	endIdx := strings.LastIndex(normalized, toolCallsEnd)
	if beginIdx == -1 || endIdx == -1 || endIdx <= beginIdx {
		return nil
	}
	body := normalized[beginIdx+len(toolCallsBegin) : endIdx]
	var calls []composerToolCall
	offset := 0
	for {
		start := strings.Index(body[offset:], toolCallBegin)
		if start == -1 {
			break
		}
		contentStart := offset + start + len(toolCallBegin)
		end := strings.Index(body[contentStart:], toolCallEnd)
		if end == -1 {
			break
		}
		call := parseComposerToolCallBody(body[contentStart : contentStart+end])
		if call != nil {
			calls = append(calls, *call)
		}
		offset = contentStart + end + len(toolCallEnd)
	}
	return calls
}

// parseComposerToolCallBody parses a single `<|tool_call_begin|>...<|tool_call_end|>`
// body. Tries JSON form first, then key/value form, then inline form.
func parseComposerToolCallBody(value string) *composerToolCall {
	trimmed := strings.TrimSpace(value)
	if json := parseJSONToolCallBody(trimmed); json != nil {
		return json
	}

	// Key/value form: NAME<|tool_sep|>k1\nv1<|tool_sep|>k2\nv2...
	parts := strings.Split(value, "<|tool_sep|>")
	name := strings.TrimSpace(parts[0])
	parts = parts[1:]
	if name == "" {
		return nil
	}
	if len(parts) == 0 {
		if inline := parseInlineToolCall(name); inline != nil {
			return inline
		}
		return &composerToolCall{Name: name, Arguments: map[string]any{}}
	}

	args := map[string]any{}
	for _, part := range parts {
		// Each part: KEY\nVALUE (value may span multiple lines).
		trimmed := strings.TrimLeft(part, " \t")
		if trimmed == "" {
			continue
		}
		nl := strings.IndexAny(trimmed, "\r\n")
		var key, rawValue string
		if nl < 0 {
			key = strings.TrimSpace(trimmed)
		} else {
			key = strings.TrimSpace(trimmed[:nl])
			rawValue = strings.TrimSpace(trimmed[nl+1:])
		}
		if key == "" {
			continue
		}
		args[key] = parseComposerToolArgument(rawValue)
	}
	return &composerToolCall{Name: name, Arguments: args}
}

// parseJSONToolCallBody handles the JSON variant: a JSON object naming the
// tool and carrying arguments under any of the common keys.
func parseJSONToolCallBody(value string) *composerToolCall {
	if !strings.HasPrefix(value, "{") || !strings.HasSuffix(value, "}") {
		return nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil
	}
	// Function-style envelope: {"function":{"name":..,"arguments":..}}
	var fn map[string]any
	if f, ok := raw["function"].(map[string]any); ok {
		fn = f
	}
	name := firstNonEmptyString(raw["name"], raw["tool"], raw["tool_name"], raw["toolName"], fn["name"])
	if name == "" {
		return nil
	}
	var argsRaw any
	for _, k := range []string{"arguments", "args", "input", "parameters", "params"} {
		if v, ok := raw[k]; ok && v != nil {
			argsRaw = v
			break
		}
	}
	if argsRaw == nil && fn != nil {
		argsRaw = fn["arguments"]
	}
	args := recordFromToolArguments(argsRaw)
	if args == nil {
		args = map[string]any{}
	}
	return &composerToolCall{Name: name, Arguments: args}
}

func firstNonEmptyString(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func recordFromToolArguments(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	if s, ok := value.(string); ok && strings.TrimSpace(s) != "" {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return parsed
		}
	}
	return nil
}

// parseInlineToolCall handles the `name(arg=val, arg=val)` form, which some
// fine-tunes emit when there are no <|tool_sep|> markers in the body.
var inlineToolCallRegex = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)\s*(?:\(([\s\S]*)\)|\[([\s\S]*)\])?$`)

func parseInlineToolCall(value string) *composerToolCall {
	m := inlineToolCallRegex.FindStringSubmatch(strings.TrimSpace(value))
	if m == nil {
		return nil
	}
	name := strings.TrimSpace(m[1])
	if name == "" {
		return nil
	}
	rawArgs := m[2]
	if rawArgs == "" {
		rawArgs = m[3]
	}
	rawArgs = strings.TrimSpace(rawArgs)
	args := map[string]any{}
	if rawArgs != "" {
		args = parseInlineToolArguments(rawArgs)
	}
	return &composerToolCall{Name: name, Arguments: args}
}

var inlineArgRegex = regexp.MustCompile(`^([A-Za-z0-9_.\-]+)\s*[:=]\s*([\s\S]*)$`)

func parseInlineToolArguments(value string) map[string]any {
	args := map[string]any{}
	for _, part := range splitInlineArguments(value) {
		m := inlineArgRegex.FindStringSubmatch(strings.TrimSpace(part))
		if m == nil {
			continue
		}
		args[m[1]] = parseComposerToolArgument(strings.TrimSpace(m[2]))
	}
	return args
}

// splitInlineArguments splits a comma-separated argument list while respecting
// quotes and JSON-object nesting. `key=val, key={a:1,b:2}, key="x,y"` → 3 parts.
func splitInlineArguments(value string) []string {
	var parts []string
	start := 0
	var quote byte
	depth := 0
	for i := 0; i < len(value); i++ {
		c := value[i]
		if quote != 0 {
			if c == quote && (i == 0 || value[i-1] != '\\') {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
			continue
		}
		if c == '{' || c == '[' {
			depth++
		} else if c == '}' || c == ']' {
			if depth > 0 {
				depth--
			}
		} else if c == ',' && depth == 0 {
			parts = append(parts, value[start:i])
			start = i + 1
		}
	}
	parts = append(parts, value[start:])
	return parts
}

// parseComposerToolArgument coerces a raw value string to its JSON-typed
// equivalent: bool, null, number, JSON object/array, or plain string.
func parseComposerToolArgument(value string) any {
	if value == "" {
		return ""
	}
	switch value {
	case "true":
		return true
	case "false":
		return false
	case "null":
		return nil
	}
	// Numeric (integer or float).
	if isNumericLiteral(value) {
		var n float64
		if err := json.Unmarshal([]byte(value), &n); err == nil {
			return n
		}
	}
	// JSON object/array.
	if (strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}")) ||
		(strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]")) {
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err == nil {
			return parsed
		}
	}
	return value
}

var numericRegex = regexp.MustCompile(`^-?\d+(?:\.\d+)?$`)

func isNumericLiteral(s string) bool {
	return numericRegex.MatchString(s)
}

// composerThinkingExtractor mirrors composer-api's ThinkingTextExtractor
// (cursor.ts:906). Composer-2.5 emits ALL output as ConversationMessage.Thinking
// frames (inner field 25), with the visible reply embedded after a literal
// </think> or <|final|> marker that may span multiple frames.
//
// While `open` (i.e., no final-marker has been seen yet), Push buffers thinking
// content and scans the buffer on each Push for the LAST occurrence of any
// control token (the reference uses last-match-wins, cursor.ts:635-642).
// On a match: emit any preceding buffered content as thinking, flip to closed,
// and return the post-marker suffix as visible text. After close, Push passes
// content through verbatim as visible text.
//
// Flush at EOF:
//   - if still open with no marker: discards buffer (matches composer-api's
//     "unfinished thinking is model-internal", cursor.ts:929-930)
//   - if a marker had been seen mid-stream: returns empty (the post-marker
//     content was already emitted)
type composerThinkingExtractor struct {
	buf  strings.Builder
	open bool
}

// composerControlTokenPattern matches </think>, <|final|>, full-width variants,
// and any whitespace-padded forms. Matches cursor.ts:20.
var composerControlTokenPattern = regexp.MustCompile(`</think>|<\s*[|\x{FF5C}]\s*final\s*[|\x{FF5C}]\s*>`)

func newComposerThinkingExtractor() *composerThinkingExtractor {
	return &composerThinkingExtractor{open: true}
}

// Push returns (thinking-text chunks emitted, visible-text chunks emitted) for
// this delta. The buffered state may carry partial-marker bytes between calls.
func (e *composerThinkingExtractor) Push(delta string) ([]string, []string) {
	if !e.open {
		// Already past the final marker — pass through as visible text.
		if delta == "" {
			return nil, nil
		}
		return nil, []string{delta}
	}
	if delta == "" {
		return nil, nil
	}
	e.buf.WriteString(delta)
	return e.scanForMarker()
}

func (e *composerThinkingExtractor) scanForMarker() ([]string, []string) {
	current := e.buf.String()

	// Find the LAST occurrence of any marker (matches cursor.ts:635-642).
	matches := composerControlTokenPattern.FindAllStringIndex(current, -1)
	if len(matches) == 0 {
		// No complete marker yet. Hold back the longest suffix that could be
		// the start of any control-token candidate so cross-chunk splits work.
		keep := controlTokenPrefixLength(current)
		if keep > 0 {
			emit := current[:len(current)-keep]
			e.buf.Reset()
			e.buf.WriteString(current[len(current)-keep:])
			if emit == "" {
				return nil, nil
			}
			return []string{emit}, nil
		}
		// No suspicious suffix — emit whole buffer as thinking; the buffer
		// stays empty until the next Push.
		e.buf.Reset()
		return []string{current}, nil
	}

	last := matches[len(matches)-1]
	prefix := current[:last[0]]
	suffix := current[last[1]:]
	// Strip leading whitespace introduced by the marker (composer-api does
	// `.replace(/^\s+/, "")` at cursor.ts:916).
	suffix = strings.TrimLeft(suffix, " \t\r\n")

	e.buf.Reset()
	e.open = false

	var thinking []string
	var visible []string
	if prefix != "" {
		thinking = append(thinking, prefix)
	}
	if suffix != "" {
		visible = append(visible, suffix)
	}
	return thinking, visible
}

// Flush drains any residual buffered content at end of stream. Per
// composer-api semantics (cursor.ts:921-931): if `open` and no marker was
// seen, the buffer is discarded (unfinished thinking is model-internal). If
// `open` but a marker turns up in the buffer at flush, behave like Push.
// If already closed, return empty.
func (e *composerThinkingExtractor) Flush() string {
	if !e.open {
		e.buf.Reset()
		return ""
	}
	current := e.buf.String()
	e.buf.Reset()
	matches := composerControlTokenPattern.FindAllStringIndex(current, -1)
	if len(matches) == 0 {
		// Discard — composer-api does this at cursor.ts:929-930.
		return ""
	}
	last := matches[len(matches)-1]
	suffix := strings.TrimLeft(current[last[1]:], " \t\r\n")
	e.open = false
	return suffix
}
