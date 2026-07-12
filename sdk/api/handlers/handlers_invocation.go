package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"golang.org/x/net/context"
)

type executionIdentityContextKey struct{}

// WithExecutionIdentity stores the issued execution identity on the request context.
func WithExecutionIdentity(ctx context.Context, identity coreexecutor.ExecutionIdentity) context.Context {
	if strings.TrimSpace(identity.InvocationID) == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, executionIdentityContextKey{}, identity)
}

func executionIdentityFromContext(ctx context.Context) (coreexecutor.ExecutionIdentity, bool) {
	if ctx == nil {
		return coreexecutor.ExecutionIdentity{}, false
	}
	raw := ctx.Value(executionIdentityContextKey{})
	identity, ok := raw.(coreexecutor.ExecutionIdentity)
	if !ok || strings.TrimSpace(identity.InvocationID) == "" {
		return coreexecutor.ExecutionIdentity{}, false
	}
	return identity, true
}

// IssueExecutionIdentity resolves or generates an invocation identity at the API
// boundary and stores it on the gin/request context for the rest of the turn.
func IssueExecutionIdentity(ctx context.Context, rawJSON []byte) (context.Context, coreexecutor.ExecutionIdentity, error) {
	if identity, ok := executionIdentityFromContext(ctx); ok {
		return ctx, identity, nil
	}
	headers := headersFromContext(ctx)
	identity, err := coreexecutor.ResolveExecutionIdentity(headers, rawJSON)
	if err != nil {
		return ctx, coreexecutor.ExecutionIdentity{}, err
	}
	ctx = WithExecutionIdentity(ctx, identity)
	if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil {
		ginCtx.Set("cliproxy_execution_identity", identity)
		if !ginCtx.Writer.Written() && shouldExposeInvocationIdentity(headers, identity) {
			ginCtx.Writer.Header().Set(coreexecutor.HeaderCLIProxyInvocationID, identity.InvocationID)
			if coreexecutor.HasCapability(headers, coreexecutor.CapabilityInvocationIDV1) {
				appendCapabilityAdvertise(ginCtx.Writer.Header(), coreexecutor.CapabilityInvocationIDV1)
			}
			if coreexecutor.HasCapability(headers, coreexecutor.CapabilityStreamResumeV1) {
				appendCapabilityAdvertise(ginCtx.Writer.Header(), coreexecutor.CapabilityStreamResumeV1)
			}

		}
	}
	return ctx, identity, nil
}

func shouldExposeInvocationIdentity(headers http.Header, identity coreexecutor.ExecutionIdentity) bool {
	if strings.TrimSpace(identity.InvocationID) == "" {
		return false
	}
	if !identity.ServerIssued {
		return true
	}
	return coreexecutor.HasCapability(headers, coreexecutor.CapabilityInvocationIDV1)
}

func appendCapabilityAdvertise(header http.Header, capability string) {
	if header == nil || capability == "" {
		return
	}
	existing := strings.TrimSpace(header.Get(coreexecutor.HeaderCLIProxyCapabilities))
	if existing == "" {
		header.Set(coreexecutor.HeaderCLIProxyCapabilities, capability)
		return
	}
	for _, part := range strings.Split(existing, ",") {
		if strings.EqualFold(strings.TrimSpace(part), capability) {
			return
		}
	}
	header.Set(coreexecutor.HeaderCLIProxyCapabilities, existing+", "+capability)
}

func attachExecutionIdentity(ctx context.Context, meta map[string]any, headers http.Header, rawJSON []byte) (context.Context, map[string]any, http.Header, coreexecutor.ExecutionIdentity, error) {
	ctx, identity, err := IssueExecutionIdentity(ctx, rawJSON)
	if err != nil {
		return ctx, meta, headers, coreexecutor.ExecutionIdentity{}, err
	}
	meta = coreexecutor.ApplyExecutionIdentityMetadata(meta, identity)
	headers = coreexecutor.EnsureInvocationHeader(headers, identity)
	return ctx, meta, headers, identity, nil
}

func invocationHandshakeResponse(identity coreexecutor.ExecutionIdentity) ([]byte, http.Header, *interfaces.ErrorMessage) {
	outcome := coreexecutor.NewInvocationHandshakeOutcome(identity)
	body, err := json.Marshal(outcome)
	if err != nil {
		return nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: err}
	}
	headers := make(http.Header)
	headers.Set(coreexecutor.HeaderCLIProxyInvocationID, identity.InvocationID)
	headers.Set("Content-Type", "application/json")
	appendCapabilityAdvertise(headers, coreexecutor.CapabilityInvocationIDV1)
	return body, headers, nil
}

func maybeInvocationHandshake(ctx context.Context, rawJSON []byte, stream bool) (context.Context, []byte, http.Header, *interfaces.ErrorMessage, bool) {
	if stream {
		return ctx, nil, nil, nil, false
	}
	headers := headersFromContext(ctx)
	if !coreexecutor.PrefersInvocationHandshake(headers) {
		return ctx, nil, nil, nil, false
	}
	// Handshake is available to capable clients and to Prefer-only callers.
	ctx, identity, err := IssueExecutionIdentity(ctx, rawJSON)
	if err != nil {
		return ctx, nil, nil, &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: err}, true
	}
	body, outHeaders, errMsg := invocationHandshakeResponse(identity)
	return ctx, body, outHeaders, errMsg, true
}

// seedStreamInvocationHeaders builds response headers that expose invocation
// identity before ExecuteStream / bootstrap so capable clients can observe the
// ID even if upstream connect fails.
func seedStreamInvocationHeaders(requestHeaders http.Header, identity coreexecutor.ExecutionIdentity) http.Header {
	if !shouldExposeInvocationIdentity(requestHeaders, identity) {
		return nil
	}
	headers := mergeInvocationResponseHeaders(nil, identity)
	if coreexecutor.HasCapability(requestHeaders, coreexecutor.CapabilityInvocationIDV1) {
		appendCapabilityAdvertise(headers, coreexecutor.CapabilityInvocationIDV1)
	}
	return headers
}

// preserveInvocationHeaders re-applies invocation identity onto dst after a
// header replace (e.g. bootstrap retry) so the early-issued ID is not dropped.
// When dst is non-nil it is mutated in place to keep caller map identity.
func preserveInvocationHeaders(dst http.Header, requestHeaders http.Header, identity coreexecutor.ExecutionIdentity) http.Header {
	if !shouldExposeInvocationIdentity(requestHeaders, identity) {
		return dst
	}
	if dst == nil {
		dst = make(http.Header)
	}
	if strings.TrimSpace(dst.Get(coreexecutor.HeaderCLIProxyInvocationID)) == "" {
		dst.Set(coreexecutor.HeaderCLIProxyInvocationID, identity.InvocationID)
	}
	return dst
}

func mergeInvocationResponseHeaders(dst http.Header, identity coreexecutor.ExecutionIdentity) http.Header {
	if identity.InvocationID == "" {
		return dst
	}
	if dst == nil {
		dst = make(http.Header)
	} else {
		dst = dst.Clone()
	}
	if strings.TrimSpace(dst.Get(coreexecutor.HeaderCLIProxyInvocationID)) == "" {
		dst.Set(coreexecutor.HeaderCLIProxyInvocationID, identity.InvocationID)
	}
	return dst
}

func firstStreamInvocationControlEvent(identity coreexecutor.ExecutionIdentity, headers http.Header) []byte {
	if identity.InvocationID == "" {
		return nil
	}
	if !coreexecutor.HasCapability(headers, coreexecutor.CapabilityInvocationIDV1) {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"object":        "cliproxy.invocation",
		"invocation_id": identity.InvocationID,
		"server_issued": identity.ServerIssued,
	})
	if err != nil {
		return nil
	}
	var b strings.Builder
	b.WriteString("event: cliproxy.invocation\n")
	b.WriteString("data: ")
	b.Write(payload)
	b.WriteString("\n\n")
	return []byte(b.String())
}
