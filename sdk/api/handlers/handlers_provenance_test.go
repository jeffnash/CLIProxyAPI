package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/turnprovenance"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestWriteErrorResponseRendersTypedProvenanceClarification(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &BaseAPIHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(coreexecutor.HeaderCLIProxyCapabilities, coreexecutor.CapabilityProvenanceClarificationV1)
	c.Request = req

	cerr := &turnprovenance.ClarificationError{
		InvocationID:    "inv1_test",
		ResolutionToken: "tok",
		Decision: turnprovenance.Decision{
			Candidates: []turnprovenance.Segment{{ID: "a", OriginalIndex: 0}, {ID: "b", OriginalIndex: 1}},
		},
	}
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: http.StatusUnprocessableEntity, Error: cerr})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", w.Code)
	}
	var out coreexecutor.ProtocolOutcome
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body=%s err=%v", w.Body.String(), err)
	}
	if out.Object != "cliproxy.provenance_clarification" || out.ResolutionToken != "tok" || len(out.Candidates) != 2 {
		t.Fatalf("outcome=%+v", out)
	}
}

func TestWriteErrorResponseLegacyClarificationKeepsEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &BaseAPIHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	cerr := &turnprovenance.ClarificationError{InvocationID: "inv1_test", Message: "need clarification"}
	handler.WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: http.StatusUnprocessableEntity, Error: cerr})
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", w.Code)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Fatalf("legacy body not json: %s", w.Body.String())
	}
	var out coreexecutor.ProtocolOutcome
	if err := json.Unmarshal(w.Body.Bytes(), &out); err == nil && out.Object == "cliproxy.provenance_clarification" {
		t.Fatal("legacy client must not receive typed ProtocolOutcome object")
	}
}
