package responses

import (
	"bytes"
	"context"
	"strings"

	translatorcommon "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/common"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexResponsesSSEState struct {
	pendingEventLine             string
	hasNonEmptyDataSinceBoundary bool
}

// ConvertCodexResponseToOpenAIResponses converts OpenAI Chat Completions streaming chunks
// to OpenAI Responses SSE events (response.*).

func ConvertCodexResponseToOpenAIResponses(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	// If the caller doesn't provide a persistent param state, pass framing lines
	// through without stateful buffering.
	if param == nil {
		rawJSON = bytes.TrimSuffix(rawJSON, []byte("\r"))
		if bytes.HasPrefix(rawJSON, []byte("data:")) {
			payload := bytes.TrimSpace(rawJSON[5:])
			if len(payload) == 0 {
				return [][]byte{}
			}
			payload = enrichResponsesDataPayload(payload, modelName, originalRequestRawJSON, requestRawJSON)
			out := make([]byte, 0, len(payload)+len("data: "))
			out = append(out, []byte("data: ")...)
			out = append(out, payload...)
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
		payload := bytes.TrimSpace(rawJSON[5:])
		if len(payload) == 0 {
			st.pendingEventLine = ""
			st.hasNonEmptyDataSinceBoundary = false
			return [][]byte{}
		}
		payload = enrichResponsesDataPayload(payload, modelName, originalRequestRawJSON, requestRawJSON)
		out := make([]byte, 0, len(payload)+len("data: "))
		out = append(out, []byte("data: ")...)
		out = append(out, payload...)
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

// enrichResponsesDataPayload applies per-payload enrichment for lifecycle events:
// it echoes the original request instructions and backfills response.model from
// the request when the upstream event omits it.
func enrichResponsesDataPayload(payload []byte, modelName string, originalRequestRawJSON, requestRawJSON []byte) []byte {
	if typeResult := gjson.GetBytes(payload, "type"); typeResult.Exists() {
		typeStr := typeResult.String()
		if typeStr == "response.created" || typeStr == "response.in_progress" || typeStr == "response.completed" {
			if gjson.GetBytes(payload, "response.instructions").Exists() {
				instructions := gjson.GetBytes(originalRequestRawJSON, "instructions").String()
				payload, _ = sjson.SetBytes(payload, "response.instructions", instructions)
			}
		}
	}
	return setResponsesModel(payload, modelName, originalRequestRawJSON, requestRawJSON)
}

func setResponsesModel(rawJSON []byte, _ string, originalRequestRawJSON, requestRawJSON []byte) []byte {
	eventType := gjson.GetBytes(rawJSON, "type").String()
	if eventType != "response.created" && eventType != "response.in_progress" {
		return rawJSON
	}
	if gjson.GetBytes(rawJSON, "response.model").Exists() {
		return rawJSON
	}

	// Only backfill the model when the requests identify one. A bare executor
	// model name is intentionally not used here so that payloads without any
	// request context keep their exact upstream bytes.
	requestModelName := translatorcommon.RequestModelName(originalRequestRawJSON, requestRawJSON)
	if requestModelName == "" {
		return rawJSON
	}

	updated, errSet := sjson.SetBytes(rawJSON, "response.model", requestModelName)
	if errSet != nil {
		return rawJSON
	}
	return updated
}

// ConvertCodexResponseToOpenAIResponsesNonStream builds a single Responses JSON
// from a non-streaming OpenAI Chat Completions response.
func ConvertCodexResponseToOpenAIResponsesNonStream(_ context.Context, _ string, _, _, rawJSON []byte, _ *any) []byte {
	rootResult := gjson.ParseBytes(rawJSON)
	// Verify this is a terminal response event.
	responseType := rootResult.Get("type").String()
	if responseType != "response.completed" && responseType != "response.incomplete" {
		return []byte{}
	}
	responseResult := rootResult.Get("response")
	return []byte(responseResult.Raw)
}
