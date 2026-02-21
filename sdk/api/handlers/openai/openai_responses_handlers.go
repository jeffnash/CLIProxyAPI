// Package openai provides HTTP handlers for OpenAIResponses API endpoints.
// This package implements the OpenAIResponses-compatible API interface, including model listing
// and chat completion functionality. It supports both streaming and non-streaming responses,
// and manages a pool of clients to interact with backend services.
// The handlers translate OpenAIResponses API requests to the appropriate backend format and
// convert responses back to OpenAIResponses-compatible format.
package openai

import (
	"bytes"
	"context"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v6/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v6/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// OpenAIResponsesAPIHandler contains the handlers for OpenAIResponses API endpoints.
// It holds a pool of clients to interact with the backend service.
type OpenAIResponsesAPIHandler struct {
	*handlers.BaseAPIHandler
}

type responsesSSEWriteState struct {
	wroteNonEmptyData   bool   // true if we've ever written non-empty data (for suppressing leading delimiters)
	currentEventHasData bool   // true if current event block has non-empty data since last boundary
	lastWasDelimiter    bool   // true if last write was a delimiter
	pendingEventLine    []byte // buffered event: line, written only when non-empty data arrives
}

func (st *responsesSSEWriteState) writeLine(w http.ResponseWriter, line []byte) {
	// Normalize CRLF.
	line = bytes.TrimSuffix(line, []byte("\r"))

	// Treat empty line as an SSE delimiter.
	if len(line) == 0 {
		// Suppress leading delimiters (before any data) and delimiters for event-only blocks.
		if !st.wroteNonEmptyData || !st.currentEventHasData {
			// Clear pending event line since this event block had no data.
			st.pendingEventLine = nil
			return
		}
		_, _ = w.Write([]byte("\n"))
		st.lastWasDelimiter = true
		st.currentEventHasData = false // reset per-event flag at boundary
		st.pendingEventLine = nil
		return
	}

	// Buffer event: lines until we see non-empty data.
	if bytes.HasPrefix(line, []byte("event:")) {
		st.pendingEventLine = append([]byte(nil), line...) // copy
		return
	}

	// Filter empty data: payloads to prevent downstream json.loads("") errors.
	// Empty "data:" lines would cause SSE decoders to emit events with data="" which fails JSON parsing.
	if bytes.HasPrefix(line, []byte("data:")) {
		if len(bytes.TrimSpace(line[5:])) == 0 {
			return
		}
		// Non-empty data: flush pending event line first.
		if st.pendingEventLine != nil {
			// Inject delimiter before event if we have prior data and not already at boundary.
			if st.currentEventHasData && !st.lastWasDelimiter {
				_, _ = w.Write([]byte("\n"))
			}
			_, _ = w.Write(st.pendingEventLine)
			_, _ = w.Write([]byte("\n"))
			st.pendingEventLine = nil
			st.lastWasDelimiter = false
		}
		st.wroteNonEmptyData = true
		st.currentEventHasData = true
	}

	_, _ = w.Write(line)
	_, _ = w.Write([]byte("\n"))

	st.lastWasDelimiter = false
}

func (st *responsesSSEWriteState) writeChunk(w http.ResponseWriter, chunk []byte) {
	// Handle chunks that contain multiple SSE lines (some translators emit "event: ...\ndata: ...").
	// Split on newline and run the line-level logic per line so state tracking works correctly.
	//
	// Special case: if chunk ends with a single \n (but not \n\n), drop the trailing empty segment
	// to avoid treating it as a delimiter. This preserves buffered event lines for subsequent data.
	// - "event: foo\n" splits to ["event: foo", ""] - drop the trailing ""
	// - "event: foo\n\n" splits to ["event: foo", "", ""] - keep one "" as delimiter
	// - "" (empty chunk from upstream) - honor as explicit delimiter signal
	lines := bytes.Split(chunk, []byte("\n"))

	// If chunk is empty, pass through as-is (explicit delimiter from upstream).
	if len(chunk) == 0 {
		st.writeLine(w, chunk)
		return
	}

	// Drop trailing empty segment from single trailing newline, but keep for double newline.
	// Double newline produces [..., "", ""], so we check if last two are both empty.
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		if len(lines) < 2 || len(lines[len(lines)-2]) != 0 {
			// Single trailing newline - drop the empty segment
			lines = lines[:len(lines)-1]
		}
	}

	for i := range lines {
		st.writeLine(w, lines[i])
	}
}

func (st *responsesSSEWriteState) writeDone(w http.ResponseWriter) {
	// Only emit a delimiter if the current event block has non-empty data and we're not already
	// at an event boundary. This avoids dispatching empty SSE events downstream.
	if !st.currentEventHasData || st.lastWasDelimiter {
		return
	}
	_, _ = w.Write([]byte("\n"))
	st.lastWasDelimiter = true
	st.currentEventHasData = false
}

// NewOpenAIResponsesAPIHandler creates a new OpenAIResponses API handlers instance.
// It takes an BaseAPIHandler instance as input and returns an OpenAIResponsesAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handlers instance
//
// Returns:
//   - *OpenAIResponsesAPIHandler: A new OpenAIResponses API handlers instance
func NewOpenAIResponsesAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIResponsesAPIHandler {
	return &OpenAIResponsesAPIHandler{
		BaseAPIHandler: apiHandlers,
	}
}

// HandlerType returns the identifier for this handler implementation.
func (h *OpenAIResponsesAPIHandler) HandlerType() string {
	return OpenaiResponse
}

// Models returns the OpenAIResponses-compatible model metadata supported by this handler.
func (h *OpenAIResponsesAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("openai")
}

// OpenAIResponsesModels handles the /v1/models endpoint.
// It returns a list of available AI models with their capabilities
// and specifications in OpenAIResponses-compatible format.
func (h *OpenAIResponsesAPIHandler) OpenAIResponsesModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   h.Models(),
	})
}

// Responses handles the /v1/responses endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *OpenAIResponsesAPIHandler) Responses(c *gin.Context) {
	rawJSON, err := c.GetRawData()
	// If data retrieval fails, return a 400 Bad Request error.
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		h.handleStreamingResponse(c, rawJSON)
	} else {
		h.handleNonStreamingResponse(c, rawJSON)
	}

}

func (h *OpenAIResponsesAPIHandler) Compact(c *gin.Context) {
	rawJSON, err := c.GetRawData()
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}

	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported for compact responses",
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if streamResult.Exists() {
		if updated, err := sjson.DeleteBytes(rawJSON, "stream"); err == nil {
			rawJSON = updated
		}
	}

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "responses/compact")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleNonStreamingResponse handles non-streaming chat completion responses
// for Gemini models. It selects a client from the pool, sends the request, and
// aggregates the response before sending it back to the client in OpenAIResponses format.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleStreamingResponse handles streaming responses for Gemini models.
// It establishes a streaming connection with the backend service and forwards
// the response chunks to the client in real-time using Server-Sent Events.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleStreamingResponse(c *gin.Context, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	// New core execution path
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	dataChan, upstreamHeaders, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache, no-transform")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
		if h.Cfg.Streaming.DisableProxyBuffering {
			c.Header("X-Accel-Buffering", "no") // Disable proxy buffering for SSE
		}
	}

	// Peek at the first chunk
	writeState := &responsesSSEWriteState{}
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case errMsg, ok := <-errChan:
			if !ok {
				// Err channel closed cleanly; wait for data channel.
				errChan = nil
				continue
			}
			// Upstream failed immediately. Return proper error status and JSON.
			h.WriteErrorResponse(c, errMsg)
			if errMsg != nil {
				cliCancel(errMsg.Error)
			} else {
				cliCancel(nil)
			}
			return
		case chunk, ok := <-dataChan:
			if !ok {
				// Stream closed without data? Send headers and done.
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
				_, _ = c.Writer.Write([]byte("\n"))
				flusher.Flush()
				cliCancel(nil)
				return
			}

			// Success! Set headers.
			setSSEHeaders()
			handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)

			// Write first chunk logic (matching forwardResponsesStream)
			writeState.writeChunk(c.Writer, chunk)
			flusher.Flush()

			// Continue
			h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, dataChan, errChan, writeState)
			return
		}
	}
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, writeState *responsesSSEWriteState) {
	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		WriteChunk: func(chunk []byte) {
			writeState.writeChunk(c.Writer, chunk)
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			if errMsg == nil {
				return
			}
			status := http.StatusInternalServerError
			if errMsg.StatusCode > 0 {
				status = errMsg.StatusCode
			}
			errText := http.StatusText(status)
			if errMsg.Error != nil && errMsg.Error.Error() != "" {
				errText = errMsg.Error.Error()
			}
			body := handlers.BuildErrorResponseBody(status, errText)
			// Write error as a well-formed SSE event without emitting a leading delimiter that could
			// be interpreted as an empty event by downstream clients.
			writeState.writeChunk(c.Writer, []byte("event: error\ndata: "+string(body)))
			writeState.writeChunk(c.Writer, []byte(""))
		},
		WriteDone: func() {
			writeState.writeDone(c.Writer)
		},
	})
}
