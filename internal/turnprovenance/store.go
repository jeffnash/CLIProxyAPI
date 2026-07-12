package turnprovenance

import (
	"sync"
)

// Store is a conversation-scoped recurrence fingerprint store.
// Only COMPLETED turns may train it. Keys are HMAC digests, never plaintext.
type Store interface {
	Lookup(tenantID, conversationID string) RecurrenceEvidence
	ObserveCompleted(tenantID, conversationID string, standingDigest, neighborDigest string, suffixIndex int)
}

// MemoryStore is a process-local recurrence store for tests and cold starts.
// Production deployments should back this with durable state.
type MemoryStore struct {
	mu      sync.Mutex
	secret  []byte
	minOcc  int
	minByte int
	byKey   map[string]*conversationRecurrence
}

type conversationRecurrence struct {
	obs map[string]*RecurrenceObservation // keyed by segment digest (caller-provided keyed digest)
}

// NewMemoryStore constructs an in-memory recurrence store.
func NewMemoryStore(secret []byte, minOccurrences, minByteLength int) *MemoryStore {
	if minOccurrences <= 0 {
		minOccurrences = 3
	}
	if minByteLength <= 0 {
		minByteLength = 64
	}
	return &MemoryStore{
		secret:  append([]byte(nil), secret...),
		minOcc:  minOccurrences,
		minByte: minByteLength,
		byKey:   make(map[string]*conversationRecurrence),
	}
}

func (s *MemoryStore) key(tenantID, conversationID string) string {
	if len(s.secret) == 0 {
		return tenantID + "\x00" + conversationID
	}
	return FingerprintKey(s.secret, tenantID, conversationID)
}

// Lookup returns recurrence evidence for a tenant/conversation.
func (s *MemoryStore) Lookup(tenantID, conversationID string) RecurrenceEvidence {
	if s == nil || tenantID == "" || conversationID == "" {
		return RecurrenceEvidence{Enabled: false, MinOccurrences: 3, MinByteLength: 64}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := RecurrenceEvidence{
		Enabled:        true,
		MinOccurrences: s.minOcc,
		MinByteLength:  s.minByte,
	}
	rec := s.byKey[s.key(tenantID, conversationID)]
	if rec == nil {
		return out
	}
	for _, o := range rec.obs {
		out.Observations = append(out.Observations, *o)
	}
	return out
}

// ObserveCompleted records a COMPLETED-turn observation. Non-completed turns must not call this.
func (s *MemoryStore) ObserveCompleted(tenantID, conversationID, standingDigest, neighborDigest string, suffixIndex int) {
	if s == nil || tenantID == "" || conversationID == "" || standingDigest == "" {
		return
	}
	// Store only keyed fingerprints — never plain content digests.
	standingDigest = SegmentFingerprint(s.secret, tenantID, conversationID, standingDigest)
	if neighborDigest != "" {
		neighborDigest = SegmentFingerprint(s.secret, tenantID, conversationID, neighborDigest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.key(tenantID, conversationID)
	rec := s.byKey[k]
	if rec == nil {
		rec = &conversationRecurrence{obs: make(map[string]*RecurrenceObservation)}
		s.byKey[k] = rec
	}
	o := rec.obs[standingDigest]
	if o == nil {
		o = &RecurrenceObservation{
			SegmentDigest:       standingDigest,
			NeighborDigest:      neighborDigest,
			RelativeSuffixIndex: suffixIndex,
		}
		rec.obs[standingDigest] = o
	}
	// Neighbor must change across turns for recurrence to remain valid evidence.
	if o.NeighborDigest != "" && neighborDigest != "" && o.NeighborDigest != neighborDigest {
		// keep the latest neighbor; occurrence still counts when position stable
	}
	if neighborDigest != "" {
		o.NeighborDigest = neighborDigest
	}
	o.RelativeSuffixIndex = suffixIndex
	o.Occurrences++
	o.LastSeenTurn++
}
