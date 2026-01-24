package responses

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/misc"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexResponsesSSEState struct {
	pendingEventLine             string
	hasNonEmptyDataSinceBoundary bool
}

// ConvertCodexResponseToOpenAIResponses converts OpenAI Chat Completions streaming chunks
// to OpenAI Responses SSE events (response.*).

func ConvertCodexResponseToOpenAIResponses(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []string {
	// If the caller doesn't provide a persistent param state (nil), do not buffer SSE framing lines.
	// In that case, pass through "event:" lines as-is to avoid silently breaking framing.
	// Stateful buffering is only enabled when param is non-nil so event/data/delimiter coordination
	// can be maintained across calls.
	if param == nil {
		// Normalize CRLF SSE lines from bufio.Scanner (it splits on '\n' but may leave a trailing '\r').
		rawJSON = bytes.TrimSuffix(rawJSON, []byte("\r"))

		if bytes.HasPrefix(rawJSON, []byte("data:")) {
			rawJSON = bytes.TrimSpace(rawJSON[5:])
			if len(rawJSON) == 0 {
				return []string{}
			}
			if typeResult := gjson.GetBytes(rawJSON, "type"); typeResult.Exists() {
				typeStr := typeResult.String()
				if typeStr == "response.created" || typeStr == "response.in_progress" || typeStr == "response.completed" {
					if gjson.GetBytes(rawJSON, "response.instructions").Exists() {
						instructions := selectInstructions(originalRequestRawJSON, requestRawJSON)
						rawJSON, _ = sjson.SetBytes(rawJSON, "response.instructions", instructions)
					}
				}
			}
			out := fmt.Sprintf("data: %s", string(rawJSON))
			return []string{out}
		}
		return []string{string(rawJSON)}
	}

	var st *codexResponsesSSEState
	if *param == nil {
		*param = &codexResponsesSSEState{}
	}
	st = (*param).(*codexResponsesSSEState)

	// Normalize CRLF SSE lines from bufio.Scanner (it splits on '\n' but may leave a trailing '\r').
	// If not normalized, blank delimiter lines can become "\r" and break downstream SSE framing.
	rawJSON = bytes.TrimSuffix(rawJSON, []byte("\r"))

	// Only emit delimiter/blank lines after we've emitted a non-empty data payload.
	// Otherwise, downstream SSE decoders may dispatch empty events with data="".
	if len(rawJSON) == 0 {
		if st.hasNonEmptyDataSinceBoundary {
			st.hasNonEmptyDataSinceBoundary = false
			st.pendingEventLine = ""
			return []string{""}
		}
		st.pendingEventLine = ""
		return []string{}
	}

	// Track event: lines and only emit them together with a subsequent non-empty data: payload.
	// This prevents "event:" + delimiter sequences from dispatching empty events downstream.
	if bytes.HasPrefix(rawJSON, []byte("event:")) {
		st.pendingEventLine = strings.TrimRight(string(rawJSON), "\r")
		return []string{}
	}

	// Handle data: lines specially - we need to validate they have content
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
		// Skip empty data payloads to avoid JSON parse errors in downstream clients.
		// Empty "data:" lines would create SSE events with data="" which fails json.loads("").
		if len(rawJSON) == 0 {
			st.pendingEventLine = ""
			st.hasNonEmptyDataSinceBoundary = false
			return []string{}
		}
		if typeResult := gjson.GetBytes(rawJSON, "type"); typeResult.Exists() {
			typeStr := typeResult.String()
			if typeStr == "response.created" || typeStr == "response.in_progress" || typeStr == "response.completed" {
				if gjson.GetBytes(rawJSON, "response.instructions").Exists() {
					instructions := selectInstructions(originalRequestRawJSON, requestRawJSON)
					rawJSON, _ = sjson.SetBytes(rawJSON, "response.instructions", instructions)
				}
			}
		}
		out := fmt.Sprintf("data: %s", string(rawJSON))
		st.hasNonEmptyDataSinceBoundary = true
		if st.pendingEventLine != "" {
			eventLine := st.pendingEventLine
			st.pendingEventLine = ""
			return []string{eventLine, out}
		}
		return []string{out}
	}
	// Pass through all other SSE lines (empty lines as event delimiters, event: lines, comments).
	// These are necessary for proper SSE framing and client parsing.
	return []string{string(rawJSON)}
}

// ConvertCodexResponseToOpenAIResponsesNonStream builds a single Responses JSON
// from a non-streaming OpenAI Chat Completions response.
func ConvertCodexResponseToOpenAIResponsesNonStream(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) string {
	rootResult := gjson.ParseBytes(rawJSON)
	// Verify this is a response.completed event
	if rootResult.Get("type").String() != "response.completed" {
		return ""
	}
	responseResult := rootResult.Get("response")
	template := responseResult.Raw
	if responseResult.Get("instructions").Exists() {
		template, _ = sjson.Set(template, "instructions", selectInstructions(originalRequestRawJSON, requestRawJSON))
	}
	return template
}

func selectInstructions(originalRequestRawJSON, requestRawJSON []byte) string {
	userAgent := misc.ExtractCodexUserAgent(originalRequestRawJSON)
	if misc.IsOpenCodeUserAgent(userAgent) {
		return gjson.GetBytes(requestRawJSON, "instructions").String()
	}
	return gjson.GetBytes(originalRequestRawJSON, "instructions").String()
}
