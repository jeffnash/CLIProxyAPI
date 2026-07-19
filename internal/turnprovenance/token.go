package turnprovenance

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	resolutionTokenVersion = 1
	defaultTokenTTL        = 15 * time.Minute
)

// ResolutionTokenPayload is the signed clarification binding.
type ResolutionTokenPayload struct {
	Version           int      `json:"v"`
	TenantFingerprint string   `json:"tf"`
	ConversationID    string   `json:"cid"`
	InvocationID      string   `json:"iid"`
	RequestDigest     string   `json:"rd"`
	CandidateIDs      []string `json:"cids"`
	CandidateDigests  []string `json:"cdigs"`
	AllowedOps        []string `json:"ops"`
	IssuedAtUnix      int64    `json:"iat"`
	ExpiresAtUnix     int64    `json:"exp"`
	Nonce             string   `json:"n"`
}

// ResolutionRequest is a client's selection among signed candidates.
type ResolutionRequest struct {
	ResolutionToken  string     `json:"resolution_token"`
	CurrentSegments  []string   `json:"current_segments"`
	StandingSegments []string   `json:"standing_segments"`
	MergeGroups      [][]string `json:"merge_groups,omitempty"`
}

// TokenSecret returns the explicit provenance secret or a purpose-separated
// key derived from the stable Composer lineage secret.
func TokenSecret() ([]byte, error) {
	v := strings.TrimSpace(os.Getenv("CLIPROXY_TURN_PROVENANCE_SECRET"))
	if v != "" {
		return []byte(v), nil
	}

	// Composer already requires a stable lineage secret for restart-safe routing.
	// Derive a purpose-separated provenance key from it so signed clarification
	// remains usable when deployments have not set a second secret explicitly.
	lineage := strings.TrimSpace(os.Getenv("CURSOR_COMPOSER_LINEAGE_SECRET"))
	if lineage == "" {
		return nil, errors.New("CLIPROXY_TURN_PROVENANCE_SECRET or CURSOR_COMPOSER_LINEAGE_SECRET is not set")
	}
	mac := hmac.New(sha256.New, []byte(lineage))
	_, _ = mac.Write([]byte("cliproxy-turn-provenance-v1"))
	return mac.Sum(nil), nil
}

// IssueResolutionToken signs a clarification token for the candidate set.
func IssueResolutionToken(secret []byte, tenantID, conversationID, invocationID, requestDigest string, candidates []Segment, now time.Time, ttl time.Duration) (string, error) {
	if len(secret) == 0 {
		return "", errors.New("resolution token secret required")
	}
	if ttl <= 0 {
		ttl = defaultTokenTTL
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload := ResolutionTokenPayload{
		Version:           resolutionTokenVersion,
		TenantFingerprint: FingerprintKey(secret, tenantID, conversationID),
		ConversationID:    conversationID,
		InvocationID:      invocationID,
		RequestDigest:     requestDigest,
		AllowedOps:        []string{"select_current", "select_standing", "merge_group"},
		IssuedAtUnix:      now.Unix(),
		ExpiresAtUnix:     now.Add(ttl).Unix(),
		Nonce:             base64.RawURLEncoding.EncodeToString(nonce),
	}
	for _, c := range candidates {
		payload.CandidateIDs = append(payload.CandidateIDs, c.ID)
		payload.CandidateDigests = append(payload.CandidateDigests, c.ContentDigest)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// VerifyResolutionToken validates signature, expiry, and binding fields.
func VerifyResolutionToken(secret []byte, token string, tenantID, conversationID, invocationID, requestDigest string, now time.Time) (*ResolutionTokenPayload, error) {
	if len(secret) == 0 {
		return nil, errors.New("resolution token secret required")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("malformed resolution token")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("token body: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("token signature: %w", err)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("resolution token signature mismatch")
	}
	var payload ResolutionTokenPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.Version != resolutionTokenVersion {
		return nil, errors.New("unsupported resolution token version")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if now.Unix() > payload.ExpiresAtUnix {
		return nil, errors.New("resolution token expired")
	}
	wantTF := FingerprintKey(secret, tenantID, conversationID)
	if payload.TenantFingerprint != wantTF || payload.ConversationID != conversationID {
		return nil, errors.New("resolution token tenant/conversation mismatch")
	}
	if payload.InvocationID != invocationID {
		return nil, errors.New("resolution token invocation mismatch")
	}
	if requestDigest != "" && payload.RequestDigest != "" && payload.RequestDigest != requestDigest {
		return nil, errors.New("resolution token request digest mismatch")
	}
	return &payload, nil
}

// ApplyResolution checks that selections are within the signed candidate set
// and form a complete, non-conflicting classification of every candidate.
func ApplyResolution(payload *ResolutionTokenPayload, req ResolutionRequest) (Decision, error) {
	if payload == nil {
		return Decision{}, errors.New("nil resolution payload")
	}
	if len(payload.CandidateIDs) == 0 {
		return Decision{}, errors.New("resolution token has empty candidate set")
	}
	allowed := map[string]bool{}
	for _, id := range payload.CandidateIDs {
		if strings.TrimSpace(id) == "" {
			return Decision{}, errors.New("resolution token contains empty candidate id")
		}
		allowed[id] = true
	}

	current, err := normalizeSegmentIDs(req.CurrentSegments, "current")
	if err != nil {
		return Decision{}, err
	}
	standing, err := normalizeSegmentIDs(req.StandingSegments, "standing")
	if err != nil {
		return Decision{}, err
	}
	if len(current) == 0 {
		return Decision{}, errors.New("resolution requires at least one current segment")
	}

	classified := map[string]string{}
	mark := func(ids []string, label string) error {
		for _, id := range ids {
			if !allowed[id] {
				return fmt.Errorf("segment %q not in signed candidate set", id)
			}
			if prev, ok := classified[id]; ok && prev != label {
				return fmt.Errorf("segment %q cannot be both %s and %s", id, prev, label)
			}
			classified[id] = label
		}
		return nil
	}
	if err := mark(current, "current"); err != nil {
		return Decision{}, err
	}
	if err := mark(standing, "standing"); err != nil {
		return Decision{}, err
	}

	mergeGroups := make([][]string, 0, len(req.MergeGroups))
	for i, g := range req.MergeGroups {
		group, err := normalizeSegmentIDs(g, fmt.Sprintf("merge_groups[%d]", i))
		if err != nil {
			return Decision{}, err
		}
		if len(group) == 0 {
			return Decision{}, fmt.Errorf("merge_groups[%d] is empty", i)
		}
		// Merge members are current intent; allow overlap with current, reject standing.
		if err := mark(group, "current"); err != nil {
			return Decision{}, err
		}
		inCurrent := map[string]bool{}
		for _, id := range current {
			inCurrent[id] = true
		}
		for _, id := range group {
			if !inCurrent[id] {
				current = append(current, id)
				inCurrent[id] = true
			}
		}
		mergeGroups = append(mergeGroups, group)
	}

	for _, id := range payload.CandidateIDs {
		if _, ok := classified[id]; !ok {
			return Decision{}, fmt.Errorf("candidate %q omitted from resolution selection", id)
		}
	}

	return Decision{
		Kind:             DecisionResolvedExplicit,
		CurrentSegments:  current,
		StandingSegments: standing,
		MergeGroups:      mergeGroups,
		ReasonCode:       ReasonExplicitMetadata,
		Evidence: []Evidence{{
			Class: EvidenceExplicit,
			Code:  "signed_resolution",
		}},
	}, nil
}

func normalizeSegmentIDs(ids []string, label string) ([]string, error) {
	out := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("%s contains an empty segment id", label)
		}
		if seen[id] {
			return nil, fmt.Errorf("%s contains duplicate segment %q", label, id)
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}
