package executor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// Metadata and header contracts for invocation identity.
const (
	InvocationIDMetadataKey           = "invocation_id"
	ClientIdempotencyKeyMetadataKey   = "idempotency_key"
	TenantIDMetadataKey               = "tenant_id"
	ConversationIDMetadataKey         = "conversation_id"
	CanonicalRequestHashMetadataKey   = "canonical_request_hash"
	InvocationServerIssuedMetadataKey = "invocation_server_issued"

	HeaderIdempotencyKey       = "Idempotency-Key"
	HeaderCLIProxyInvocationID = "X-CLIProxy-Invocation-ID"
	HeaderClientTurnID         = "X-Client-Turn-ID"
	HeaderCLIProxyCapabilities = "X-CLIProxy-Capabilities"
	HeaderPrefer               = "Prefer"

	CapabilityInvocationIDV1            = "invocation-id-v1"
	CapabilityProvenanceClarificationV1 = "provenance-clarification-v1"
	CapabilityStreamResumeV1            = "stream-resume-v1"
	PreferInvocationHandshake           = "invocation-handshake"
)

var invocationIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{7,255}$`)

// ExecutionIdentity binds one logical provider invocation across retries and recovery.
type ExecutionIdentity struct {
	InvocationID         string
	ClientIdempotencyKey string
	TenantID             string
	ConversationID       string
	CanonicalRequestHash string
	// ServerIssued is true when the server generated InvocationID because the
	// client supplied none. A server-issued ID is not proof that a later
	// anonymous request is a retry until the client echoes it.
	ServerIssued bool
}

// ProtocolOutcomeKind identifies a typed non-model protocol result.
type ProtocolOutcomeKind string

const (
	ProtocolOutcomeProvenanceClarification ProtocolOutcomeKind = "provenance_clarification"
	ProtocolOutcomeInvocationHandshake     ProtocolOutcomeKind = "invocation_handshake"
)

// ProtocolCandidateSegment is one candidate presented during typed clarification.
type ProtocolCandidateSegment struct {
	ID            string `json:"id"`
	Digest        string `json:"digest,omitempty"`
	OriginalIndex int    `json:"original_index"`
	Preview       string `json:"preview,omitempty"`
}

// ProtocolOutcome is a typed protocol result that must not be mistaken for a
// model completion. Clarification paths advertise no tools and call no provider.
type ProtocolOutcome struct {
	Object            string                     `json:"object"`
	Kind              ProtocolOutcomeKind        `json:"kind"`
	StopReason        string                     `json:"stop_reason,omitempty"`
	InvocationID      string                     `json:"invocation_id,omitempty"`
	Candidates        []ProtocolCandidateSegment `json:"candidates,omitempty"`
	ResolutionToken   string                     `json:"resolution_token,omitempty"`
	RetryAfterSeconds int                        `json:"retry_after_seconds,omitempty"`
	Instructions      string                     `json:"instructions,omitempty"`
}

// ValidInvocationID reports whether id matches the shared invocation identity grammar.
func ValidInvocationID(id string) bool {
	id = strings.TrimSpace(id)
	return invocationIDPattern.MatchString(id)
}

// GenerateInvocationID returns a cryptographically random invocation ID.
// It fails closed when the CSPRNG is unavailable — callers must not invent a placeholder ID.
func GenerateInvocationID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate invocation id: %w", err)
	}
	return "inv1_" + hex.EncodeToString(b), nil
}

// HasCapability reports whether the capability token is advertised in header values.
func HasCapability(header http.Header, capability string) bool {
	capability = strings.TrimSpace(capability)
	if capability == "" || header == nil {
		return false
	}
	for _, raw := range header.Values(HeaderCLIProxyCapabilities) {
		for _, part := range strings.Split(raw, ",") {
			if strings.EqualFold(strings.TrimSpace(part), capability) {
				return true
			}
		}
	}
	return false
}

// PrefersInvocationHandshake reports whether Prefer requests an identity handshake.
func PrefersInvocationHandshake(header http.Header) bool {
	if header == nil {
		return false
	}
	for _, raw := range header.Values(HeaderPrefer) {
		for _, part := range strings.Split(raw, ",") {
			token := strings.TrimSpace(part)
			if i := strings.IndexByte(token, ';'); i >= 0 {
				token = strings.TrimSpace(token[:i])
			}
			if strings.EqualFold(token, PreferInvocationHandshake) {
				return true
			}
		}
	}
	return false
}

// ResolveExecutionIdentity selects an invocation identity using the plan precedence:
// Idempotency-Key, X-CLIProxy-Invocation-ID, X-Client-Turn-ID, metadata.turn_id,
// metadata.invocation_id, then a server-generated ID.
func ResolveExecutionIdentity(headers http.Header, originalRequest []byte) (ExecutionIdentity, error) {
	identity := ExecutionIdentity{}
	candidates := make([]string, 0, 8)

	if headers != nil {
		idem := strings.TrimSpace(headers.Get(HeaderIdempotencyKey))
		if idem != "" {
			identity.ClientIdempotencyKey = idem
			candidates = append(candidates, idem)
		}
		candidates = append(candidates,
			headers.Get(HeaderCLIProxyInvocationID),
			headers.Get(HeaderClientTurnID),
		)
	}
	for _, payload := range [][]byte{originalRequest} {
		if len(payload) == 0 {
			continue
		}
		candidates = append(candidates,
			gjson.GetBytes(payload, "metadata.turn_id").String(),
			gjson.GetBytes(payload, "metadata.invocation_id").String(),
		)
		userID := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
		if strings.HasPrefix(userID, "{") {
			candidates = append(candidates,
				gjson.Get(userID, "turn_id").String(),
				gjson.Get(userID, "invocation_id").String(),
			)
		}
	}

	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if !ValidInvocationID(candidate) {
			continue
		}
		identity.InvocationID = candidate
		identity.ServerIssued = false
		return identity, nil
	}

	id, err := GenerateInvocationID()
	if err != nil {
		return ExecutionIdentity{}, err
	}
	identity.InvocationID = id
	identity.ServerIssued = true
	return identity, nil
}

// ApplyExecutionIdentityMetadata writes identity fields into execution metadata.
func ApplyExecutionIdentityMetadata(meta map[string]any, identity ExecutionIdentity) map[string]any {
	if meta == nil {
		meta = make(map[string]any, 4)
	}
	if identity.InvocationID != "" {
		meta[InvocationIDMetadataKey] = identity.InvocationID
	}
	if identity.ClientIdempotencyKey != "" {
		meta[ClientIdempotencyKeyMetadataKey] = identity.ClientIdempotencyKey
	}
	if identity.TenantID != "" {
		meta[TenantIDMetadataKey] = identity.TenantID
	}
	if identity.ConversationID != "" {
		meta[ConversationIDMetadataKey] = identity.ConversationID
	}
	if identity.CanonicalRequestHash != "" {
		meta[CanonicalRequestHashMetadataKey] = identity.CanonicalRequestHash
	}
	if identity.ServerIssued {
		meta[InvocationServerIssuedMetadataKey] = true
	}
	return meta
}

// EnsureInvocationHeader copies the invocation ID onto request headers so
// downstream executors observe the same identity the API boundary issued.
func EnsureInvocationHeader(headers http.Header, identity ExecutionIdentity) http.Header {
	if identity.InvocationID == "" {
		return headers
	}
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	if strings.TrimSpace(headers.Get(HeaderCLIProxyInvocationID)) == "" {
		headers.Set(HeaderCLIProxyInvocationID, identity.InvocationID)
	}
	return headers
}

// InvocationIDFromMetadata reads a previously issued invocation ID from metadata.
func InvocationIDFromMetadata(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	raw, ok := meta[InvocationIDMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return ""
	}
}

// NewInvocationHandshakeOutcome builds the non-executing handshake response body.
func NewInvocationHandshakeOutcome(identity ExecutionIdentity) ProtocolOutcome {
	return ProtocolOutcome{
		Object:       "cliproxy.invocation_handshake",
		Kind:         ProtocolOutcomeInvocationHandshake,
		InvocationID: identity.InvocationID,
		Instructions: "Resubmit the original request with X-CLIProxy-Invocation-ID set to this invocation_id.",
	}
}
