package responses

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexResponsesSSEState struct {
	pendingEventLine             string
	hasNonEmptyDataSinceBoundary bool
}

// ConvertCodexResponseToOpenAIResponses converts OpenAI Chat Completions streaming chunks
// to OpenAI Responses SSE events (response.*).

func ConvertCodexResponseToOpenAIResponses(_ context.Context, _ string, originalRequestRawJSON, _, rawJSON []byte, param *any) [][]byte {
	// If the caller doesn't provide a persistent param state, pass framing lines
	// through without stateful buffering.
	if param == nil {
		rawJSON = bytes.TrimSuffix(rawJSON, []byte("\r"))
		if bytes.HasPrefix(rawJSON, []byte("data:")) {
			rawJSON = bytes.TrimSpace(rawJSON[5:])
			if len(rawJSON) == 0 {
				return [][]byte{}
			}
			if typeResult := gjson.GetBytes(rawJSON, "type"); typeResult.Exists() {
				typeStr := typeResult.String()
				if typeStr == "response.created" || typeStr == "response.in_progress" || typeStr == "response.completed" {
					if gjson.GetBytes(rawJSON, "response.instructions").Exists() {
						instructions := gjson.GetBytes(originalRequestRawJSON, "instructions").String()
						rawJSON, _ = sjson.SetBytes(rawJSON, "response.instructions", instructions)
					}
				}
			}
			out := fmt.Appendf(nil, "data: %s", rawJSON)
			return [][]byte{out}
		}
		return [][]byte{rawJSON}
	}

	var st *codexResponsesSSEState
	if *param == nil {
		*param = &codexResponsesSSEState{}
	}
	st = (*param).(*codexResponsesSSEState)

	rawJSON = bytes.TrimSuffix(rawJSON, []byte("\r"))

	if len(rawJSON) == 0 {
		if st.hasNonEmptyDataSinceBoundary {
			st.hasNonEmptyDataSinceBoundary = false
			st.pendingEventLine = ""
			return [][]byte{[]byte{}}
		}
		st.pendingEventLine = ""
		return [][]byte{}
	}

	if bytes.HasPrefix(rawJSON, []byte("event:")) {
		st.pendingEventLine = strings.TrimRight(string(rawJSON), "\r")
		return [][]byte{}
	}

	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
		if len(rawJSON) == 0 {
			st.pendingEventLine = ""
			st.hasNonEmptyDataSinceBoundary = false
			return [][]byte{}
		}
		if typeResult := gjson.GetBytes(rawJSON, "type"); typeResult.Exists() {
			typeStr := typeResult.String()
			if typeStr == "response.created" || typeStr == "response.in_progress" || typeStr == "response.completed" {
				if gjson.GetBytes(rawJSON, "response.instructions").Exists() {
					instructions := gjson.GetBytes(originalRequestRawJSON, "instructions").String()
					rawJSON, _ = sjson.SetBytes(rawJSON, "response.instructions", instructions)
				}
			}
		}
		out := fmt.Appendf(nil, "data: %s", rawJSON)
		st.hasNonEmptyDataSinceBoundary = true
		if st.pendingEventLine != "" {
			eventLine := st.pendingEventLine
			st.pendingEventLine = ""
			return [][]byte{[]byte(eventLine), out}
		}
		return [][]byte{out}
	}
	return [][]byte{rawJSON}
}

// ConvertCodexResponseToOpenAIResponsesNonStream builds a single Responses JSON
// from a non-streaming OpenAI Chat Completions response.
func ConvertCodexResponseToOpenAIResponsesNonStream(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) []byte {
	rootResult := gjson.ParseBytes(rawJSON)
	// Verify this is a response.completed event
	if rootResult.Get("type").String() != "response.completed" {
		return []byte{}
	}
	responseResult := rootResult.Get("response")
	return []byte(responseResult.Raw)
}
